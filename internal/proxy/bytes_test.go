package proxy_test

import (
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

// one proxied call, returning the single logged event
func callAndLog(t *testing.T, upstream http.HandlerFunc, path, reqBody string, hdr map[string]string) proxy.Event {
	t.Helper()
	up := httptest.NewServer(upstream)
	defer up.Close()
	logPath := filepath.Join(t.TempDir(), "e.jsonl")
	lg, err := proxy.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"anthropic": up.URL}))
	defer px.Close()

	req, _ := http.NewRequest("POST", px.URL+path, strings.NewReader(reqBody))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	lg.Close()

	f, _ := os.Open(logPath)
	defer f.Close()
	evs, _ := stats.Load(f, time.Time{})
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	return evs[0]
}

func TestProxyCountsRequestAndResponseBytes(t *testing.T) {
	const reqBody = `{"model":"claude-x","messages":[{"role":"user","content":"hello"}]}`
	const respBody = `{"model":"claude-x","usage":{"input_tokens":3,"output_tokens":4}}`
	var upstreamSaw int
	ev := callAndLog(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamSaw = len(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, respBody)
	}, "/anthropic/v1/messages", reqBody, nil)

	if upstreamSaw != len(reqBody) {
		t.Fatalf("the proxy must forward the body unchanged: upstream got %d bytes", upstreamSaw)
	}
	if ev.BytesUp != int64(len(reqBody)) || ev.BytesDown != int64(len(respBody)) {
		t.Fatalf("bytes up/down = %d/%d, want %d/%d", ev.BytesUp, ev.BytesDown, len(reqBody), len(respBody))
	}
	if !ev.HasBytes() {
		t.Error("HasBytes should be true")
	}
}

func TestProxyCountsEveryByteOfAStreamedResponse(t *testing.T) {
	var sent int
	ev := callAndLog(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			"event: message_start\ndata: " + `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}` + "\n\n",
			"event: content_block_delta\ndata: " + strings.Repeat("x", 3000) + "\n\n",
			"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n",
		}
		for _, c := range chunks {
			n, _ := io.WriteString(w, c)
			sent += n
			fl.Flush()
		}
	}, "/anthropic/v1/messages", `{"stream":true}`, nil)
	if ev.BytesDown != int64(sent) || sent < 3000 {
		t.Fatalf("streamed %d bytes, event says %d", sent, ev.BytesDown)
	}
	if ev.BytesUp != int64(len(`{"stream":true}`)) {
		t.Fatalf("up = %d", ev.BytesUp)
	}
}

// The response is counted after decompression, what the client receives.
func TestResponseBytesAreCountedAfterDecompression(t *testing.T) {
	body := `{"model":"m","usage":{"input_tokens":1}}` + strings.Repeat(" ", 2000) // compresses very well
	ev := callAndLog(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, body)
			gz.Close()
			return
		}
		io.WriteString(w, body)
	}, "/anthropic/v1/messages", `{}`, map[string]string{"Accept-Encoding": "gzip"})
	if ev.BytesDown != int64(len(body)) {
		t.Fatalf("down = %d, want the decompressed %d", ev.BytesDown, len(body))
	}
}

func TestEmptyRequestBodyCountsZeroUp(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()
	lg, _ := proxy.NewLogger(filepath.Join(t.TempDir(), "e.jsonl"))
	defer lg.Close()
	var got proxy.Event
	lg.OnEvent = func(e proxy.Event) { got = e }
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"anthropic": up.URL}))
	defer px.Close()
	resp, err := http.Get(px.URL + "/anthropic/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got.BytesUp != 0 || got.BytesDown != int64(len(`{"data":[]}`)) {
		t.Fatalf("a GET has no request body: up=%d down=%d", got.BytesUp, got.BytesDown)
	}
}
