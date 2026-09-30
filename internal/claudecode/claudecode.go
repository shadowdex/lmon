// Package claudecode reads token usage from Claude Code's own session logs
// (~/.claude/projects/**/*.jsonl), so calls that never went through the lmon
// proxy still show up in stats and top.
//
// Only usage numbers, the model name, the response id and the timestamp are
// read. Prompts, replies and tool output in those files are never kept.
//
// Claude Code writes one assistant line per content block, so a single API
// response usually spans several lines that repeat the same usage, except that
// output_tokens grows as the stream progresses. Responses are therefore keyed
// by message.id and merged taking the largest value of each counter.
package claudecode

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

const Source = "claude-code"

// DefaultRoots returns the project-log directories to scan: every entry of
// $CLAUDE_CONFIG_DIR (comma separated) if set, otherwise ~/.claude and
// ~/.config/claude, whichever exist.
func DefaultRoots() []string {
	var bases []string
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		for _, b := range strings.Split(env, ",") {
			if b = strings.TrimSpace(b); b != "" {
				bases = append(bases, b)
			}
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		bases = []string{filepath.Join(home, ".claude"), filepath.Join(home, ".config", "claude")}
	}
	var roots []string
	for _, b := range bases {
		p := filepath.Join(b, "projects")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

type fileState struct{ offset int64 }

// Scanner incrementally reads Claude Code logs. The zero value is not usable;
// set Roots (and optionally Since) first.
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
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	RequestID string          `json:"requestId"`
	Message   json.RawMessage `json:"message"`
}

// Poll reads whatever was appended since the last call and returns the
// responses it touched, each in full and up to date. A response can be
// returned again later with larger output_tokens as its stream completes, so
// callers should replace by Event.ID rather than add.
func (s *Scanner) Poll() ([]proxy.Event, error) {
	if s.files == nil {
		s.files, s.resp = map[string]*fileState{}, map[string]*proxy.Event{}
	}
	touched := map[string]bool{}
	var firstErr error
	for _, root := range s.Roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil // unreadable subtree or other file: skip, keep walking
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
			if info.Size() < st.offset { // truncated: start over (merging is idempotent)
				st.offset = 0
			}
			if info.Size() == st.offset {
				return nil
			}
			if err := s.readNew(path, st, touched); err != nil && firstErr == nil {
				firstErr = err
			}
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	out := make([]proxy.Event, 0, len(touched))
	for id := range touched {
		out = append(out, *s.resp[id])
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
func (s *Scanner) readNew(path string, st *fileState, touched map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := r.ReadBytes('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		st.offset += int64(len(b))
		s.handle(b, touched)
	}
}

var usageMarker = []byte(`"usage"`)

func (s *Scanner) handle(b []byte, touched map[string]bool) {
	if !bytes.Contains(b, usageMarker) { // cheap reject: most lines are not API responses
		return
	}
	var l line
	if json.Unmarshal(b, &l) != nil || l.Type != "assistant" || len(l.Message) == 0 {
		return
	}
	u, ok := usage.Anthropic{}.ParseJSON(l.Message)
	if !ok || u.Model == "" || strings.HasPrefix(u.Model, "<") { // "<synthetic>" = no real API call
		return
	}
	id := u.ID
	if id == "" {
		id = l.RequestID
	}
	if id == "" {
		return
	}
	u.ID = id
	ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil || ts.Before(s.Since) {
		return
	}

	cur := s.resp[id]
	if cur == nil {
		s.resp[id] = &proxy.Event{
			Time: ts, Provider: "anthropic", Source: Source, Status: 200,
			Usage: u, HasUsage: true,
		}
		touched[id] = true
		return
	}
	if ts.Before(cur.Time) {
		cur.Time = ts // the response started at its earliest line
		touched[id] = true
	}
	changed := false
	// Every counter must be updated, so don't short-circuit.
	for _, p := range []struct {
		dst *int
		v   int
	}{
		{&cur.InputTokens, u.InputTokens}, {&cur.OutputTokens, u.OutputTokens},
		{&cur.CacheReadTokens, u.CacheReadTokens}, {&cur.CacheWriteTokens, u.CacheWriteTokens},
		{&cur.CacheWrite1hTokens, u.CacheWrite1hTokens}, {&cur.WebSearchRequests, u.WebSearchRequests},
	} {
		if p.v > *p.dst {
			*p.dst = p.v
			changed = true
		}
	}
	if changed {
		touched[id] = true
	}
}
