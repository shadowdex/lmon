package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// asst builds an assistant line shaped like Claude Code's real logs.
func asst(id string, at time.Time, model string, in, out, cr, cw, cw1h, search int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": at.Format("2006-01-02T15:04:05.000Z"), "requestId": "req_" + id,
		"isSidechain": false, "cwd": "/secret/project",
		"message": map[string]any{
			"id": id, "model": model, "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "PRIVATE REPLY TEXT"}},
			"usage": map[string]any{
				"input_tokens": in, "output_tokens": out,
				"cache_read_input_tokens": cr, "cache_creation_input_tokens": cw,
				"cache_creation":  map[string]any{"ephemeral_1h_input_tokens": cw1h, "ephemeral_5m_input_tokens": cw - cw1h},
				"server_tool_use": map[string]any{"web_search_requests": search},
			},
		},
	})
	return string(b) + "\n"
}

func other(typ string) string {
	b, _ := json.Marshal(map[string]any{"type": typ, "timestamp": base.Format(time.RFC3339), "message": map[string]any{"content": "a usage question"}})
	return string(b) + "\n"
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(s)
}

func scan(t *testing.T, root string) *Scanner {
	t.Helper()
	s := &Scanner{Roots: []string{root}}
	if _, err := s.Poll(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMultiLineResponseIsOneEventWithMaxOutput(t *testing.T) {
	root := t.TempDir()
	// One API response written as 3 content-block lines: identical input and
	// cache, output_tokens growing as the stream progressed (16, 16, 313).
	write(t, filepath.Join(root, "proj", "s1.jsonl"),
		asst("msg_A", base, "claude-sonnet-5", 2, 16, 21077, 4002, 4002, 0)+
			asst("msg_A", base.Add(time.Second), "claude-sonnet-5", 2, 16, 21077, 4002, 4002, 0)+
			asst("msg_A", base.Add(2*time.Second), "claude-sonnet-5", 2, 313, 21077, 4002, 4002, 0))

	evs := scan(t, root).All()
	if len(evs) != 1 {
		t.Fatalf("want 1 response, got %d", len(evs))
	}
	e := evs[0]
	if e.ID != "msg_A" || e.Provider != "anthropic" || e.Source != Source || !e.HasUsage || e.Status != 200 {
		t.Fatalf("metadata: %+v", e)
	}
	if e.InputTokens != 2 || e.OutputTokens != 313 || e.CacheReadTokens != 21077 || e.CacheWriteTokens != 4002 || e.CacheWrite1hTokens != 4002 {
		t.Fatalf("tokens must be the max per field, not summed over lines: %+v", e.Usage)
	}
	if !e.Time.Equal(base) {
		t.Fatalf("response time should be its first line: %v", e.Time)
	}
	if e.HasLatency() {
		t.Fatal("imported events have no latency")
	}
}

func TestSkipsSyntheticNonAssistantAndGarbage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"),
		asst("msg_syn", base, "<synthetic>", 0, 0, 0, 0, 0, 0)+
			other("user")+other("system")+
			"not json at all with \"usage\" inside\n"+
			`{"type":"assistant","timestamp":"bad","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`+"\n"+
			asst("msg_ok", base, "claude-sonnet-5", 1, 2, 0, 0, 0, 0))
	evs := scan(t, root).All()
	if len(evs) != 1 || evs[0].ID != "msg_ok" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSubagentFilesCountedAndForkedDuplicateOnce(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "proj", "sess1.jsonl")
	sub := filepath.Join(root, "proj", "sess1", "subagents", "agent-1.jsonl")
	fork := filepath.Join(root, "proj", "sess2.jsonl") // a resumed session repeats old history
	write(t, main, asst("msg_1", base, "claude-sonnet-5", 1, 10, 0, 0, 0, 0))
	write(t, sub, asst("msg_sub", base, "claude-haiku-4-5", 5, 50, 0, 0, 0, 0))
	write(t, fork, asst("msg_1", base, "claude-sonnet-5", 1, 10, 0, 0, 0, 0)+asst("msg_2", base, "claude-sonnet-5", 1, 20, 0, 0, 0, 0))

	evs := scan(t, root).All()
	got := map[string]int{}
	for _, e := range evs {
		got[e.ID] = e.OutputTokens
	}
	if len(evs) != 3 || got["msg_1"] != 10 || got["msg_sub"] != 50 || got["msg_2"] != 20 {
		t.Fatalf("got %v (%d events)", got, len(evs))
	}
}

func TestIncrementalPollReturnsUpdatedResponseNotADuplicate(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "p", "s.jsonl")
	write(t, p, asst("msg_A", base, "claude-sonnet-5", 2, 16, 100, 0, 0, 0))
	s := &Scanner{Roots: []string{root}}

	first, _ := s.Poll()
	if len(first) != 1 || first[0].OutputTokens != 16 {
		t.Fatalf("poll 1: %+v", first)
	}
	if again, _ := s.Poll(); len(again) != 0 {
		t.Fatalf("idle poll returned %d events", len(again))
	}
	// The stream finishes: a later line for the same response with final output.
	write(t, p, asst("msg_A", base.Add(time.Second), "claude-sonnet-5", 2, 313, 100, 0, 0, 0)+
		asst("msg_B", base.Add(time.Minute), "claude-sonnet-5", 1, 1, 0, 0, 0, 0))
	second, _ := s.Poll()
	byID := map[string]int{}
	for _, e := range second {
		byID[e.ID] = e.OutputTokens
	}
	if len(second) != 2 || byID["msg_A"] != 313 || byID["msg_B"] != 1 {
		t.Fatalf("poll 2 must return A (updated) and B: %+v", second)
	}
	if n := len(s.All()); n != 2 {
		t.Fatalf("All() has %d responses", n)
	}
}

func TestPartialLineIsWaitedFor(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "p", "s.jsonl")
	full := asst("msg_A", base, "claude-sonnet-5", 1, 2, 0, 0, 0, 0)
	write(t, p, full[:len(full)/2])
	s := &Scanner{Roots: []string{root}}
	if evs, _ := s.Poll(); len(evs) != 0 {
		t.Fatalf("parsed half a line: %+v", evs)
	}
	write(t, p, full[len(full)/2:])
	if evs, _ := s.Poll(); len(evs) != 1 || evs[0].ID != "msg_A" {
		t.Fatalf("got %+v", evs)
	}
}

func TestSinceSkipsOldFilesAndOldEvents(t *testing.T) {
	root := t.TempDir()
	oldF := filepath.Join(root, "p", "old.jsonl")
	newF := filepath.Join(root, "p", "new.jsonl")
	write(t, oldF, asst("msg_oldfile", base, "claude-sonnet-5", 1, 1, 0, 0, 0, 0))
	past := base.Add(-48 * time.Hour)
	os.Chtimes(oldF, past, past)
	write(t, newF, asst("msg_oldevent", base.Add(-72*time.Hour), "claude-sonnet-5", 1, 1, 0, 0, 0, 0)+
		asst("msg_fresh", base, "claude-sonnet-5", 1, 1, 0, 0, 0, 0))

	s := &Scanner{Roots: []string{root}, Since: base.Add(-24 * time.Hour)}
	s.Poll()
	evs := s.All()
	if len(evs) != 1 || evs[0].ID != "msg_fresh" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTruncatedFileIsReReadWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "p", "s.jsonl")
	write(t, p, asst("msg_A", base, "claude-sonnet-5", 1, 5, 0, 0, 0, 0)+asst("msg_B", base, "claude-sonnet-5", 1, 6, 0, 0, 0, 0))
	s := &Scanner{Roots: []string{root}}
	s.Poll()
	os.WriteFile(p, []byte(asst("msg_A", base, "claude-sonnet-5", 1, 5, 0, 0, 0, 0)), 0o644) // shorter than before
	s.Poll()
	got := map[string]int{}
	for _, e := range s.All() {
		got[e.ID]++
	}
	if got["msg_A"] != 1 || got["msg_B"] != 1 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestWebSearchAndMissingRootsAreHandled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"), asst("msg_W", base, "claude-haiku-4-5-20251001", 131747, 3435, 0, 0, 0, 4))
	evs := scan(t, root).All()
	if len(evs) != 1 || evs[0].WebSearchRequests != 4 {
		t.Fatalf("got %+v", evs)
	}
	s := &Scanner{Roots: []string{filepath.Join(root, "does-not-exist")}}
	if evs, err := s.Poll(); err != nil || len(evs) != 0 {
		t.Fatalf("missing root: %v %v", evs, err)
	}
}

func TestNoConversationContentIsRetained(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "p", "s.jsonl"), asst("msg_A", base, "claude-sonnet-5", 1, 2, 0, 0, 0, 0))
	b, _ := json.Marshal(scan(t, root).All())
	for _, leak := range []string{"PRIVATE REPLY TEXT", "/secret/project"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("event carries content %q: %s", leak, b)
		}
	}
}

func TestDefaultRootsHonoursEnv(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(a, "projects"), 0o755)
	os.MkdirAll(filepath.Join(b, "projects"), 0o755)
	t.Setenv("CLAUDE_CONFIG_DIR", a+", "+b+", "+filepath.Join(a, "nope"))
	roots := DefaultRoots()
	if len(roots) != 2 || roots[0] != filepath.Join(a, "projects") || roots[1] != filepath.Join(b, "projects") {
		t.Fatalf("got %v", roots)
	}
}
