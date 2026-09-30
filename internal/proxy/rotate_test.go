package proxy_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
)

func countEvents(t *testing.T, path string) map[string]int {
	t.Helper()
	evs, err := stats.LoadFiles(proxy.LogFiles(path), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range evs {
		seen[e.Path]++
	}
	return seen
}

func TestRotationKeepsEveryEventWhenKeepIsLargeEnough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lg, err := proxy.NewRotatingLogger(path, proxy.Rotation{MaxBytes: 2000, Keep: 50})
	if err != nil {
		t.Fatal(err)
	}
	lg.OnError = func(err error) { t.Errorf("unexpected: %v", err) }

	const workers, per = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				lg.Write(proxy.Event{Time: time.Now(), Provider: "anthropic", Path: fmt.Sprintf("/w%d-%d", w, i), Status: 200})
			}
		}(w)
	}
	wg.Wait()
	lg.Close()

	files := proxy.LogFiles(path)
	if len(files) < 3 {
		t.Fatalf("expected several rotated files, got %v", files)
	}
	for _, f := range files {
		if st, _ := os.Stat(f); st.Size() > 2000+400 { // max plus at most one line
			t.Errorf("%s is %d bytes, over the limit", f, st.Size())
		}
	}
	seen := countEvents(t, path)
	if len(seen) != workers*per {
		t.Fatalf("lost or duplicated events: %d unique of %d", len(seen), workers*per)
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("%s appears %d times", p, n)
		}
	}
}

func TestRotationDeletesOldestBeyondKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lg, _ := proxy.NewRotatingLogger(path, proxy.Rotation{MaxBytes: 500, Keep: 2})
	for i := 0; i < 100; i++ {
		lg.Write(proxy.Event{Time: time.Now(), Provider: "p", Path: fmt.Sprintf("/%03d", i)})
	}
	lg.Close()

	files := proxy.LogFiles(path)
	if len(files) != 3 { // .2, .1 and the active file
		t.Fatalf("files = %v", files)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal(".3 should never exist with Keep=2")
	}
	seen := countEvents(t, path)
	if len(seen) >= 100 || len(seen) == 0 {
		t.Fatalf("expected the oldest events dropped, have %d", len(seen))
	}
	if seen["/099"] != 1 {
		t.Fatal("newest event must survive")
	}
}

func TestRotationDisabledGrowsOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lg, _ := proxy.NewRotatingLogger(path, proxy.Rotation{MaxBytes: 0, Keep: 3})
	for i := 0; i < 200; i++ {
		lg.Write(proxy.Event{Time: time.Now(), Provider: "p", Path: "/x"})
	}
	lg.Close()
	if files := proxy.LogFiles(path); len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
}

// If the rename can't happen, events must keep landing in the active file.
func TestFailedRotationStillAppendsAndReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	blocker := path + ".1" // Keep=1 makes the logger try to remove this; it is a non-empty dir
	if err := os.MkdirAll(filepath.Join(blocker, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	lg, _ := proxy.NewRotatingLogger(path, proxy.Rotation{MaxBytes: 300, Keep: 1})
	var errs []error
	lg.OnError = func(err error) { errs = append(errs, err) }

	for i := 0; i < 40; i++ {
		lg.Write(proxy.Event{Time: time.Now(), Provider: "p", Path: fmt.Sprintf("/%02d", i)})
	}
	lg.Close()

	if len(errs) == 0 {
		t.Fatal("expected the rotation failure to be reported")
	}
	if len(errs) > 10 {
		t.Fatalf("failure retried on every write (%d reports); expected backoff", len(errs))
	}
	evs, _ := stats.LoadFiles([]string{path}, time.Time{})
	if len(evs) != 40 {
		t.Fatalf("events lost while rotation was failing: %d/40", len(evs))
	}
}

func TestLogFilesOrderAndIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	for _, n := range []string{"events.jsonl", "events.jsonl.1", "events.jsonl.2", "events.jsonl.10", "events.jsonl.bak", "events.jsonl.0", "other.jsonl.1"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	var got []string
	for _, f := range proxy.LogFiles(path) {
		got = append(got, filepath.Base(f))
	}
	want := []string{"events.jsonl.10", "events.jsonl.2", "events.jsonl.1", "events.jsonl"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
