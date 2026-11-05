package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf"
	"github.com/mmcdole/gofeed"
	"github.com/stevemeier/rssmix/lib"
)

func compilerFixture(t *testing.T) (string, string) {
	t.Helper()
	oldDB, oldK := database, k
	db, err := sqlx.Connect("sqlite3", filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	database = db
	k = koanf.New(".")
	t.Cleanup(func() { db.Close(); database = oldDB; k = oldK })
	schema, err := os.ReadFile("../sql/sqlite3.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "feed.xml")
	out := filepath.Join(dir, "out.xml")
	if _, err := db.Exec("INSERT INTO compilation(id,name,filename,url) VALUES ('test','Test',?,'https://example.org/out.xml')", out); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO feed(id,uschema,urn,filename) VALUES (1,'https','example.org/feed',?)", source); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"INSERT INTO feed_status VALUES (1,100,100,1)", "INSERT INTO compilation_content VALUES ('test',1)", "INSERT INTO compilation_status VALUES ('test',50,0)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := lib.EnsureCompilationState(db.DB); err != nil {
		t.Fatal(err)
	}
	return source, out
}
func rss(title string) []byte {
	return []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title><link>https://example.org</link><description>Test</description><item><title>` + title + `</title><description>Content</description><guid>` + title + `</guid></item></channel></rss>`)
}
func putSource(t *testing.T, path, title string) {
	t.Helper()
	data := rss(title)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.RecordFeedRefresh(database.DB, 1, path, fmt.Sprintf("%x", sha256.Sum256(data)), 100); err != nil {
		t.Fatal(err)
	}
}
func mustCompile(t *testing.T) {
	t.Helper()
	if ok, err := update_compilation("test"); err != nil || !ok {
		t.Fatalf("compile: %v %v", ok, err)
	}
}
func assertQueued(t *testing.T, want bool) {
	t.Helper()
	queue := compilations_needing_update()
	if (len(queue) > 0) != want {
		t.Fatalf("queue=%v want dirty=%t", queue, want)
	}
}
func TestItemWithoutLinkDoesNotPanic(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "No link")
	mustCompile(t)
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parsed, err := gofeed.NewParser().Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].Title != "No link" || parsed.Items[0].Link != "" {
		t.Fatalf("items %+v", parsed.Items)
	}
	assertQueued(t, false)
}
func TestBadActiveSourcePreservesOutputAndState(t *testing.T) {
	for _, kind := range []string{"missing", "null filename", "invalid XML", "HTML", "changed cache", "missing status", "missing catalogue"} {
		t.Run(kind, func(t *testing.T) {
			path, out := compilerFixture(t)
			putSource(t, path, "Before")
			mustCompile(t)
			original, _ := os.ReadFile(out)
			switch kind {
			case "missing":
				os.Remove(path)
			case "null filename":
				database.Exec("UPDATE feed SET filename=NULL WHERE id=1")
			case "invalid XML":
				putSource(t, path, "invalid")
				data := []byte("<rss><broken>")
				os.WriteFile(path, data, 0600)
				lib.RecordFeedRefresh(database.DB, 1, path, fmt.Sprintf("%x", sha256.Sum256(data)), 100)
			case "HTML":
				data := []byte("<html><body>Error</body></html>")
				os.WriteFile(path, data, 0600)
				lib.RecordFeedRefresh(database.DB, 1, path, fmt.Sprintf("%x", sha256.Sum256(data)), 100)
			case "changed cache":
				os.WriteFile(path, rss("Unrecorded"), 0600)
			case "missing status":
				database.Exec("DELETE FROM feed_status WHERE id=1")
			case "missing catalogue":
				database.Exec("DELETE FROM feed WHERE id=1")
			}
			// Force a retry, also for an externally missing file with unchanged revision.
			database.Exec("DELETE FROM compilation_build_state WHERE id='test'")
			if ok, err := update_compilation("test"); err == nil || ok {
				t.Fatalf("bad source accepted: %v %v", ok, err)
			}
			after, _ := os.ReadFile(out)
			if string(original) != string(after) {
				t.Fatal("previous output overwritten")
			}
			var count int
			database.QueryRow("SELECT COUNT(*) FROM compilation_build_state").Scan(&count)
			if count != 0 {
				t.Fatal("failed compilation marked successful")
			}
			assertQueued(t, true)
		})
	}
}
func TestConcurrentAndSameSecondUpdatesRemainQueued(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "Before")
	snapshot, err := readCompilationSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCompilation(snapshot); err != nil {
		t.Fatal(err)
	}
	// New revision with the exact same timestamp, after reading old content.
	putSource(t, path, "After")
	if err := markSnapshotCompiled(snapshot); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(out)
	if !strings.Contains(string(old), "Before") {
		t.Fatal("fixture incorrect")
	}
	assertQueued(t, true)
	mustCompile(t)
	assertQueued(t, false)
	current, _ := os.ReadFile(out)
	if !strings.Contains(string(current), "After") {
		t.Fatal("new content lost")
	}
}
func TestUpdateBetweenSnapshotAndReadIsRetried(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "Before")
	mustCompile(t)
	old, _ := os.ReadFile(out)
	snapshot, err := readCompilationSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	putSource(t, path, "After")
	if err := writeCompilation(snapshot); err == nil {
		t.Fatal("changed snapshot accepted")
	}
	after, _ := os.ReadFile(out)
	if string(after) != string(old) {
		t.Fatal("output overwritten")
	}
	assertQueued(t, true)
}
func TestPausedSourcesKeepCachedContent(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "First cached item")
	pausedPath := filepath.Join(t.TempDir(), "paused.xml")
	pausedData := rss("Paused cached item")
	if err := os.WriteFile(pausedPath, pausedData, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO feed(id,uschema,urn,filename) VALUES (2,'https','paused.example/feed',?)", pausedPath); err != nil {
		t.Fatal(err)
	}
	database.Exec("INSERT INTO feed_status VALUES (2,100,100,0)")
	database.Exec("INSERT INTO compilation_content VALUES ('test',2)")
	if _, err := lib.RecordFeedRefresh(database.DB, 2, pausedPath, fmt.Sprintf("%x", sha256.Sum256(pausedData)), 100); err != nil {
		t.Fatal(err)
	}
	mustCompile(t)
	assertQueued(t, false)
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "First cached item") || !strings.Contains(string(body), "Paused cached item") {
		t.Fatal("paused source missing from compilation")
	}
	// Pausing all downloads must not remove any source or change the snapshot.
	if _, err := database.Exec("UPDATE feed_status SET active=0"); err != nil {
		t.Fatal(err)
	}
	assertQueued(t, false)
	mustCompile(t)
	assertQueued(t, false)
	body, _ = os.ReadFile(out)
	if !strings.Contains(string(body), "First cached item") || !strings.Contains(string(body), "Paused cached item") {
		t.Fatal("all-paused compilation lost cached content")
	}
	// Only removing the association removes a source from the output.
	if _, err := database.Exec("DELETE FROM compilation_content WHERE id='test' AND feed_id=2"); err != nil {
		t.Fatal(err)
	}
	assertQueued(t, true)
	mustCompile(t)
	body, _ = os.ReadFile(out)
	if strings.Contains(string(body), "Paused cached item") || !strings.Contains(string(body), "First cached item") {
		t.Fatal("membership change not reflected")
	}
	// Even a paused source is still required; do not silently omit broken cache.
	before := append([]byte(nil), body...)
	os.Remove(path)
	if ok, err := update_compilation("test"); err == nil || ok {
		t.Fatal("missing paused cache accepted")
	}
	body, _ = os.ReadFile(out)
	if string(body) != string(before) {
		t.Fatal("missing paused cache overwrote last output")
	}
}

func TestNoAssignedSourcesKeepsPreviousOutput(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "Before")
	mustCompile(t)
	before, _ := os.ReadFile(out)
	database.Exec("DELETE FROM compilation_content WHERE id='test'")
	assertQueued(t, true)
	if ok, err := update_compilation("test"); err == nil || ok {
		t.Fatal("no-assigned-sources accepted")
	}
	after, _ := os.ReadFile(out)
	if string(after) != string(before) {
		t.Fatal("output emptied")
	}
}

func TestConfigurationAndMembershipChangesQueueBuild(t *testing.T) {
	path, _ := compilerFixture(t)
	putSource(t, path, "Before")
	mustCompile(t)
	assertQueued(t, false)
	database.Exec("UPDATE compilation SET name='Changed' WHERE id='test'")
	assertQueued(t, true)
	mustCompile(t)
	assertQueued(t, false)
	k.Set("items.max", 3)
	assertQueued(t, true)
	mustCompile(t)
	assertQueued(t, false)
	database.Exec("DELETE FROM compilation_content WHERE id='test'")
	assertQueued(t, true)
}
func TestMissingOutputQueuesBuild(t *testing.T) {
	path, out := compilerFixture(t)
	putSource(t, path, "Before")
	mustCompile(t)
	os.Remove(out)
	assertQueued(t, true)
}

func TestValidEmptySourceCanProduceEmptyFeed(t *testing.T) {
	path, out := compilerFixture(t)
	data := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Empty</title><link>https://example.org</link><description>Empty but valid</description></channel></rss>`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.RecordFeedRefresh(database.DB, 1, path, fmt.Sprintf("%x", sha256.Sum256(data)), 100); err != nil {
		t.Fatal(err)
	}
	mustCompile(t)
	file, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	feed, err := gofeed.NewParser().Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Items) != 0 {
		t.Fatal("expected empty feed")
	}
}
