package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestExplicitUserAgent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("User-Agent") != "rssmix/1.0" {
			t.Errorf("User-Agent=%q", r.Header.Get("User-Agent"))
		}
		if r.Method != "GET" {
			t.Errorf("method=%s", r.Method)
		}
		if calls == 2 {
			if r.Header.Get("If-None-Match") != `"test"` {
				t.Error("conditional header missing")
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"test"`)
		fmt.Fprint(w, "test feed")
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "feed")
	first, err := fetchFeed(server.Client(), server.URL, path)
	if err != nil || !first.changed {
		t.Fatalf("initial: %+v %v", first, err)
	}
	second, err := fetchFeed(server.Client(), server.URL, path)
	if err != nil || second.changed {
		t.Fatalf("conditional: %+v %v", second, err)
	}
	if calls != 2 {
		t.Fatalf("requests=%d", calls)
	}
}
