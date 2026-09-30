package geoip

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestInvalidMMDBIsRejectedAndExistingDBKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gz(t, "<html>definitely not an mmdb</html>"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "db.mmdb")
	os.WriteFile(dest, []byte("old-good-db"), 0o644)

	_, err := Update(context.Background(), Options{BaseURL: srv.URL, Dest: dest, Now: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)})
	if err == nil || !strings.Contains(err.Error(), "not a valid mmdb") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "old-good-db" {
		t.Fatalf("existing db was clobbered: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".geoip-*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestFallsBackToPreviousMonthOn404(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "db.mmdb")
	_, err := Update(context.Background(), Options{BaseURL: srv.URL, Dest: dest, Now: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"/dbip-city-lite-2026-01.mmdb.gz", "/dbip-city-lite-2025-12.mmdb.gz"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("requested %v, want %v", paths, want)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dest should not exist after failed update")
	}
}

func TestNonGzipBodyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain text, not gzip"))
	}))
	defer srv.Close()
	_, err := Update(context.Background(), Options{BaseURL: srv.URL, Dest: filepath.Join(t.TempDir(), "d"), Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "not a gzip") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePrefersExplicit(t *testing.T) {
	if got := Resolve("/x/y.mmdb"); got != "/x/y.mmdb" {
		t.Fatalf("got %q", got)
	}
}
