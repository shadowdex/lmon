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
	"sort"
	"strconv"
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

// Rotation controls size-based log rotation. When the active file would grow
// past MaxBytes it becomes <path>.1 (older files shift up to <path>.Keep, and
// the oldest is deleted). MaxBytes <= 0 disables rotation.
type Rotation struct {
	MaxBytes int64
	Keep     int
}

// DefaultRotation keeps about 200 MB at most: the active file plus three
// rotated ones of 50 MB each.
var DefaultRotation = Rotation{MaxBytes: 50 << 20, Keep: 3}

// Logger appends events to a JSONL file, rotating it by size. It is safe for
// concurrent use by one process; run a single proxy per log file.
type Logger struct {
	// OnError, if set, is called for write and rotation failures. Events are
	// never dropped because rotation failed: the logger keeps appending.
	OnError func(error)

	mu      sync.Mutex
	f       *os.File
	path    string
	rot     Rotation
	size    int64
	retryAt int64 // don't retry a failed rotation until size reaches this
}

// DefaultLogPath is ~/.lmon/events.jsonl.
func DefaultLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lmon", "events.jsonl")
}

func NewLogger(path string) (*Logger, error) { return NewRotatingLogger(path, DefaultRotation) }

func NewRotatingLogger(path string, rot Rotation) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Logger{path: path, rot: rot}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func (l *Logger) report(err error) {
	if l.OnError != nil {
		l.OnError(err)
	}
}

func (l *Logger) Write(e Event) {
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil { // a previous rotation couldn't reopen the file
		if err := l.open(); err != nil {
			l.report(fmt.Errorf("event dropped, cannot open log: %w", err))
			return
		}
	}
	if l.rot.MaxBytes > 0 && l.size > 0 && l.size+int64(len(b)) > l.rot.MaxBytes && l.size >= l.retryAt {
		l.rotate()
		if l.f == nil {
			l.report(fmt.Errorf("event dropped, cannot reopen log after rotation"))
			return
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		l.report(err)
	}
}

// rotate must be called with l.mu held. The file is closed first because
// Windows can't rename an open file. If the rename fails we reopen the same
// file and keep appending, retrying after another 1 MiB.
func (l *Logger) rotate() {
	l.f.Close()
	l.f = nil
	shiftErr := l.shift()
	if err := l.open(); err != nil {
		l.report(err)
		return
	}
	if shiftErr != nil {
		l.retryAt = l.size + 1<<20
		l.report(fmt.Errorf("log rotation failed, continuing to append: %w", shiftErr))
		return
	}
	l.retryAt = 0
}

func (l *Logger) shift() error {
	if l.rot.Keep <= 0 {
		return os.Remove(l.path)
	}
	name := func(i int) string { return fmt.Sprintf("%s.%d", l.path, i) }
	if err := os.Remove(name(l.rot.Keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := l.rot.Keep - 1; i >= 1; i-- {
		if err := os.Rename(name(i), name(i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(l.path, name(1))
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// LogFiles returns the active log and its rotated files, oldest first
// (<path>.N ... <path>.1, <path>). Only files that exist are returned.
func LogFiles(path string) []string {
	entries, _ := os.ReadDir(filepath.Dir(path))
	base := filepath.Base(path)
	type rotated struct {
		n    int
		name string
	}
	var rot []rotated
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), base+".")
		if !ok || e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(suffix); err == nil && n > 0 {
			rot = append(rot, rotated{n, filepath.Join(filepath.Dir(path), e.Name())})
		}
	}
	sort.Slice(rot, func(i, j int) bool { return rot[i].n > rot[j].n })
	var out []string
	for _, r := range rot {
		out = append(out, r.name)
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	return out
}

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
