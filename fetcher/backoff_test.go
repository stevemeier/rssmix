package main

import (
	"context"
	"fmt"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testHost(t *testing.T) (*hostTransport, *time.Time, *int, *string, *int) {
	t.Helper()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	status := 429
	retry := ""
	calls := 0
	tr := newHostTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		h := http.Header{}
		if retry != "" {
			h.Set("Retry-After", retry)
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: ioutil.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	tr.now = func() time.Time { return now }
	tr.wait = func(ctx context.Context, d time.Duration) error { now = now.Add(d); return nil }
	return tr, &now, &status, &retry, &calls
}
func send(t *testing.T, tr *hostTransport, address string) error {
	t.Helper()
	r, _ := http.NewRequest("GET", address, nil)
	r = r.WithContext(context.WithValue(r.Context(), feedAddressKey{}, address))
	resp, err := tr.RoundTrip(r)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}
func TestExponentialBackoffAndRestart(t *testing.T) {
	tr, now, status, retry, calls := testHost(t)
	_ = retry
	path := filepath.Join(t.TempDir(), "state.json")
	if err := tr.loadState(path); err != nil {
		t.Fatal(err)
	}
	address := "https://example.org/b"
	for i, want := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		if err := send(t, tr, address); err != nil {
			t.Fatal(err)
		}
		s := tr.states["example.org"]
		if s.Failures != i+1 || s.Until.Sub(*now) != want {
			t.Fatalf("step %d: %+v", i, s)
		}
		if s.ResumeURL != address {
			t.Fatal("resume not atomically persisted")
		}
		n := *calls
		if err := send(t, tr, address); err == nil || *calls != n {
			t.Fatal("blocked request reached server")
		}
		restored := newHostTransport(tr.base)
		restored.now = tr.now
		restored.wait = tr.wait
		if err := restored.loadState(path); err != nil {
			t.Fatal(err)
		}
		if restored.states["example.org"].Failures != i+1 {
			t.Fatal("count not restored")
		}
		if err := send(t, restored, address); err == nil {
			t.Fatal("restart lost cooldown")
		}
		tr = restored
		*now = s.Until
	}
	*status = 200
	if err := send(t, tr, address); err != nil {
		t.Fatal(err)
	}
	if tr.states["example.org"].Failures != 6 {
		t.Fatal("single 200 reset count")
	}
	if err := tr.recovered("example.org"); err != nil {
		t.Fatal(err)
	}
	if tr.states["example.org"].Failures != 0 {
		t.Fatal("successful pass did not reset")
	}
}
func TestRetryAfterAndOtherHosts(t *testing.T) {
	for _, date := range []bool{false, true} {
		tr, now, status, retry, calls := testHost(t)
		*status = 503
		if date {
			*retry = now.Add(2 * time.Hour).Format(http.TimeFormat)
		} else {
			*retry = "7200"
		}
		if err := send(t, tr, "https://example.org/a"); err != nil {
			t.Fatal(err)
		}
		if tr.states["example.org"].Until.Sub(*now) != 2*time.Hour {
			t.Fatal("Retry-After shortened")
		}
		*status = 200
		if err := send(t, tr, "https://other.org/a"); err != nil {
			t.Fatal(err)
		}
		if *calls != 2 {
			t.Fatal("other host blocked")
		}
	}
}
func TestResumeRotation(t *testing.T) {
	tr, _, _, _, _ := testHost(t)
	feeds := []feedRef{{1, "https://example.org/a"}, {2, "https://example.org/b"}, {3, "https://example.org/c"}, {4, "https://other.org/a"}}
	if err := tr.rememberRetry(feeds[1].URL); err != nil {
		t.Fatal(err)
	}
	tr.rememberRetry(feeds[2].URL) // Skipped later feeds must not replace failed feed.
	got := tr.orderFeeds(feeds)
	for i, want := range []int64{2, 3, 1, 4} {
		if got[i].ID != want {
			t.Fatalf("order %+v", got)
		}
	}
	tr.completed(feeds[1].URL)
	if tr.states["example.org"].ResumeURL != "" {
		t.Fatal("completed cursor retained")
	}
}
func TestInvalidStateStopsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ioutil.WriteFile(path, []byte("broken"), 0600)
	tr, _, _, _, _ := testHost(t)
	if tr.loadState(path) == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestSQLitePassRecoveryAndResume(t *testing.T) {
	oldDB, oldK, oldTransport := database, k, feedTransport
	defer func() { database = oldDB; k = oldK; feedTransport = oldTransport }()
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite3", filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	database = db
	for _, q := range []string{"CREATE TABLE feed(id integer primary key, uschema text,urn text,filename text)", "CREATE TABLE feed_status(id integer primary key,refreshed integer,updated integer,active integer)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	failPath := "/b"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if r.URL.Path == failPath {
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, "feed-"+r.URL.Path)
	}))
	defer server.Close()
	for i, path := range []string{"/a", "/b"} {
		if _, err := db.Exec("INSERT INTO feed(id,uschema,urn) VALUES (?,'http',?)", i+1, strings.TrimPrefix(server.URL, "http://")+path); err != nil {
			t.Fatal(err)
		}
		db.Exec("INSERT INTO feed_status VALUES (?,0,0,1)", i+1)
	}
	k = koanf.New(".")
	k.Set("subdirs", 0)
	tr, now, _, _, _ := testHost(t)
	tr.base = http.DefaultTransport
	feedTransport = tr
	statePath := filepath.Join(dir, "state.json")
	if err := tr.loadState(statePath); err != nil {
		t.Fatal(err)
	}
	refresh_feeds(dir)
	host := addressHost(server.URL)
	if tr.states[host].Failures != 1 || tr.states[host].ResumeURL != server.URL+"/b" {
		t.Fatalf("first failure %+v", tr.states[host])
	}
	// A second pass during cooldown must make no network requests.
	refresh_feeds(dir)
	if len(requests) != 2 {
		t.Fatal("cooldown sent request")
	}
	*now = tr.states[host].Until
	failPath = "/a"
	refresh_feeds(dir)
	if requests[2] != "/b" || requests[3] != "/a" {
		t.Fatalf("did not resume: %v", requests)
	}
	if tr.states[host].Failures != 2 {
		t.Fatal("partial success reset escalation")
	}
	*now = tr.states[host].Until
	failPath = ""
	refresh_feeds(dir)
	if tr.states[host].Failures != 0 || tr.states[host].ResumeURL != "" {
		t.Fatal("full successful pass did not reset")
	}
	restored := newHostTransport(http.DefaultTransport)
	if err := restored.loadState(statePath); err != nil {
		t.Fatal(err)
	}
	if restored.states[host].Failures != 0 {
		t.Fatal("reset not persisted")
	}
}
