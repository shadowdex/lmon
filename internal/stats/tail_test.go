package stats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestTailerReadsUnseenTailOfRotatedFileWithoutLossOrDuplicates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	appendFile(t, p, line("a")+line("b"))
	if evs, _ := tl.Poll(); len(evs) != 2 {
		t.Fatalf("setup: %d", len(evs))
	}
	appendFile(t, p, line("c")+line("d"))        // written, but not yet polled...
	if err := os.Rename(p, p+".1"); err != nil { // ...when the logger rotates
		t.Fatal(err)
	}
	appendFile(t, p, line("e"))

	evs, _ := tl.Poll()
	var got []string
	for _, e := range evs {
		got = append(got, e.Model)
	}
	if strings.Join(got, "") != "cde" {
		t.Fatalf("got %v, want [c d e] (c,d from the rotated file, e from the new one)", got)
	}
	appendFile(t, p, line("f"))
	if evs, _ := tl.Poll(); len(evs) != 1 || evs[0].Model != "f" {
		t.Fatalf("after rotation got %+v", evs)
	}
}

func TestTailerReplacedFileWithoutRotatedCopyJustRestarts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	appendFile(t, p, line("a"))
	tl.Poll()
	os.Remove(p)
	appendFile(t, p, line("z"))
	if evs, _ := tl.Poll(); len(evs) != 1 || evs[0].Model != "z" {
		t.Fatalf("got %+v", evs)
	}
}

func TestLoadFilesSkipsFilesOlderThanCutoffAndKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	old, cur := filepath.Join(dir, "e.jsonl.1"), filepath.Join(dir, "e.jsonl")
	os.WriteFile(old, []byte(line("old")), 0o644)
	os.WriteFile(cur, []byte(line("cur")), 0o644)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)

	evs, err := LoadFiles([]string{old, cur, filepath.Join(dir, "gone")}, time.Time{})
	if err != nil || len(evs) != 2 || evs[0].Model != "old" || evs[1].Model != "cur" {
		t.Fatalf("all: %+v %v", evs, err)
	}
	// line() events are stamped 2026-01-01, so use a cutoff on the file mtime only:
	evs, _ = LoadFiles([]string{old, cur}, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(evs) != 2 {
		t.Fatalf("old cutoff should keep both, got %d", len(evs))
	}
	evs, _ = LoadFiles([]string{old}, time.Now().Add(-24*time.Hour))
	if len(evs) != 0 {
		t.Fatalf("file last written 48h ago must be skipped, got %d", len(evs))
	}
}

// The same inode and the same size but different content: what ext4's inode
// reuse produces after a delete and recreate. Inode comparison alone can't see
// it, so this checks the content-based identity on every OS.
func TestTailerDetectsReplacementWithSameInodeAndSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	appendFile(t, p, line("a"))
	if evs, _ := tl.Poll(); len(evs) != 1 {
		t.Fatalf("setup: %d", len(evs))
	}
	if err := os.WriteFile(p, []byte(line("z")), 0o644); err != nil { // truncates and rewrites the same inode
		t.Fatal(err)
	}
	evs, _ := tl.Poll()
	if len(evs) != 1 || evs[0].Model != "z" {
		t.Fatalf("replacement not noticed: %+v", evs)
	}
	appendFile(t, p, line("y"))
	if evs, _ := tl.Poll(); len(evs) != 1 || evs[0].Model != "y" {
		t.Fatalf("after replacement: %+v", evs)
	}
}

func TestTailerDoesNotRereadWhenFileIsSimplyGrowing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	tl := &Tailer{Path: p}
	appendFile(t, p, line("a")[:10]) // less than one head's worth, and a partial line
	tl.Poll()
	appendFile(t, p, line("a")[10:])
	if evs, _ := tl.Poll(); len(evs) != 1 {
		t.Fatalf("got %d", len(evs))
	}
	for i := 0; i < 5; i++ { // head grows past headBytes; nothing may repeat
		appendFile(t, p, line("m"))
		if evs, _ := tl.Poll(); len(evs) != 1 {
			t.Fatalf("poll %d got %d events", i, len(evs))
		}
	}
	if evs, _ := tl.Poll(); len(evs) != 0 {
		t.Fatalf("idle poll returned %d", len(evs))
	}
}
