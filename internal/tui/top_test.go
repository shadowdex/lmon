package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shadowdex/lmon/internal/claudecode"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
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

	// Not New(): it polls with the real clock, which would prune events dated
	// around t0 before the test's clock is set.
	m := &Model{
		tailer:   &stats.Tailer{Path: p},
		now:      func() time.Time { return t0 },
		window:   1,
		imported: map[string]proxy.Event{},
	}
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
		if m := New(filepath.Join(t.TempDir(), "x"), c.in, nil, nil, nil); m.window != c.want {
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

func priced(t *testing.T) *pricing.Table {
	t.Helper()
	tb, err := pricing.Reduce([]byte(`{"claude-x":{"litellm_provider":"anthropic","mode":"chat","input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`))
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestCostColumnSummaryAndBurnRate(t *testing.T) {
	m := testModel(
		ev(1*time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 1_000_000}), // $1.00
		ev(3*time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 1_000_000}), // $1.00
	)
	m.prices = priced(t)
	out := plain(m.Render(140))
	for _, want := range []string{"COST", "cost $2.00", "$40.00/h", "$2.00"} { // 2.00 over a 3 minute span = 40/h
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$2.00*") {
		t.Error("fully priced data must not carry the lower-bound marker")
	}
}

func TestUnpricedCallsMarkCostAsLowerBound(t *testing.T) {
	m := testModel(
		ev(time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 1_000_000}),
		ev(time.Minute, "anthropic", "claude-unknown", 200, 100, usage.Usage{InputTokens: 5}),
		ev(time.Minute, "anthropic", "claude-x", 429, 100, usage.Usage{}), // failed: free, must not trigger the marker by itself
	)
	m.prices = priced(t)
	out := plain(m.Render(140))
	if !strings.Contains(out, "cost $1.00*") {
		t.Errorf("expected lower-bound marker in summary:\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Errorf("unknown model row should show n/a:\n%s", out)
	}
}

func TestNoPriceTableMeansNoCostColumnOrSummary(t *testing.T) {
	m := testModel(ev(time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 10}))
	out := plain(m.Render(140))
	if strings.Contains(out, "COST") || strings.Contains(out, "cost $") {
		t.Errorf("cost shown without a table:\n%s", out)
	}
}

func TestBurnRateSpanClamping(t *testing.T) {
	// under a minute of data extrapolates from one minute, not from seconds
	if got := burnRate(1, 5*time.Second, time.Hour); got != "$60.00/h" {
		t.Errorf("short span: %s", got)
	}
	// span can't exceed the window
	if got := burnRate(10, 48*time.Hour, 24*time.Hour); got != "$0.417/h" {
		t.Errorf("long span: %s", got)
	}
	if got := burnRate(6, 30*time.Minute, time.Hour); got != "$12.00/h" {
		t.Errorf("normal: %s", got)
	}
}

func TestCostColumnSurvivesNarrowerThanP50(t *testing.T) {
	m := testModel(ev(time.Minute, "anthropic", "claude-x", 200, 100, usage.Usage{InputTokens: 1_000_000}))
	m.prices = priced(t)
	out := plain(m.table(m.events, t0, 15*time.Minute, 62))
	if !strings.Contains(out, "COST") {
		t.Errorf("COST should outlast P50/IN/ERR/OUT when space is tight:\n%s", out)
	}
	if strings.Contains(out, "P50") {
		t.Errorf("P50 should have been dropped before COST:\n%s", out)
	}
}

func claudeLine(id string, at time.Time, in, out int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": at.Format("2006-01-02T15:04:05.000Z"), "requestId": "req_" + id,
		"message": map[string]any{"id": id, "model": "claude-x", "usage": map[string]any{"input_tokens": in, "output_tokens": out}},
	})
	return string(b) + "\n"
}

// labeledScanner adapts a Claude scanner to the Importer interface.
type labeledScanner struct{ *claudecode.Scanner }

func (labeledScanner) Label() string { return "claude-code" }

func claudeModel(t *testing.T, lines ...string) (*Model, string) {
	t.Helper()
	root := t.TempDir()
	f := filepath.Join(root, "proj", "s.jsonl")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte(strings.Join(lines, "")), 0o644)
	m := &Model{
		tailer:   &stats.Tailer{Path: filepath.Join(root, "no-proxy-log.jsonl")},
		now:      func() time.Time { return t0 },
		window:   1,
		claude:   labeledScanner{&claudecode.Scanner{Roots: []string{root}}},
		imported: map[string]proxy.Event{},
	}
	m.poll()
	return m, f
}

func TestClaudeEventsUpsertAsTheirStreamCompletes(t *testing.T) {
	m, file := claudeModel(t, claudeLine("msg_A", t0.Add(-time.Minute), 10, 16))
	if got := m.all(); len(got) != 1 || got[0].OutputTokens != 16 {
		t.Fatalf("first poll: %+v", got)
	}
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(claudeLine("msg_A", t0.Add(-59*time.Second), 10, 313)) // same response, final output
	f.Close()
	m.poll()
	got := m.all()
	if len(got) != 1 || got[0].OutputTokens != 313 {
		t.Fatalf("the response must be replaced, not added: %+v", got)
	}
	out := plain(m.Render(140))
	if !strings.Contains(out, "1 calls") {
		t.Errorf("one response counted as several calls:\n%s", out)
	}
}

func TestImportedOnlyRowsShowDashForLatency(t *testing.T) {
	m, _ := claudeModel(t, claudeLine("msg_A", t0.Add(-time.Minute), 10, 16))
	out := plain(m.Render(140))
	if strings.Contains(out, "p50") || strings.Contains(out, "0ms") {
		t.Errorf("fake latency shown for calls the proxy never timed:\n%s", out)
	}
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "claude-x") {
			row = l
		}
	}
	if strings.Count(row, " - ") < 2 {
		t.Errorf("latency cells should be dashes: %q", row)
	}
}

func TestCallSeenByProxyAndClaudeIsCountedOnceWithLatency(t *testing.T) {
	m, _ := claudeModel(t, claudeLine("msg_A", t0.Add(-time.Minute), 10, 16))
	px := ev(time.Minute, "anthropic", "claude-x", 200, 1234, usage.Usage{InputTokens: 10, OutputTokens: 16})
	px.ID = "msg_A"
	m.events = []proxy.Event{px}
	if n := len(m.all()); n != 1 {
		t.Fatalf("same response id from both sources counted %d times", n)
	}
	out := plain(m.Render(140))
	if !strings.Contains(out, "1 calls") || !strings.Contains(out, "1.2s") {
		t.Errorf("expected one call with the proxy's latency:\n%s", out)
	}
}

func TestOldImportedEventsArePruned(t *testing.T) {
	m, _ := claudeModel(t, claudeLine("msg_old", t0.Add(-48*time.Hour), 1, 1), claudeLine("msg_new", t0.Add(-time.Minute), 1, 1))
	m.poll()
	if _, stale := m.imported["msg_old"]; stale {
		t.Error("event older than the retention period was kept")
	}
	if _, ok := m.imported["msg_new"]; !ok {
		t.Error("fresh event missing")
	}
}

func TestHeaderFlagsApproximateClaudeSource(t *testing.T) {
	m, _ := claudeModel(t, claudeLine("msg_A", t0.Add(-time.Minute), 1, 1))
	if out := plain(m.Render(120)); !strings.Contains(out, "claude-code (approx.") {
		t.Errorf("claude source not flagged as approximate:\n%s", out)
	}
	if out := plain(testModel().Render(120)); strings.Contains(out, "claude-code") {
		t.Errorf("proxy-only view must not mention claude-code:\n%s", out)
	}
}
