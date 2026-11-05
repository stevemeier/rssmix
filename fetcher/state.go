package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/stevemeier/rssmix/lib"
)

// One fetcher process owns scheduling for this database. SQLite persists each
// complete host record atomically; the transport serializes in-process changes.
type stateStore struct{ db *sql.DB }

func openStateStore(db *sql.DB) (*stateStore, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS fetcher_host_state (
   host TEXT PRIMARY KEY NOT NULL,
   next_request TEXT NOT NULL DEFAULT '',
   paused_until TEXT NOT NULL DEFAULT '',
   failures INTEGER NOT NULL DEFAULT 0 CHECK(failures BETWEEN 0 AND 1000000),
   resume_url TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS fetcher_http_state (
   url TEXT PRIMARY KEY NOT NULL,
   etag TEXT NOT NULL DEFAULT '',
   last_modified TEXT NOT NULL DEFAULT '',
   content_hash TEXT NOT NULL DEFAULT '',
   validation_version INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS fetcher_state_migration (
   name TEXT PRIMARY KEY NOT NULL, completed_at TEXT NOT NULL)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return nil, fmt.Errorf("initialize fetcher state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &stateStore{db: db}, nil
}

func stateTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func parseStateTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

const insertHostState = `INSERT INTO fetcher_host_state(host,next_request,paused_until,failures,resume_url)
 VALUES (?,?,?,?,?)`
const insertHTTPState = `INSERT INTO fetcher_http_state(url,etag,last_modified,content_hash,validation_version)
 VALUES (?,?,?,?,?)`

func (s *stateStore) saveHost(host string, h *hostState) error {
	_, err := s.db.Exec(insertHostState+` ON CONFLICT(host) DO UPDATE SET
 next_request=excluded.next_request, paused_until=excluded.paused_until,
 failures=excluded.failures, resume_url=excluded.resume_url`,
		host, stateTime(h.Next), stateTime(h.Until), h.Failures, h.ResumeURL)
	return err
}
func (s *stateStore) loadHosts() (map[string]*hostState, error) {
	rows, err := s.db.Query(`SELECT host,next_request,paused_until,failures,resume_url FROM fetcher_host_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := make(map[string]*hostState)
	for rows.Next() {
		var host, next, until string
		h := &hostState{}
		if err := rows.Scan(&host, &next, &until, &h.Failures, &h.ResumeURL); err != nil {
			return nil, err
		}
		if host == "" || h.Failures < 0 || h.Failures > 1000000 {
			return nil, fmt.Errorf("invalid persisted host state for %q", host)
		}
		if h.Next, err = parseStateTime(next); err != nil {
			return nil, fmt.Errorf("host %s next request: %w", host, err)
		}
		if h.Until, err = parseStateTime(until); err != nil {
			return nil, fmt.Errorf("host %s cooldown: %w", host, err)
		}
		hosts[host] = h
	}
	return hosts, rows.Err()
}
func (s *stateStore) loadValidators(address string) (feedValidators, error) {
	v := feedValidators{URL: address}
	err := s.db.QueryRow(`SELECT etag,last_modified,content_hash,validation_version FROM fetcher_http_state WHERE url=?`, address).
		Scan(&v.ETag, &v.LastModified, &v.SHA256, &v.ValidationVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	return v, err
}
func (s *stateStore) saveValidators(v feedValidators) error {
	_, err := s.db.Exec(insertHTTPState+` ON CONFLICT(url) DO UPDATE SET
 etag=excluded.etag,last_modified=excluded.last_modified,content_hash=excluded.content_hash,
 validation_version=excluded.validation_version`, v.URL, v.ETag, v.LastModified, v.SHA256, v.ValidationVersion)
	return err
}

// Import the previous workdir once per database, including inactive sources.
// The marker and every imported record commit together. Existing SQLite rows
// win over legacy files. Never delete the originals or read them after commit.
func (s *stateStore) importLegacy(workdir string, subdirs int) error {
	const migration = "json-state-v1"
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM fetcher_state_migration WHERE name=?`, migration).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	hosts := make(map[string]*hostState)
	path := filepath.Join(workdir, "rssmix-fetcher-state.json")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var disk diskState
		if err := json.Unmarshal(b, &disk); err != nil {
			return fmt.Errorf("invalid backoff state %s: %w", path, err)
		}
		if disk.Version != 1 || disk.Hosts == nil {
			return fmt.Errorf("unsupported backoff state: %s", path)
		}
		for host, h := range disk.Hosts {
			if host == "" || h == nil || h.Failures < 0 || h.Failures > 1000000 {
				return fmt.Errorf("invalid host entry in %s", path)
			}
		}
		hosts = disk.Hosts
	}

	// Close rows before starting a transaction (also works with a single DB connection).
	var addresses []string
	rows, err := s.db.Query(`SELECT uschema,urn FROM feed`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var scheme, urn string
		if err := rows.Scan(&scheme, &urn); err != nil {
			rows.Close()
			return err
		}
		addresses = append(addresses, scheme+"://"+urn)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var validators []feedValidators
	for _, address := range addresses {
		path := filepath.Join(workdir, lib.Subdirs(sha256sum(address), subdirs)) + ".http.json"
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var v feedValidators
		if err := json.Unmarshal(b, &v); err != nil || v.URL != address {
			// Old fetchers ignored broken validators. A full validated GET is safe;
			// unlike cooldowns, validators may be discarded without a request burst.
			log.Printf("Ignoring invalid legacy HTTP validators: %s", path)
			continue
		}
		validators = append(validators, v)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Recheck under the transaction in case initialization was repeated.
	err = tx.QueryRow(`SELECT 1 FROM fetcher_state_migration WHERE name=?`, migration).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for host, h := range hosts {
		if _, err := tx.Exec(insertHostState+` ON CONFLICT(host) DO NOTHING`, host, stateTime(h.Next), stateTime(h.Until), h.Failures, h.ResumeURL); err != nil {
			return err
		}
	}
	for _, v := range validators {
		if _, err := tx.Exec(insertHTTPState+` ON CONFLICT(url) DO NOTHING`, v.URL, v.ETag, v.LastModified, v.SHA256, v.ValidationVersion); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO fetcher_state_migration(name,completed_at) VALUES (?,?)`, migration, stateTime(time.Now())); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("Migrated legacy fetcher state to SQLite (%d host records, %d HTTP validator records considered)", len(hosts), len(validators))
	return nil
}
