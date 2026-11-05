package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type sourceVersion struct {
	ID          int64
	Filename    string
	Revision    int64
	ContentHash string
	StatusKnown bool
}
type compilationSnapshot struct {
	ID       string
	Name     string
	Filename string
	URL      string
	Include  string
	Exclude  string
	ItemsMax int
	Sources  []sourceVersion
}

// A short read transaction captures config and source revisions consistently;
// no SQLite transaction is held while parsing or writing feed files.
func readCompilationSnapshot(id string) (compilationSnapshot, error) {
	s := compilationSnapshot{ID: id, ItemsMax: k.Int("items.max")}
	tx, err := database.Begin()
	if err != nil {
		return s, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT COALESCE(name,''),COALESCE(filename,''),COALESCE(url,''),
 COALESCE(filter_inc,''),COALESCE(filter_exc,'') FROM compilation WHERE id=?`, id).
		Scan(&s.Name, &s.Filename, &s.URL, &s.Include, &s.Exclude)
	if err != nil {
		return s, err
	}
	rows, err := tx.Query(`SELECT DISTINCT c.feed_id,COALESCE(f.filename,''),COALESCE(r.revision,0),COALESCE(r.content_hash,''),status.id IS NOT NULL
 FROM compilation_content c LEFT JOIN feed f ON f.id=c.feed_id
 LEFT JOIN feed_status status ON status.id=c.feed_id
 LEFT JOIN feed_revision r ON r.feed_id=c.feed_id
 WHERE c.id=? ORDER BY c.feed_id`, id)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var source sourceVersion
		if err := rows.Scan(&source.ID, &source.Filename, &source.Revision, &source.ContentHash, &source.StatusKnown); err != nil {
			rows.Close()
			return s, err
		}
		s.Sources = append(s.Sources, source)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return s, err
	}
	rows.Close()
	return s, tx.Commit()
}
func (s compilationSnapshot) hash() string {
	b, _ := json.Marshal(s)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func compilations_needing_update() []string {
	rows, err := database.Query("SELECT id FROM compilation WHERE filename IS NOT NULL AND filename<>'' ORDER BY id")
	if err != nil {
		logSnapshotError("queue", err)
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		logSnapshotError("queue", err)
		return nil
	}
	var queue []string
	for _, id := range ids {
		snapshot, err := readCompilationSnapshot(id)
		if err != nil {
			logSnapshotError(id, err)
			continue
		}
		var previous string
		err = database.QueryRow("SELECT snapshot_hash FROM compilation_build_state WHERE id=?", id).Scan(&previous)
		if err != nil && err != sql.ErrNoRows {
			logSnapshotError(id, err)
			continue
		}
		_, outputErr := os.Stat(snapshot.Filename)
		if err == sql.ErrNoRows || previous != snapshot.hash() || outputErr != nil {
			queue = append(queue, id)
		}
	}
	return queue
}

// Store precisely the snapshot that produced the file, never a new snapshot
// read after compilation. Concurrent updates therefore remain queued.
func markSnapshotCompiled(snapshot compilationSnapshot) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO compilation_build_state(id,snapshot_hash) VALUES (?,?)
 ON CONFLICT(id) DO UPDATE SET snapshot_hash=excluded.snapshot_hash`, snapshot.ID, snapshot.hash()); err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE compilation_status SET updated=? WHERE id=?", time.Now().Unix(), snapshot.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec("INSERT INTO compilation_status(id,updated,published) VALUES (?,?,0)", snapshot.ID, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
