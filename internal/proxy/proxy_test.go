package proxy_test

import (
	"bufio"
	"compress/gzip"
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

func TestProxyStreamsSSEIncrementallyAndRecordsUsage(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: "+
			`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":5,"cache_read_input_tokens":100}}}`+"\n\n")
		fl.Flush()
		<-release // hold the stream open until the client has seen the first event
		io.WriteString(w, "event: message_delta\ndata: "+`{"type":"message_delta","usage":{"output_tokens":42}}`+"\n\n")
		fl.Flush()
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	lg, _ := proxy.NewLogger(logPath)
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"anthropic": up.URL}))
	defer px.Close()

	resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// The first event must arrive while the upstream is still blocked.
	first := make(chan string, 1)
	rd := bufio.NewReader(resp.Body)
	go func() { l, _ := rd.ReadString('\n'); first <- l }()
	select {
	case l := <-first:
		if !strings.HasPrefix(l, "event: message_start") {
			t.Fatalf("unexpected first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy buffered the stream instead of flushing")
	}
	close(release)
	io.Copy(io.Discard, rd)
	resp.Body.Close()
	lg.Close()

	f, _ := os.Open(logPath)
	defer f.Close()
	evs, _ := stats.Load(f, time.Time{})
	if len(evs) != 1 {
		t.Fatalf("events=%d", len(evs))
	}
	e := evs[0]
	if !e.Streaming || !e.HasUsage || e.InputTokens != 5 || e.CacheReadTokens != 100 || e.OutputTokens != 42 {
		t.Fatalf("bad event %+v", e)
	}
}

func TestProxyParsesGzipUpstreamAndClientStillGetsBody(t *testing.T) {
	const body = `{"model":"gpt-x","usage":{"prompt_tokens":10,"completion_tokens":2}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, body)
			gz.Close()
			return
		}
		io.WriteString(w, body)
	}))
	defer up.Close()

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	lg, _ := proxy.NewLogger(logPath)
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"openai": up.URL}))
	defer px.Close()

	req, _ := http.NewRequest("POST", px.URL+"/openai/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Accept-Encoding", "gzip") // what real SDKs send
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	lg.Close()
	if string(got) != body {
		t.Fatalf("client got %q (Content-Encoding=%q)", got, resp.Header.Get("Content-Encoding"))
	}
	f, _ := os.Open(logPath)
	defer f.Close()
	evs, _ := stats.Load(f, time.Time{})
	if len(evs) != 1 || !evs[0].HasUsage || evs[0].InputTokens != 10 || evs[0].OutputTokens != 2 {
		t.Fatalf("bad events %+v", evs)
	}
}

func TestProxyUpstreamDownReturns502AndLogsError(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	url := up.URL
	up.Close() // nothing listening any more

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	lg, _ := proxy.NewLogger(logPath)
	rec := httptest.NewRecorder()
	proxy.Handler(lg, map[string]string{"anthropic": url}).ServeHTTP(rec, httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(`{}`)))
	lg.Close()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code %d", rec.Code)
	}
	f, _ := os.Open(logPath)
	defer f.Close()
	evs, _ := stats.Load(f, time.Time{})
	if len(evs) != 1 || evs[0].Status != http.StatusBadGateway {
		t.Fatalf("bad events %+v", evs)
	}
}
