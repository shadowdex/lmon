package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func ev(ago time.Duration, prov, model string, status int, total float64, u usage.Usage) proxy.Event {
	u.Model = model
	return proxy.Event{Time: t0.Add(-ago), Provider: prov, Status: status, TotalMs: total, TTFBMs: total / 4, Usage: u, HasUsage: true}
}

func testModel(evs ...proxy.Event) *Model {
	return &Model{
		tailer: nil,
		events: evs,
		now:    func() time.Time { return t0 },
		window: 1, // 15m
	}
}

func plain(s string) string { return ansi.Strip(s) }

func TestSparkline(t *testing.T) {
	if got := sparkline([]float64{0, 7}); got != "▁█" {
		t.Fatalf("got %q", got)
	}
	if got := sparkline([]float64{-1, 5, -1}); got != " ▅ " {
		t.Fatalf("flat series with gaps: got %q", got)
	}
	if got := sparkline([]float64{3, 3, 3}); got != "▅▅▅" {
		t.Fatalf("no spread: got %q", got)
	}
	if sparkline(nil) != "" {
		t.Fatal("nil")
	}
}

func TestBucketIndexOldestFirstNewestLast(t *testing.T) {
	win := 10 * time.Minute
	if i, ok := bucketIndex(t0, t0, win, 10); !ok || i != 9 {
		t.Fatalf("now -> %d %v", i, ok)
	}
	if i, ok := bucketIndex(t0.Add(-win+time.Second), t0, win, 10); !ok || i != 0 {
		t.Fatalf("oldest -> %d %v", i, ok)
	}
	if _, ok := bucketIndex(t0.Add(-win), t0, win, 10); ok {
		t.Fatal("event at exactly the window edge should be outside")
	}
	if i, ok := bucketIndex(t0.Add(time.Minute), t0, win, 10); !ok || i != 9 {
		t.Fatalf("future clock skew -> %d %v", i, ok)
	}
}

func TestRenderTable(t *testing.T) {
	m := testModel(
		ev(1*time.Minute, "anthropic", "claude-x", 200, 1000, usage.Usage{InputTokens: 10, OutputTokens: 100, CacheReadTokens: 90}),
		ev(2*time.Minute, "anthropic", "claude-x", 200, 3000, usage.Usage{InputTokens: 10, OutputTokens: 100, CacheReadTokens: 90}),
		ev(3*time.Minute, "openai", "gpt-y", 429, 500, usage.Usage{InputTokens: 50, OutputTokens: 5}),
		ev(2*time.Hour, "openai", "too-old", 200, 1, usage.Usage{}), // outside the 15m window
	)
	out := plain(m.Render(120))
	for _, want := range []string{"3 calls", "claude-x", "gpt-y", "HIT%", "TREND", "1 errors", "cache hit 72%"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "too-old") {
		t.Error("event outside the window was shown")
	}
	// claude-x has 2 calls so it sorts above gpt-y (1 call) by default.
	if strings.Index(out, "claude-x") > strings.Index(out, "gpt-y") {
		t.Error("expected calls-descending order")
	}
}

func TestRenderEmptyState(t *testing.T) {
	out := plain(testModel().Render(80))
	if !strings.Contains(out, "Waiting for calls") || !strings.Contains(out, "lmon proxy") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestNarrowWidthDropsColumnsButKeepsKeyOnes(t *testing.T) {
	m := testModel(ev(time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 1, OutputTokens: 1}))
	for _, w := range []int{120, 90, 70, 50} {
		for i, line := range strings.Split(plain(m.table(m.events, t0, 15*time.Minute, w)), "\n") {
			if lipglossWidth(line) > w {
				t.Errorf("width %d: line %d is %d wide: %q", w, i, lipglossWidth(line), line)
			}
		}
	}
	wide := plain(m.table(m.events, t0, 15*time.Minute, 120))
	narrow := plain(m.table(m.events, t0, 15*time.Minute, 70))
	if !strings.Contains(wide, "TREND") || strings.Contains(narrow, "TREND") {
		t.Error("TREND should exist when wide and be dropped first when narrow")
	}
	if !strings.Contains(narrow, "P95") || !strings.Contains(narrow, "HIT%") {
		t.Error("key columns must survive narrow widths")
	}
}

func TestKeysCycleWindowSortPauseQuit(t *testing.T) {
	m := testModel()
	key := func(s string) tea.Msg { return tea.KeyPressMsg{Code: rune(s[0]), Text: s} }

	m.Update(key("w"))
	if m.window != 2 {
		t.Fatalf("window = %d", m.window)
	}
	m.Update(key("s"))
	if sortKeys[m.sortBy] != "tokens" {
		t.Fatalf("sort = %s", sortKeys[m.sortBy])
	}
	m.Update(key("p"))
	if !m.paused || !strings.Contains(plain(m.Render(80)), "PAUSED") {
		t.Fatal("pause not shown")
	}
	_, cmd := m.Update(key("q"))
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatal("q did not produce QuitMsg")
	}
}

func TestPollReadsLogAndPrunesOldEvents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	lg, _ := proxy.NewLogger(p)
	lg.Write(ev(48*time.Hour, "anthropic", "ancient", 200, 1, usage.Usage{}))
	lg.Write(ev(time.Minute, "anthropic", "fresh", 200, 1, usage.Usage{}))
	lg.Close()

	m := New(p, 15*time.Minute)
	m.now = func() time.Time { return t0 }
	m.poll()
	if len(m.events) != 1 || m.events[0].Model != "fresh" {
		t.Fatalf("events = %+v", m.events)
	}
}

func TestNewPicksSmallestWindowAtLeastRequested(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want int
	}{{time.Minute, 0}, {15 * time.Minute, 1}, {20 * time.Minute, 2}, {999 * time.Hour, 3}} {
		if m := New(filepath.Join(t.TempDir(), "x"), c.in); m.window != c.want {
			t.Errorf("window(%s) = %d, want %d", c.in, m.window, c.want)
		}
	}
}

func TestHumanAndFmt(t *testing.T) {
	cases := map[string]string{human(999): "999", human(1500): "1.5k", human(25000): "25k", human(2_500_000): "2.5M", fmtMs(450): "450ms", fmtMs(1500): "1.5s"}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func lipglossWidth(s string) int { return len([]rune(s)) }
