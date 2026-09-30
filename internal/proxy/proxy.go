// Package proxy is a local reverse proxy that records usage, latency and cache
// stats for LLM API calls without changing what the client sees.
//
// Clients point their SDK base URL at http://localhost:PORT/<provider>, e.g.
// ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
// OPENAI_BASE_URL=http://localhost:8787/openai/v1
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shadowdex/lmon/internal/usage"
)

// Event is one recorded API call. It never contains prompts or responses.
type Event struct {
	Time      time.Time `json:"time"`
	Provider  string    `json:"provider"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Streaming bool      `json:"streaming"`
	TTFBMs    float64   `json:"ttfb_ms"`
	TotalMs   float64   `json:"total_ms"`
	usage.Usage
	HasUsage bool `json:"has_usage"`
}

type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// DefaultLogPath is ~/.lmon/events.jsonl.
func DefaultLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lmon", "events.jsonl")
}

func NewLogger(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

func (l *Logger) Write(e Event) {
	b, _ := json.Marshal(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}

func (l *Logger) Close() error { return l.f.Close() }

// Handler returns the proxy handler. upstreams overrides a provider's default
// upstream URL (used in tests).
func Handler(log *Logger, upstreams map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		prov, rest, _ := strings.Cut(trimmed, "/")
		ad, ok := usage.Registry[prov]
		if !ok {
			http.Error(w, "lmon: unknown provider "+fmt.Sprintf("%q", prov)+"; use /<provider>/... (anthropic, openai, xai, groq, mistral, deepseek, openrouter)", http.StatusNotFound)
			return
		}
		base := ad.Upstream()
		if u, ok := upstreams[prov]; ok {
			base = u
		}
		target, err := url.Parse(base)
		if err != nil {
			http.Error(w, "lmon: bad upstream", http.StatusInternalServerError)
			return
		}

		start := time.Now()
		var ttfb time.Duration
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + "/" + rest
				pr.Out.URL.RawPath = ""
				pr.Out.Host = target.Host
				// Drop the client's Accept-Encoding so Go negotiates gzip itself and
				// hands us (and the client) a decoded body we can parse.
				pr.Out.Header.Del("Accept-Encoding")
			},
			FlushInterval: -1, // flush immediately so SSE streams aren't buffered
			ModifyResponse: func(resp *http.Response) error {
				ttfb = time.Since(start)
				// Tee the body so usage can be parsed without delaying the client.
				resp.Body = &teeBody{
					rc: resp.Body,
					onEOF: func(body []byte) {
						ev := Event{
							Time: start, Provider: prov, Path: "/" + rest, Status: resp.StatusCode,
							TTFBMs: ms(ttfb), TotalMs: ms(time.Since(start)),
						}
						ct := resp.Header.Get("Content-Type")
						var u usage.Usage
						var found bool
						if strings.HasPrefix(ct, "text/event-stream") {
							ev.Streaming = true
							u, found = ad.ParseSSE(body)
						} else {
							u, found = ad.ParseJSON(body)
						}
						ev.Usage, ev.HasUsage = u, found
						log.Write(ev)
					},
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				log.Write(Event{Time: start, Provider: prov, Path: "/" + rest, Status: http.StatusBadGateway, TotalMs: ms(time.Since(start))})
				http.Error(w, "lmon: upstream error: "+err.Error(), http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// maxCapture bounds memory per response kept for usage parsing.
const maxCapture = 16 << 20

// teeBody passes bytes through untouched while keeping a bounded copy.
type teeBody struct {
	rc    io.ReadCloser
	buf   bytes.Buffer
	once  sync.Once
	onEOF func([]byte)
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 && t.buf.Len() < maxCapture {
		t.buf.Write(p[:n])
	}
	if err != nil {
		t.once.Do(func() { t.onEOF(t.buf.Bytes()) })
	}
	return n, err
}

func (t *teeBody) Close() error {
	t.once.Do(func() { t.onEOF(t.buf.Bytes()) })
	return t.rc.Close()
}
