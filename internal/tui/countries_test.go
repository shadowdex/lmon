package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

func TestBarFillsProportionallyWithAMinimumOfOneCell(t *testing.T) {
	for _, c := range []struct {
		frac  float64
		cells int
		want  string
	}{
		{0, 10, "░░░░░░░░░░"}, {0.5, 10, "█████░░░░░"}, {1, 10, "██████████"},
		{0.001, 10, "█░░░░░░░░░"},                       // anything above zero stays visible
		{1.7, 10, "██████████"}, {-3, 10, "░░░░░░░░░░"}, // clamped
		{0.5, 4, "██░░"}, {0.5, 0, ""},
	} {
		if got := plain(bar(c.frac, c.cells)); got != c.want {
			t.Errorf("bar(%v,%d) = %q, want %q", c.frac, c.cells, got, c.want)
		}
	}
}

func TestBarIsAGradientFromBlueToRed(t *testing.T) {
	raw := bar(1, 10)
	if !strings.Contains(raw, "38;2;111;155;240") { // #6f9bf0, the first colour
		t.Errorf("the first cell should be blue: %q", raw)
	}
	if !strings.Contains(raw, "38;2;220;91;82") { // #dc5b52, the last colour
		t.Errorf("the last cell should be red: %q", raw)
	}
	if bar(0.3, 10) == bar(1, 10) {
		t.Error("different fractions must differ")
	}
}

func countryModel(t *testing.T) *Model {
	mk := func(prov, model string, ago time.Duration, in, out int, up, down int64) proxy.Event {
		return proxy.Event{Time: t0.Add(-ago), Provider: prov, Status: 200, HasUsage: true, BytesUp: up, BytesDown: down,
			Usage: usage.Usage{Model: model, InputTokens: in, OutputTokens: out}}
	}
	m := testModel(
		mk("anthropic", "claude-x", time.Minute, 90_000, 4_000, 120_000, 800_000),
		mk("anthropic", "claude-x", 2*time.Minute, 60_000, 3_000, 90_000, 500_000),
		mk("openai", "gpt-x", 3*time.Minute, 20_000, 1_000, 30_000, 100_000),
	)
	m.paths = map[string]netpath.Summary{
		"api.anthropic.com": {Host: "api.anthropic.com", Countries: []string{"FR"}, EdgeKind: "nearby", EdgeISO: "FR", EdgeCity: "Paris", SavedAt: t0.Add(-4 * time.Hour)},
		"api.openai.com":    {Host: "api.openai.com", Countries: []string{"FR", "GB"}, EdgeKind: "nearby", EdgeISO: "GB", EdgeCity: "London", SavedAt: t0.Add(-4 * time.Hour)},
	}
	m.view = ViewCountries
	return m
}

func count(s, sub string) int { return strings.Count(s, sub) }

// flat joins wrapped text back into one line: box borders and line breaks removed.
func flat(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "│", " ")), " ")
}

func TestCountriesPanelShowsCountriesBarsAndTheUnobservableRow(t *testing.T) {
	out := plain(countryModel(t).Render(130))
	text := flat(out)
	for _, want := range []string{
		"lmon top countries", "[c] models",
		"Countries on the path to each provider's edge",
		"COUNTRY", "CALLS", "TOKENS IN", "TOKENS OUT", "BYTES UP", "BYTES DOWN", "PROVIDERS",
		"beyond the edge: not observable",
		"oldest measured 4h ago", "add up to more than the total",
		"anthropic, openai", // FR carries both providers
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	var fr, gb, beyond string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, " FR ") && strings.Contains(l, "█"):
			fr = l
		case strings.Contains(l, " GB ") && strings.Contains(l, "█"):
			gb = l
		case strings.Contains(l, "beyond the edge"):
			beyond = l
		}
	}
	if fr == "" || gb == "" || beyond == "" {
		t.Fatalf("rows missing:\n%s", out)
	}
	if count(fr, "█") != 10 || count(gb, "█") >= count(fr, "█") || count(gb, "█") < 1 {
		t.Errorf("FR is the largest (full bar), GB smaller but visible:\nFR %q\nGB %q", fr, gb)
	}
	if count(beyond, "█") != 0 {
		t.Errorf("the unobservable row has no bar: %q", beyond)
	}
	// FR is on both paths: all 3 calls, 175k+... tokens in; GB only the one openai call
	if !strings.Contains(fr, " 3 ") || !strings.Contains(gb, " 1 ") {
		t.Errorf("call counts: FR %q GB %q", fr, gb)
	}
	if !strings.Contains(fr, "240 kB") || !strings.Contains(fr, "1.4 MB") { // 120+90+30 kB up, 800+500+100 kB down
		t.Errorf("FR bytes should sum all three proxied calls: %q", fr)
	}
}

func TestCountriesPanelNarrowTerminalDropsColumnsButKeepsTheCore(t *testing.T) {
	m := countryModel(t)
	for _, w := range []int{130, 90, 70, 50} {
		out := plain(m.Render(w))
		for i, l := range strings.Split(out, "\n") {
			if n := len([]rune(l)); n > w {
				t.Errorf("width %d: line %d is %d wide: %q", w, i, n, l)
			}
		}
		for _, core := range []string{"COUNTRY", "CALLS", "TOKENS IN"} {
			if !strings.Contains(out, core) {
				t.Errorf("width %d lost %q", w, core)
			}
		}
	}
	if narrow := plain(m.Render(60)); strings.Contains(narrow, "PROVIDERS") {
		t.Error("PROVIDERS should be the first column dropped")
	}
	if wide := plain(m.Render(130)); !strings.Contains(wide, "PROVIDERS") {
		t.Error("PROVIDERS should show when there is room")
	}
}

func TestCountriesPanelWithoutSavedPathsExplainsHowToGetThem(t *testing.T) {
	m := countryModel(t)
	m.paths = map[string]netpath.Summary{}
	out := plain(m.Render(120))
	if !strings.Contains(out, "No saved paths yet") || !strings.Contains(out, "lmon path") || !strings.Contains(out, "(no path data)") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out, "oldest measured") {
		t.Error("no age without paths")
	}
}

func TestKeyCTogglesBetweenModelsAndCountries(t *testing.T) {
	m := countryModel(t)
	m.view = ViewModels
	key := func(s string) tea.Msg { return tea.KeyPressMsg{Code: rune(s[0]), Text: s} }
	if out := flat(plain(m.Render(120))); !strings.Contains(out, "lmon top models") || !strings.Contains(out, "[c] countries") {
		t.Fatalf("starts on models:\n%s", out)
	}
	m.Update(key("c"))
	if m.view != ViewCountries {
		t.Fatal("c should open the countries view")
	}
	if out := flat(plain(m.Render(120))); !strings.Contains(out, "lmon top countries") || strings.Contains(out, "sort") {
		t.Fatalf("sorting does not apply to the countries view:\n%s", out)
	}
	m.Update(key("c"))
	if m.view != ViewModels {
		t.Fatal("c again should return")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.view != ViewCountries {
		t.Fatal("tab should switch too")
	}
}

func TestEntersColumnInTheModelsTableOnlyWhenPathsExist(t *testing.T) {
	m := countryModel(t)
	m.view = ViewModels
	out := plain(m.Render(160))
	if !strings.Contains(out, "ENTERS") || !strings.Contains(out, "FR Paris") || !strings.Contains(out, "GB London") {
		t.Fatalf("each provider's entry point should show:\n%s", out)
	}
	m.paths = nil
	if out := plain(m.Render(160)); strings.Contains(out, "ENTERS") {
		t.Fatalf("no ENTERS column without route data:\n%s", out)
	}
	// and an unknown or untraced provider shows a dash, not a guess
	m.paths = map[string]netpath.Summary{"api.openai.com": {EdgeKind: "nearby", EdgeISO: "GB", EdgeCity: "London"}}
	if out := plain(m.Render(160)); !strings.Contains(out, "-") {
		t.Errorf("anthropic has no path here:\n%s", out)
	}
}

func TestPathsAreLoadedAtStartAndReloadedNoMoreOftenThanEveryFiveSeconds(t *testing.T) {
	calls := 0
	loader := func() map[string]netpath.Summary {
		calls++
		return map[string]netpath.Summary{"api.anthropic.com": {EdgeKind: "nearby", EdgeISO: "FR"}}
	}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m := New(t.TempDir()+"/none.jsonl", 15*time.Minute, nil, nil, loader)
	m.now = func() time.Time { return clock }
	if calls != 1 || len(m.paths) != 1 {
		t.Fatalf("loaded once at start: calls=%d paths=%v", calls, m.paths)
	}
	m.pathsAt = time.Time{} // New used the real clock; start the throttle from our fake one
	m.poll()
	m.poll()
	clock = clock.Add(4 * time.Second)
	m.poll()
	if calls != 2 {
		t.Fatalf("within 5 s nothing reloads: calls=%d", calls)
	}
	clock = clock.Add(2 * time.Second)
	m.poll()
	if calls != 3 {
		t.Fatalf("after 5 s it reloads: calls=%d", calls)
	}
	// an unreadable file (loader returns nil) keeps the last good data
	nilLoader := func() map[string]netpath.Summary { return nil }
	m.pathsFn = nilLoader
	clock = clock.Add(10 * time.Second)
	m.poll()
	if len(m.paths) != 1 {
		t.Fatalf("a failed reload must not wipe the paths: %v", m.paths)
	}
}
