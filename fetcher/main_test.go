package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf"
)

func TestConditionalGET(t *testing.T) {
	calls := 0
	lm := "Sun, 25 Oct 2026 11:28:45 GMT"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		if calls == 1 {
			if r.Header.Get("If-Modified-Since") != "" {
				t.Error("mtime must not be sent")
			}
			w.Header().Set("ETag", `W/"version-1"`)
			w.Header().Set("Last-Modified", lm)
			fmt.Fprint(w, "feed-one")
		} else {
			if r.Header.Get("If-None-Match") != `W/"version-1"` || r.Header.Get("If-Modified-Since") != lm {
				t.Error("missing validators")
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "feed")
	first, err := fetchFeed(s.Client(), s.URL, path)
	if err != nil || !first.changed {
		t.Fatalf("first: %+v %v", first, err)
	}
	before, _ := os.Stat(path)
	second, err := fetchFeed(s.Client(), s.URL, path)
	if err != nil || second.changed {
		t.Fatalf("second: %+v %v", second, err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) || calls != 2 {
		t.Fatal("304 rewrote file or extra requests")
	}
}

func TestUnconditionalAndSameLengthChanges(t *testing.T) {
	body := "AAAA"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("unexpected validators")
		}
		fmt.Fprint(w, body)
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "feed")
	for i, tc := range []struct {
		body    string
		changed bool
	}{{"AAAA", true}, {"AAAA", false}, {"BBBB", true}, {"C", true}} {
		body = tc.body
		got, err := fetchFeed(s.Client(), s.URL, path)
		if err != nil || got.changed != tc.changed {
			t.Fatalf("case %d: %+v %v", i, got, err)
		}
		b, _ := ioutil.ReadFile(path)
		if string(b) != tc.body {
			t.Fatalf("content %q", b)
		}
	}
}

func TestStaleValidatorsNotUsed(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			t.Error("stale ETag sent")
		}
		fmt.Fprint(w, "new")
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "feed")
	ioutil.WriteFile(path, []byte("old"), 0644)
	saveValidators(path+".http.json", feedValidators{URL: s.URL, ETag: `"old"`, SHA256: "wrong"})
	if _, err := fetchFeed(s.Client(), s.URL, path); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if _, err := fetchFeed(s.Client(), s.URL, path); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadFailurePreservesFiles(t *testing.T) {
	for _, kind := range []string{"429", "500", "partial", "oversize", "unexpected304"} {
		t.Run(kind, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "429":
					w.WriteHeader(429)
				case "500":
					w.WriteHeader(500)
				case "unexpected304":
					w.WriteHeader(304)
				case "partial":
					w.Header().Set("Content-Length", "1000")
					fmt.Fprint(w, "short")
				case "oversize":
					io.Copy(w, io.LimitReader(zeroReader{}, maxFeedSize+1))
				}
			}))
			defer s.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "feed")
			ioutil.WriteFile(path, []byte("original"), 0644)
			ioutil.WriteFile(path+".http.json", []byte("old metadata"), 0600)
			if _, err := fetchFeed(s.Client(), s.URL, path); err == nil {
				t.Fatal("expected error")
			}
			b, _ := ioutil.ReadFile(path)
			m, _ := ioutil.ReadFile(path + ".http.json")
			if string(b) != "original" || string(m) != "old metadata" {
				t.Fatal("failure modified files")
			}
			files, _ := ioutil.ReadDir(dir)
			if len(files) != 2 {
				t.Fatal("temporary file leaked")
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestRedirectPacing(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/feed", 302)
			return
		}
		fmt.Fprint(w, "feed")
	}))
	defer s.Close()
	now := time.Now()
	transport := newHostTransport(s.Client().Transport)
	transport.gap = 2 * time.Second
	transport.now = func() time.Time { return now }
	waited := time.Duration(0)
	transport.wait = func(ctx context.Context, d time.Duration) error { waited += d; now = now.Add(d); return nil }
	client := &http.Client{Transport: transport}
	if _, err := fetchFeed(client, s.URL+"/start", filepath.Join(t.TempDir(), "feed")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || waited != 2*time.Second {
		t.Fatalf("calls %d waited %s", calls, waited)
	}
}

func TestPacingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForHost(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshSQLiteStatus(t *testing.T) {
	oldDB, oldK, oldTransport := database, k, feedTransport
	defer func() { database = oldDB; k = oldK; feedTransport = oldTransport }()
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite3", filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	database = db
	for _, q := range []string{
		"CREATE TABLE feed (id integer primary key, uschema text, urn text, filename text)",
		"CREATE TABLE feed_status (id integer primary key, refreshed integer, updated integer, active integer)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	status := 200
	body := "feed-v1"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" {
			t.Error("HEAD must not be used")
		}
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	_, err = db.Exec("INSERT INTO feed (id,uschema,urn) VALUES (1,'http',?)", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO feed_status VALUES (1,0,0,1)")
	k = koanf.New(".")
	k.Set("subdirs", 0)
	feedTransport = newHostTransport(http.DefaultTransport)
	feedTransport.gap = 0
	refresh_feeds(dir)
	var path string
	var updated int64
	if err := db.QueryRow("SELECT filename FROM feed WHERE id=1").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if b, err := ioutil.ReadFile(path); err != nil || string(b) != body {
		t.Fatalf("feed %q %v", b, err)
	}
	db.Exec("UPDATE feed_status SET updated=42,refreshed=43")
	status = 304
	refresh_feeds(dir)
	db.QueryRow("SELECT updated FROM feed_status WHERE id=1").Scan(&updated)
	if updated != 42 {
		t.Fatal("304 marked feed changed")
	}
	status = 200
	refresh_feeds(dir)
	db.QueryRow("SELECT updated FROM feed_status WHERE id=1").Scan(&updated)
	if updated != 42 {
		t.Fatal("identical 200 marked feed changed")
	}
	body = "feed-v2"
	refresh_feeds(dir)
	db.QueryRow("SELECT updated FROM feed_status WHERE id=1").Scan(&updated)
	if updated == 42 {
		t.Fatal("new content did not update timestamp")
	}
	db.Exec("UPDATE feed_status SET updated=42,refreshed=43")
	status = 429
	refresh_feeds(dir)
	var refreshed int64
	db.QueryRow("SELECT refreshed,updated FROM feed_status WHERE id=1").Scan(&refreshed, &updated)
	if updated != 42 || refreshed != 43 {
		t.Fatal("failed request modified status")
	}
	refresh_feeds(dir)
	if requests != 5 {
		t.Fatalf("cooldown allowed network request: %d", requests)
	}
}
