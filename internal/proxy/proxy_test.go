package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
)

func TestProxyRecordsUsageAndPassesBodyThrough(t *testing.T) {
	const body = `{"model":"claude-x","usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":90}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	lg, err := proxy.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"anthropic": up.URL}))
	defer px.Close()

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != body {
		t.Fatalf("client body altered: %s", got)
	}
	lg.Close()

	f, _ := os.Open(logPath)
	defer f.Close()
	evs, err := stats.Load(f, time.Time{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events=%d err=%v", len(evs), err)
	}
	e := evs[0]
	if !e.HasUsage || e.Model != "claude-x" || e.InputTokens != 3 || e.OutputTokens != 4 || e.CacheReadTokens != 90 {
		t.Fatalf("bad event %+v", e)
	}
	rows := stats.Aggregate(evs)
	if len(rows) != 1 || rows[0].CacheHitRate < 0.96 || rows[0].CacheHitRate > 0.97 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestUnknownProvider(t *testing.T) {
	lg, _ := proxy.NewLogger(filepath.Join(t.TempDir(), "e.jsonl"))
	defer lg.Close()
	rec := httptest.NewRecorder()
	proxy.Handler(lg, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/nope/v1/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code %d", rec.Code)
	}
}
