package lib

import (
	"database/sql"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingSQLiteDatabase(t *testing.T) {
	fixture, err := ioutil.ReadFile("testdata/existing.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "existing.sqlite")
	if err := ioutil.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow("SELECT name FROM compilation WHERE id = ?", "existing").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Existing feed" {
		t.Fatalf("name %q", name)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	result, err := tx.Exec("INSERT INTO feed (uschema,urn,created) VALUES (?,?,?)", "https", "example.org/new", 456)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if id != 2 {
		tx.Rollback()
		t.Fatalf("id %d", id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT urn FROM feed WHERE id=?", id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "example.org/new" {
		t.Fatal(name)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %q %v", integrity, err)
	}
}

func TestBusyTimeoutEveryConnection(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		want   int
	}{
		{"", 5000}, {"?_busy_timeout=25", 25}, {"?_timeout=30", 30},
		{"?_busy_timeout=20&_timeout=35", 35}, {"?_pragma=busy_timeout(40)", 40},
	} {
		db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.sqlite")+tc.suffix)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(2)
		// Hold one physical connection while opening a second one.
		conn1, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn2, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, conn := range []*sql.Conn{conn1, conn2} {
			var timeout int
			if err := conn.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&timeout); err != nil {
				t.Fatal(err)
			}
			if timeout != tc.want {
				t.Fatalf("DSN %q timeout %d want %d", tc.suffix, timeout, tc.want)
			}
		}
		conn1.Close()
		conn2.Close()
		db.Close()
	}
}

func TestDSNValidation(t *testing.T) {
	for _, dsn := range []string{"db?_busy_timeout=-1", "db?_timeout=5;DROP TABLE feed", "db?bad=%xx"} {
		if _, err := sqliteDSN(dsn); err == nil {
			t.Fatalf("accepted %q", dsn)
		}
	}
	dsn, err := sqliteDSN("file:db?mode=ro&cache=shared")
	if err != nil || !strings.Contains(dsn, "mode=ro") || !strings.Contains(dsn, "cache=shared") {
		t.Fatalf("DSN %s %v", dsn, err)
	}
}

func TestMissingDirectoryFailsOnPing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "db.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Fatal("nonexistent parent directory accepted")
	}
}
