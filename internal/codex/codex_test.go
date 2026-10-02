package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `{"timestamp":"2026-10-02T18:00:00.000Z","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"2026-10-02T18:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.5"}}
{"timestamp":"2026-10-02T18:00:02.000Z","type":"response_item","payload":{"type":"message","text":"secret prompt"}}
{"timestamp":"2026-10-02T18:00:03.000Z","type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":50,"reasoning_output_tokens":10}}}
{"timestamp":"2026-10-02T18:00:03.500Z","type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":50}}}
`

func TestPollImportsResponsesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026", "rollout-x.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(sample), 0o644)

	s := &Scanner{Roots: []string{dir}}
	evs, err := s.Poll()
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %d events, err %v; want 1", len(evs), err)
	}
	e := evs[0]
	if e.Provider != "openai" || e.Source != Source || e.Model != "gpt-5.5" || e.ID != "resp_1" ||
		e.InputTokens != 600 || e.CacheReadTokens != 400 || e.OutputTokens != 50 || e.ReasoningTokens != 10 {
		t.Errorf("unexpected event %+v", e)
	}
	if e.HasLatency() {
		t.Error("imported events must not claim latency")
	}

	// Appended lines are picked up, and the model carries over between polls.
	more := `{"timestamp":"2026-10-02T18:05:00.000Z","type":"token_usage_record","payload":{"response_id":"resp_2","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}}` + "\n"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(more)
	f.WriteString(strings.TrimSuffix(more, "\n")) // unterminated: left for later
	f.Close()
	evs, _ = s.Poll()
	if len(evs) != 1 || evs[0].ID != "resp_2" || evs[0].Model != "gpt-5.5" {
		t.Fatalf("second poll = %+v", evs)
	}
	if len(s.All()) != 2 {
		t.Errorf("All() = %d, want 2", len(s.All()))
	}
}

func TestSinceSkipsOldEvents(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(sample), 0o644)
	s := &Scanner{Roots: []string{dir}, Since: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
	if evs, _ := s.Poll(); len(evs) != 0 {
		t.Errorf("got %d events, want 0", len(evs))
	}
}
