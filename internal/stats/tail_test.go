package stats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendFile(t *testing.T, p, s string) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(s)
}

func line(model string) string {
	return `{"time":"2026-01-01T00:00:00Z","provider":"anthropic","model":"` + model + `","status":200}` + "\n"
}

func TestTailerMissingFileThenGrowth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	if evs, err := tl.Poll(); err != nil || len(evs) != 0 {
		t.Fatalf("missing file: %v %v", evs, err)
	}
	appendFile(t, p, line("a")+line("b"))
	evs, _ := tl.Poll()
	if len(evs) != 2 {
		t.Fatalf("got %d", len(evs))
	}
	if evs, _ := tl.Poll(); len(evs) != 0 {
		t.Fatalf("re-read %d old events", len(evs))
	}
	appendFile(t, p, line("c"))
	if evs, _ := tl.Poll(); len(evs) != 1 || evs[0].Model != "c" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTailerHoldsPartialLineUntilNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	full := line("x")
	appendFile(t, p, full[:20])
	if evs, _ := tl.Poll(); len(evs) != 0 {
		t.Fatalf("parsed a partial line: %+v", evs)
	}
	appendFile(t, p, full[20:])
	if evs, _ := tl.Poll(); len(evs) != 1 || evs[0].Model != "x" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTailerRestartsAfterTruncation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	appendFile(t, p, line("a")+line("b")+line("c"))
	tl.Poll()
	os.WriteFile(p, []byte(line("new")), 0o644) // shorter than before
	evs, _ := tl.Poll()
	if len(evs) != 1 || evs[0].Model != "new" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTailerSkipsGarbageAndBoundsInitialRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	appendFile(t, p, strings.Repeat(line("old"), 50)+"not json\n"+line("tail"))
	tl := &Tailer{Path: p, MaxInitial: int64(len(line("tail")) + 30)}
	evs, _ := tl.Poll()
	if len(evs) != 1 || evs[0].Model != "tail" {
		t.Fatalf("got %d events, first=%+v", len(evs), evs)
	}
}
