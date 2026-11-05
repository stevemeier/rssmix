package lib

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func revisionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := os.ReadFile("../sql/sqlite3.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO feed(id,uschema,urn,filename) VALUES(1,'https','example.org/feed','old')")
	db.Exec("INSERT INTO feed_status VALUES(1,10,10,1)")
	if err := EnsureCompilationState(db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestRevisionMigrationIsIdempotent(t *testing.T) {
	db := revisionDB(t)
	if err := EnsureCompilationState(db); err != nil {
		t.Fatal(err)
	}
	var rev int
	var hash string
	if err := db.QueryRow("SELECT revision,content_hash FROM feed_revision WHERE feed_id=1").Scan(&rev, &hash); err != nil {
		t.Fatal(err)
	}
	if rev != 1 || hash != "" {
		t.Fatalf("migration %d %q", rev, hash)
	}
}
func TestContentRevisionAndRollbackRecovery(t *testing.T) {
	db := revisionDB(t)
	h1 := fmt.Sprintf("%x", sha256.Sum256([]byte("v1")))
	h2 := fmt.Sprintf("%x", sha256.Sum256([]byte("v2")))
	changed, err := RecordFeedRefresh(db, 1, "new", h1, 100)
	if err != nil || !changed {
		t.Fatalf("first %v %v", changed, err)
	}
	var rev int
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&rev)
	if changed, err := RecordFeedRefresh(db, 1, "new", h1, 100); err != nil || changed {
		t.Fatal("identical content changed revision")
	}
	var current int
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&current)
	if current != rev {
		t.Fatal("revision advanced")
	}
	db.Exec("CREATE TRIGGER fail_status BEFORE UPDATE ON feed_status BEGIN SELECT RAISE(ABORT,'test failure'); END")
	if _, err := RecordFeedRefresh(db, 1, "changed", h2, 100); err == nil {
		t.Fatal("expected failed commit")
	}
	var hash, path string
	db.QueryRow("SELECT revision,content_hash FROM feed_revision WHERE feed_id=1").Scan(&current, &hash)
	db.QueryRow("SELECT filename FROM feed WHERE id=1").Scan(&path)
	if current != rev || hash != h1 || path != "new" {
		t.Fatal("partial DB update escaped rollback")
	}
	db.Exec("DROP TRIGGER fail_status")
	if changed, err := RecordFeedRefresh(db, 1, "changed", h2, 100); err != nil || !changed {
		t.Fatalf("recovery %v %v", changed, err)
	}
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&current)
	if current != rev+1 {
		t.Fatal("same-second update lost")
	}
}
func TestMissingStatusRollsBackVersion(t *testing.T) {
	db := revisionDB(t)
	db.Exec("DELETE FROM feed_status")
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("v1")))
	if _, err := RecordFeedRefresh(db, 1, "new", hash, 100); err == nil {
		t.Fatal("missing status accepted")
	}
	var rev int
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&rev)
	if rev != 1 {
		t.Fatal("failed status changed revision")
	}
}
