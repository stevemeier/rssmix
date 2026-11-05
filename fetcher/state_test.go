package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/stevemeier/rssmix/lib"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustStateStore(t *testing.T, db *sql.DB) *stateStore {
	t.Helper()
	s, err := openStateStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func testStateStore(t *testing.T) *stateStore {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return mustStateStore(t, db)
}
func attachState(t *testing.T, tr *hostTransport, db *sql.DB) {
	t.Helper()
	if err := tr.loadState(mustStateStore(t, db)); err != nil {
		t.Fatal(err)
	}
}
func writeLegacyJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func legacyFeeds(t *testing.T, s *stateStore, addresses ...string) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS feed(uschema TEXT,urn TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		scheme, urn, ok := strings.Cut(address, "://")
		if !ok {
			t.Fatal(address)
		}
		if _, err := s.db.Exec(`INSERT INTO feed(uschema,urn) VALUES (?,?)`, scheme, urn); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyImportCommitsOnceAndPreservesSQLite(t *testing.T) {
	s := testStateStore(t)
	s.db.SetMaxOpenConns(1)
	dir := t.TempDir()
	addresses := []string{"https://example.org/a", "https://example.org/b"}
	legacyFeeds(t, s, addresses...)
	now := time.Now().UTC().Truncate(time.Second)
	h := hostState{Next: now.Add(time.Minute), Until: now.Add(time.Hour), Failures: 6, ResumeURL: addresses[1]}
	hostPath := filepath.Join(dir, "rssmix-fetcher-state.json")
	writeLegacyJSON(t, hostPath, diskState{Version: 1, Hosts: map[string]*hostState{"example.org": &h}})
	want := feedValidators{URL: addresses[0], ETag: `W/"old"`, LastModified: "Sun, 25 Oct 2026 11:28:45 GMT", SHA256: sha256sum("cached"), ValidationVersion: 1}
	validatorPath := filepath.Join(dir, lib.Subdirs(sha256sum(want.URL), 2)) + ".http.json"
	writeLegacyJSON(t, validatorPath, want)
	// An existing database entry must win, even during the first import.
	existing := feedValidators{URL: addresses[1], ETag: `"sqlite"`, SHA256: sha256sum("database")}
	if err := s.saveValidators(existing); err != nil {
		t.Fatal(err)
	}
	writeLegacyJSON(t, filepath.Join(dir, lib.Subdirs(sha256sum(existing.URL), 2))+".http.json", feedValidators{URL: existing.URL, ETag: `"obsolete"`})
	beforeHost, _ := os.ReadFile(hostPath)
	beforeValidator, _ := os.ReadFile(validatorPath)
	if err := s.importLegacy(dir, 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadValidators(want.URL)
	if err != nil || got != want {
		t.Fatalf("validators: %+v %v", got, err)
	}
	got, err = s.loadValidators(existing.URL)
	if err != nil || got != existing {
		t.Fatalf("DB overwritten: %+v %v", got, err)
	}
	hosts, err := s.loadHosts()
	if err != nil || *hosts["example.org"] != h {
		t.Fatalf("hosts: %+v %v", hosts, err)
	}
	afterHost, _ := os.ReadFile(hostPath)
	afterValidator, _ := os.ReadFile(validatorPath)
	if !bytes.Equal(beforeHost, afterHost) || !bytes.Equal(beforeValidator, afterValidator) {
		t.Fatal("legacy files changed")
	}
	// A later reset must survive stale, or even malformed, old JSON files.
	if err := s.saveHost("example.org", &hostState{}); err != nil {
		t.Fatal(err)
	}
	want.ETag = `"new"`
	if err := s.saveValidators(want); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(hostPath, []byte("broken"), 0600)
	os.WriteFile(validatorPath, []byte("broken"), 0600)
	if err := s.importLegacy(dir, 2); err != nil {
		t.Fatal(err)
	}
	hosts, err = s.loadHosts()
	if err != nil || hosts["example.org"].Failures != 0 {
		t.Fatalf("old backoff resurrected: %v", err)
	}
	got, err = s.loadValidators(want.URL)
	if err != nil || got != want {
		t.Fatalf("old validators resurrected: %+v %v", got, err)
	}
}

func TestLegacyImportRollbackAndRetry(t *testing.T) {
	s := testStateStore(t)
	dir := t.TempDir()
	address := "https://example.org/feed"
	legacyFeeds(t, s, address)
	writeLegacyJSON(t, filepath.Join(dir, "rssmix-fetcher-state.json"), diskState{Version: 1, Hosts: map[string]*hostState{"example.org": {Failures: 3}}})
	writeLegacyJSON(t, filepath.Join(dir, sha256sum(address))+".http.json", feedValidators{URL: address, ETag: `"old"`})
	_, err := s.db.Exec(`CREATE TRIGGER fail_import BEFORE INSERT ON fetcher_http_state BEGIN SELECT RAISE(ABORT,'test failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy(dir, 0); err == nil {
		t.Fatal("failed migration reported success")
	}
	for _, table := range []string{"fetcher_host_state", "fetcher_http_state", "fetcher_state_migration"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial migration in %s: %d %v", table, n, err)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_import`); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy(dir, 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.loadHosts()
	if err != nil || h["example.org"].Failures != 3 {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestMissingOrInvalidLegacyValidators(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong URL"} {
		t.Run(kind, func(t *testing.T) {
			s := testStateStore(t)
			dir := t.TempDir()
			address := "https://example.org/feed"
			legacyFeeds(t, s, address)
			path := filepath.Join(dir, sha256sum(address)) + ".http.json"
			switch kind {
			case "malformed":
				os.WriteFile(path, []byte("broken"), 0600)
			case "wrong URL":
				writeLegacyJSON(t, path, feedValidators{URL: address + "-other", ETag: `"wrong"`})
			}
			if err := s.importLegacy(dir, 0); err != nil {
				t.Fatal(err)
			}
			v, err := s.loadValidators(address)
			if err != nil || v.ETag != "" {
				t.Fatalf("unsafe validators: %+v %v", v, err)
			}
			// Also mark an absent legacy file as migrated: no import on later restarts.
			writeLegacyJSON(t, path, feedValidators{URL: address, ETag: `"late"`})
			if err := s.importLegacy(dir, 0); err != nil {
				t.Fatal(err)
			}
			v, err = s.loadValidators(address)
			if err != nil || v.ETag != "" {
				t.Fatalf("late legacy import: %+v %v", v, err)
			}
		})
	}
}

func TestSQLiteOnlyRestartPreservesSchedulingAndConditionalGET(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s := mustStateStore(t, db)
	dir := t.TempDir()
	path := filepath.Join(dir, "feed")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("ETag", `"persisted"`)
			fmt.Fprint(w, testRSSBody("Persistent"))
		} else {
			if r.Header.Get("If-None-Match") != `"persisted"` {
				t.Error("restart lost conditional headers")
			}
			w.WriteHeader(304)
		}
	}))
	defer server.Close()
	if _, err := fetchFeed(s, server.Client(), server.URL, path); err != nil {
		t.Fatal(err)
	}
	// Keep sub-second precision and timestamps beyond UnixNano's range.
	now := time.Date(2026, 11, 5, 12, 0, 0, 123456789, time.UTC)
	h := hostState{Next: now.Add(time.Minute), Until: now.Add(time.Hour), Failures: 4, ResumeURL: server.URL}
	if err := s.saveHost(addressHost(server.URL), &h); err != nil {
		t.Fatal(err)
	}
	far := hostState{Until: time.Date(2318, 1, 1, 0, 0, 0, 0, time.UTC), Failures: 1}
	if err := s.saveHost("far.example", &far); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A real close/reopen; there are no JSON files to fall back to.
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s = mustStateStore(t, db)
	tr := newHostTransport(server.Client().Transport)
	tr.now = func() time.Time { return now }
	if err := tr.loadState(s); err != nil {
		t.Fatal(err)
	}
	restored := tr.states[addressHost(server.URL)]
	if *restored != h || *tr.states["far.example"] != far {
		t.Fatal("host state changed on restart")
	}
	if err := send(t, tr, server.URL); err == nil || calls != 1 {
		t.Fatal("restart lost cooldown")
	}
	// Once cooldown expires, the previously stored Next must still pace requests.
	if err := tr.recovered(addressHost(server.URL)); err != nil {
		t.Fatal(err)
	}
	waited := time.Duration(0)
	tr.wait = func(_ context.Context, d time.Duration) error { waited += d; now = now.Add(d); return nil }
	result, err := fetchFeed(s, &http.Client{Transport: tr}, server.URL, path)
	if err != nil || result.changed || waited != time.Minute {
		t.Fatalf("restart: %+v waited %s err %v", result, waited, err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("JSON files created: %v", files)
	}
}

func TestDatabaseFailurePreventsRequests(t *testing.T) {
	for _, kind := range []string{"validators read", "schedule write"} {
		t.Run(kind, func(t *testing.T) {
			s := testStateStore(t)
			calls := 0
			tr := newHostTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("unexpected network request")
			}))
			if err := tr.loadState(s); err != nil {
				t.Fatal(err)
			}
			if kind == "validators read" {
				s.db.Close()
			} else {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_schedule BEFORE INSERT ON fetcher_host_state BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fetchFeed(s, &http.Client{Transport: tr}, "https://example.org/feed", filepath.Join(t.TempDir(), "feed")); err == nil {
				t.Fatal("DB failure ignored")
			}
			if calls != 0 {
				t.Fatal("network request without persisted state")
			}
		})
	}
}

func TestImportedValidatorsUsedWithExistingCache(t *testing.T) {
	s := testStateStore(t)
	dir := t.TempDir()
	lm := "Sun, 25 Oct 2026 11:28:45 GMT"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"imported"` || r.Header.Get("If-Modified-Since") != lm {
			t.Error("migration lost validators")
		}
		w.WriteHeader(304)
	}))
	defer server.Close()
	legacyFeeds(t, s, server.URL)
	path := filepath.Join(dir, sha256sum(server.URL))
	body := testRSSBody("Imported")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	v := feedValidators{URL: server.URL, ETag: `W/"imported"`, LastModified: lm, SHA256: sha256sum(body)}
	writeLegacyJSON(t, path+".http.json", v)
	if err := s.importLegacy(dir, 0); err != nil {
		t.Fatal(err)
	}
	// Retired JSON may be removed; it is not needed for requests any more.
	os.Remove(path + ".http.json")
	result, err := fetchFeed(s, server.Client(), server.URL, path)
	if err != nil || result.changed || result.hash != v.SHA256 {
		t.Fatalf("imported 304: %+v %v", result, err)
	}
	v, err = s.loadValidators(server.URL)
	if err != nil || v.ValidationVersion != feedValidationVersion {
		t.Fatalf("marker not persisted in SQLite: %+v %v", v, err)
	}
}
