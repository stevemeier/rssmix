package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf"
	"github.com/stevemeier/rssmix/lib"
)

func testRSSBody(title string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel><title>Test feed</title><link>https://example.org</link><description>Test</description><item><title>` + title + `</title><description>Content</description><guid>` + title + `</guid></item></channel></rss>`
}

func TestValidFeedFormats(t *testing.T) {
	cases := map[string]string{
		"rss":                testRSSBody("RSS item without link"),
		"rss empty":          `<rss version="2.0"><channel><title>Empty</title><link>https://example.org</link><description>Empty</description></channel></rss>`,
		"rss legacy":         `<rss version="0.91"><channel><title>Legacy</title><link>https://example.org</link><description>Legacy</description></channel></rss>`,
		"podcast extensions": `<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel><title>Podcast</title><link>https://example.org</link><description>Podcast</description><itunes:author>Author</itunes:author><item><title>Episode</title><description><![CDATA[<p>HTML & text</p>]]></description><podcast:chapters url="https://example.org/chapters.json" type="application/json+chapters"/><enclosure url="https://example.org/audio.mp3" type="audio/mpeg" length="100"/></item></channel></rss>`,
		"atom":               `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><id>urn:test</id><updated>2026-11-05T12:00:00Z</updated><entry><title>Item</title><id>urn:item</id><updated>2026-11-05T12:00:00Z</updated><content type="text">Content</content></entry></feed>`,
		"atom empty":         `<feed xmlns="http://www.w3.org/2005/Atom"><title>Empty</title><id>urn:test</id><updated>2026-11-05T12:00:00Z</updated></feed>`,
		"atom 0.3":           `<feed version="0.3" xmlns="http://purl.org/atom/ns#"><title>Legacy Atom</title><id>urn:test</id><modified>2026-11-05T12:00:00Z</modified></feed>`,
		"rdf":                `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/"><channel rdf:about="https://example.org/feed"><title>RDF</title><link>https://example.org</link><description>Test</description></channel><item rdf:about="https://example.org/item"><title>Item</title><link>https://example.org/item</link></item></rdf:RDF>`,
		"UTF8 BOM":           "\xef\xbb\xbf" + testRSSBody("BOM"),
		"latin1":             "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Caf\xe9</title><link>https://example.org</link><description>Test</description></channel></rss>",
		"doctype":            `<!DOCTYPE rss PUBLIC "-//Netscape Communications//DTD RSS 0.91//EN" "http://example.invalid/no-network.dtd"><rss version="0.91"><channel><title>Legacy</title><link>https://example.org</link><description>Test</description></channel></rss>`,
		"JSON 1":             `{"version":"https://jsonfeed.org/version/1","title":"JSON","items":[{"id":"1","content_text":"Text"}]}`,
		"JSON 1.1 empty":     `{"version":"https://jsonfeed.org/version/1.1","title":"JSON","items":[]}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateFeed([]byte(data)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func invalidFeedBodies() map[string]string {
	return map[string]string{
		"HTML":  "<!DOCTYPE html><html><body>Temporarily unavailable</body></html>",
		"XHTML": `<html xmlns="http://www.w3.org/1999/xhtml"><body>Error</body></html>`,
		"empty": "", "whitespace": " \n\t", "plain": "Too many requests",
		"XML error":           "<error>Unavailable</error>",
		"mismatched XML":      "<rss><channel></rss>",
		"truncated XML":       strings.TrimSuffix(testRSSBody("truncated"), "</rss>"),
		"trailing garbage":    testRSSBody("one") + "garbage",
		"second root":         testRSSBody("one") + "<html/>",
		"second document":     testRSSBody("one") + testRSSBody("two"),
		"missing channel":     "<rss version=\"2.0\"/>",
		"nested channel":      "<rss><wrapper><channel/></wrapper></rss>",
		"two channels":        "<rss><channel/><channel/></rss>",
		"fake atom namespace": "<feed xmlns=\"https://example.org/not-atom\"/>",
		"duplicate attribute": "<rss version=\"2.0\" version=\"2.0\"><channel/></rss>",
		"unknown entity":      "<rss><channel><title>&unknown;</title></channel></rss>",
		"external entity":     `<!DOCTYPE rss [<!ENTITY x SYSTEM "file:///etc/passwd">]><rss><channel><title>&x;</title></channel></rss>`,
		"excessive nesting":   "<rss><channel>" + strings.Repeat("<x>", maxXMLDepth) + strings.Repeat("</x>", maxXMLDepth) + "</channel></rss>",
		"JSON error":          `{"error":"temporarily unavailable"}`,
		"JSON missing items":  `{"version":"https://jsonfeed.org/version/1","title":"JSON"}`,
		"JSON null items":     `{"version":"https://jsonfeed.org/version/1","title":"JSON","items":null}`,
		"JSON null item":      `{"version":"https://jsonfeed.org/version/1","title":"JSON","items":[null]}`,
		"JSON malformed":      `{"version":"https://jsonfeed.org/version/1","title":"JSON","items":[}`,
		"JSON parser panic":   `{"version":"https://jsonfeed.org/version/1.1","title":"JSON","authors":[null],"items":[]}`,
	}
}
func TestInvalidResponsesAreRejected(t *testing.T) {
	for name, body := range invalidFeedBodies() {
		t.Run(name, func(t *testing.T) {
			if err := validateFeed([]byte(body)); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestRejected200PreservesCacheAndValidators(t *testing.T) {
	state := testStateStore(t)
	for name, body := range invalidFeedBodies() {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				w.Header().Set("ETag", `"bad"`)
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "feed")
			old := []byte(testRSSBody("Last good item"))
			os.WriteFile(path, old, 0600)
			metadata := feedValidators{URL: server.URL, ETag: `"good"`, SHA256: fmt.Sprintf("%x", sha256.Sum256(old)), ValidationVersion: feedValidationVersion}
			if err := state.saveValidators(metadata); err != nil {
				t.Fatal(err)
			}
			before, err := state.loadValidators(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			stat, _ := os.Stat(path)
			if _, err := fetchFeed(state, server.Client(), server.URL, path); err == nil {
				t.Fatal("expected rejection")
			}
			after, _ := os.ReadFile(path)
			afterMeta, err := state.loadValidators(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			afterStat, _ := os.Stat(path)
			if !bytes.Equal(old, after) || before != afterMeta || !stat.ModTime().Equal(afterStat.ModTime()) {
				t.Fatal("rejected response modified good cache/validators")
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 1 {
				t.Fatal("temporary file leaked")
			}
		})
	}
}

func TestInvalidFirstDownloadCreatesNoCache(t *testing.T) {
	state := testStateStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>Error</html>") }))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "feed")
	if _, err := fetchFeed(state, server.Client(), server.URL, path); err == nil {
		t.Fatal("invalid first response accepted")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("invalid cache files left: %v", files)
	}
}

func TestValidFeedWithWrongMIMEIsAccepted(t *testing.T) {
	state := testStateStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, testRSSBody("Valid RSS"))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "feed")
	if _, err := fetchFeed(state, server.Client(), server.URL, path); err != nil {
		t.Fatal(err)
	}
	metadata, err := state.loadValidators(server.URL)
	if err != nil || metadata.ValidationVersion != feedValidationVersion {
		t.Fatal("validation marker missing")
	}
}

func TestLegacyCacheIsValidatedBeforeConditionalGET(t *testing.T) {
	state := testStateStore(t)
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "feed")
			body := []byte("<html>Old broken cache</html>")
			if valid {
				body = []byte(testRSSBody("Good legacy cache"))
			}
			os.WriteFile(path, body, 0600)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if valid {
					if r.Header.Get("If-None-Match") != `"old"` {
						t.Error("valid legacy cache lost conditional GET")
					}
					w.WriteHeader(304)
				} else {
					if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
						t.Error("invalid legacy cache used validators")
					}
					fmt.Fprint(w, testRSSBody("Recovered cache"))
				}
			}))
			defer server.Close()
			state.saveValidators(feedValidators{URL: server.URL, ETag: `"old"`, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))})
			if _, err := fetchFeed(state, server.Client(), server.URL, path); err != nil {
				t.Fatal(err)
			}
			cached, _ := os.ReadFile(path)
			if err := validateFeed(cached); err != nil {
				t.Fatal(err)
			}
			metadata, err := state.loadValidators(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.ValidationVersion != feedValidationVersion {
				t.Fatal("legacy cache not marked validated")
			}
		})
	}
}

func TestRejectedFeedDoesNotAdvanceSQLiteRevision(t *testing.T) {
	oldDB, oldK, oldTransport := database, k, feedTransport
	defer func() { database = oldDB; k = oldK; feedTransport = oldTransport }()
	dir := t.TempDir()
	db, err := sqlx.Connect("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	database = db
	schema, err := os.ReadFile("../sql/sqlite3.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	body := testRSSBody("Good")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"response"`)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	db.Exec("INSERT INTO feed(id,uschema,urn) VALUES (1,'http',?)", strings.TrimPrefix(server.URL, "http://"))
	db.Exec("INSERT INTO feed_status VALUES (1,0,0,1)")
	if err := lib.EnsureCompilationState(db.DB); err != nil {
		t.Fatal(err)
	}
	k = koanf.New(".")
	k.Set("subdirs", 0)
	feedTransport = newHostTransport(server.Client().Transport)
	attachState(t, feedTransport, db.DB)
	feedTransport.gap = 0
	refresh_feeds(dir)
	var revision, updated, refreshed int64
	var path string
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&revision)
	db.QueryRow("SELECT updated,refreshed FROM feed_status WHERE id=1").Scan(&updated, &refreshed)
	db.QueryRow("SELECT filename FROM feed WHERE id=1").Scan(&path)
	cached, _ := os.ReadFile(path)
	meta, err := feedTransport.store.loadValidators(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body = "<html>Broken</html>"
	refresh_feeds(dir)
	var nextRev, nextUpdated, nextRefreshed int64
	db.QueryRow("SELECT revision FROM feed_revision WHERE feed_id=1").Scan(&nextRev)
	db.QueryRow("SELECT updated,refreshed FROM feed_status WHERE id=1").Scan(&nextUpdated, &nextRefreshed)
	nextCache, _ := os.ReadFile(path)
	nextMeta, err := feedTransport.store.loadValidators(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if revision != nextRev || updated != nextUpdated || refreshed != nextRefreshed || !bytes.Equal(cached, nextCache) || meta != nextMeta {
		t.Fatal("invalid response modified accepted feed state")
	}
}
