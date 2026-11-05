package lib

import (
	"database/sql"
	"encoding/hex"
	"fmt"
)

// EnsureCompilationState adds only bookkeeping tables. Existing user tables,
// feed files and old timestamp columns remain intact. Run before either worker.
func EnsureCompilationState(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS feed_revision (
 feed_id INTEGER PRIMARY KEY, revision INTEGER NOT NULL, content_hash TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS compilation_build_state (
 id TEXT PRIMARY KEY, snapshot_hash TEXT NOT NULL)`,
		`INSERT OR IGNORE INTO feed_revision(feed_id,revision,content_hash)
 SELECT id,1,'' FROM feed_status WHERE updated>0`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("initialize compilation state: %w", err)
		}
	}
	return tx.Commit()
}

// RecordFeedRefresh commits the accepted content version and all feed status
// updates together. Hash comparison also recovers from an earlier failed DB
// commit after the file had already been atomically replaced.
func RecordFeedRefresh(db *sql.DB, id int64, path, hash string, now int64) (bool, error) {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 {
		return false, fmt.Errorf("invalid SHA-256 for feed %d", id)
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRow("SELECT content_hash FROM feed_revision WHERE feed_id=?", id).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	changed := err == sql.ErrNoRows || previous != hash
	if _, err := tx.Exec(`INSERT INTO feed_revision(feed_id,revision,content_hash) VALUES (?,1,?)
 ON CONFLICT(feed_id) DO UPDATE SET
 revision=feed_revision.revision + CASE WHEN feed_revision.content_hash<>excluded.content_hash THEN 1 ELSE 0 END,
 content_hash=excluded.content_hash`, id, hash); err != nil {
		return false, err
	}
	var result sql.Result
	if changed {
		result, err = tx.Exec("UPDATE feed_status SET refreshed=?, updated=? WHERE id=?", now, now, id)
	} else {
		result, err = tx.Exec("UPDATE feed_status SET refreshed=? WHERE id=?", now, id)
	}
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return false, fmt.Errorf("feed %d status row missing or invalid", id)
	}
	result, err = tx.Exec("UPDATE feed SET filename=? WHERE id=?", path, id)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return false, fmt.Errorf("feed %d row missing or invalid", id)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}
