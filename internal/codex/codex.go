// Package codex reads token usage from Codex's own session logs
// (~/.codex/sessions/**/rollout-*.jsonl), so Codex calls show up in stats and
// top even when Codex is signed in with ChatGPT, whose traffic cannot be sent
// through the lmon proxy (Codex insists on an HTTPS origin for that backend).
//
// Only usage numbers, the model name, the response id and the timestamp are
// read. Prompts, replies and tool output in those files are never kept.
//
// Each API response is recorded once, as a token_usage_record line carrying
// the response id. The model comes from the latest turn_context line before it.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

const Source = "codex"

// DefaultRoots returns the session directories to scan: $CODEX_HOME/sessions
// if set, otherwise ~/.codex/sessions, whichever exist.
func DefaultRoots() []string {
	base := os.Getenv("CODEX_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(home, ".codex")
	}
	p := filepath.Join(base, "sessions")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return []string{p}
	}
	return nil
}

type fileState struct {
	offset int64
	model  string // from the latest turn_context seen in this file
}

// Scanner incrementally reads Codex session logs. The zero value is not
// usable; set Roots (and optionally Since) first.
type Scanner struct {
	Roots []string
	// Since skips log files not modified since then, and events older than it.
	// The zero time means everything.
	Since time.Time

	files map[string]*fileState
	resp  map[string]*proxy.Event // by response id
}

// line holds just the fields we need; everything else in the file is skipped.
type line struct {
	Type      string    `json:"type"`
	Timestamp string    `json:"timestamp"`
	Payload   *struct { // turn_context: model. token_usage_record: the rest.
		Model      string `json:"model"`
		ResponseID string `json:"response_id"`
		Usage      *struct {
			Input     int `json:"input_tokens"` // includes the cached part
			Cached    int `json:"cached_input_tokens"`
			Output    int `json:"output_tokens"`
			Reasoning int `json:"reasoning_output_tokens"`
		} `json:"usage"`
	} `json:"payload"`
}

// Poll reads whatever was appended since the last call and returns the
// responses it found.
func (s *Scanner) Poll() ([]proxy.Event, error) {
	if s.files == nil {
		s.files, s.resp = map[string]*fileState{}, map[string]*proxy.Event{}
	}
	var out []proxy.Event
	var firstErr error
	for _, root := range s.Roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			st := s.files[path]
			if st == nil {
				if !s.Since.IsZero() && info.ModTime().Before(s.Since) {
					return nil // old session; look again next poll in case it revives
				}
				st = &fileState{}
				s.files[path] = st
			}
			if info.Size() < st.offset { // truncated: start over (ids dedupe)
				st.offset = 0
			}
			if info.Size() == st.offset {
				return nil
			}
			evs, err := s.readNew(path, st)
			out = append(out, evs...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out, firstErr
}

// All returns every response seen so far.
func (s *Scanner) All() []proxy.Event {
	out := make([]proxy.Event, 0, len(s.resp))
	for _, e := range s.resp {
		out = append(out, *e)
	}
	return out
}

// readNew consumes complete lines from st.offset. A final line without its
// newline is left unconsumed and re-read next time, once it is complete.
func (s *Scanner) readNew(path string, st *fileState) ([]proxy.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return nil, err
	}
	var out []proxy.Event
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := r.ReadBytes('\n')
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		st.offset += int64(len(b))
		if e, ok := s.handle(b, st); ok {
			out = append(out, e)
		}
	}
}

var (
	usageMarker = []byte(`"token_usage_record"`)
	turnMarker  = []byte(`"turn_context"`)
)

func (s *Scanner) handle(b []byte, st *fileState) (proxy.Event, bool) {
	isUsage := bytes.Contains(b, usageMarker)
	if !isUsage && !bytes.Contains(b, turnMarker) { // cheap reject: most lines are neither
		return proxy.Event{}, false
	}
	var l line
	if json.Unmarshal(b, &l) != nil || l.Payload == nil {
		return proxy.Event{}, false
	}
	switch {
	case l.Type == "turn_context":
		if l.Payload.Model != "" {
			st.model = l.Payload.Model
		}
	case l.Type == "token_usage_record" && l.Payload.Usage != nil:
		id, u := l.Payload.ResponseID, l.Payload.Usage
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if id == "" || st.model == "" || err != nil || ts.Before(s.Since) || s.resp[id] != nil {
			return proxy.Event{}, false
		}
		cached := min(u.Cached, u.Input)
		e := &proxy.Event{
			Time: ts, Provider: "openai", Source: Source, Status: 200, HasUsage: true,
			Usage: usage.Usage{
				Model: st.model, ID: id,
				InputTokens: u.Input - cached, CacheReadTokens: cached,
				OutputTokens: u.Output, ReasoningTokens: u.Reasoning,
			},
		}
		s.resp[id] = e
		return *e, true
	}
	return proxy.Event{}, false
}
