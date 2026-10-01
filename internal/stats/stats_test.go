package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

func priceTable(t *testing.T) *pricing.Table {
	t.Helper()
	tb, err := pricing.Reduce([]byte(`{"gpt-x":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`))
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func call(model string, status int, hasUsage bool, in, out int) proxy.Event {
	return proxy.Event{Time: time.Now(), Provider: "openai", Status: status, HasUsage: hasUsage,
		Usage: usage.Usage{Model: model, InputTokens: in, OutputTokens: out}}
}

func TestCostLabelStates(t *testing.T) {
	tb := priceTable(t)
	cases := []struct {
		name string
		evs  []proxy.Event
		want string
	}{
		{"all priced", []proxy.Event{call("gpt-x", 200, true, 1_000_000, 500_000)}, "$2.00"}, // 1.00 + 1.00
		{"unknown model", []proxy.Event{call("gpt-mystery", 200, true, 10, 10)}, "n/a"},
		{"partial is a lower bound", []proxy.Event{call("gpt-x", 200, true, 1_000_000, 0), call("gpt-x", 200, false, 0, 0)}, "$1.00*"},
		{"failed calls are free, not unpriced", []proxy.Event{call("gpt-x", 200, true, 1_000_000, 0), call("gpt-x", 429, false, 0, 0), call("gpt-x", 500, false, 0, 0)}, "$1.00"},
		{"only failures", []proxy.Event{call("gpt-x", 429, false, 0, 0)}, "-"},
		{"usage missing on success (e.g. streaming w/o include_usage)", []proxy.Event{call("gpt-x", 200, false, 0, 0)}, "n/a"},
	}
	for _, c := range cases {
		rows := AggregateWithPrices(c.evs, tb)
		if len(rows) != 1 || rows[0].CostLabel() != c.want {
			t.Errorf("%s: got %q (%+v), want %q", c.name, rows[0].CostLabel(), rows[0], c.want)
		}
	}
}

func TestAggregateWithoutTableHasNoCost(t *testing.T) {
	rows := Aggregate([]proxy.Event{call("gpt-x", 200, true, 100, 100)})
	if rows[0].CostUSD != 0 || rows[0].PricedCalls != 0 || rows[0].UnpricedCalls != 0 || rows[0].CostLabel() != "-" {
		t.Fatalf("%+v", rows[0])
	}
}

func TestLatencyIgnoresImportedEvents(t *testing.T) {
	proxied := proxy.Event{Time: time.Now(), Provider: "anthropic", Status: 200, TotalMs: 1000, TTFBMs: 200, HasUsage: true, Usage: usage.Usage{Model: "m", InputTokens: 1}}
	imported := proxy.Event{Time: time.Now(), Provider: "anthropic", Source: "claude-code", Status: 200, HasUsage: true, Usage: usage.Usage{Model: "m", InputTokens: 1}}
	rows := Aggregate([]proxy.Event{proxied, imported, imported})
	r := rows[0]
	if r.Calls != 3 || r.LatencyCalls != 1 || r.AvgTotalMs != 1000 || r.P50TotalMs != 1000 || r.AvgTTFBMs != 200 {
		t.Fatalf("zeros from imported events leaked into latency: %+v", r)
	}
	only := Aggregate([]proxy.Event{imported})[0]
	if only.LatencyCalls != 0 || only.AvgTotalMs != 0 {
		t.Fatalf("%+v", only)
	}
}

func TestMergePrefersProxyCopyAndKeepsUnmatched(t *testing.T) {
	mk := func(id, src string) proxy.Event {
		return proxy.Event{Source: src, Usage: usage.Usage{ID: id, Model: "m"}}
	}
	got := Merge(
		[]proxy.Event{mk("msg_1", ""), mk("", "")},
		[]proxy.Event{mk("msg_1", "claude-code"), mk("msg_2", "claude-code"), mk("", "claude-code")},
	)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID+"/"+e.Source)
	}
	want := "msg_1/ / msg_2/claude-code /claude-code"
	if strings.Join(ids, " ") != want {
		t.Fatalf("got %q want %q", strings.Join(ids, " "), want)
	}
}

func TestBytesAreSummedOnlyOverEventsThatHaveThem(t *testing.T) {
	proxied := proxy.Event{Time: time.Now(), Provider: "anthropic", Status: 200, BytesUp: 1000, BytesDown: 5000, Usage: usage.Usage{Model: "m", InputTokens: 1}, HasUsage: true}
	imported := proxy.Event{Time: time.Now(), Provider: "anthropic", Source: "claude-code", Status: 200, Usage: usage.Usage{Model: "m", InputTokens: 1}, HasUsage: true}
	r := Aggregate([]proxy.Event{proxied, proxied, imported})[0]
	if r.BytesUp != 2000 || r.BytesDown != 10000 || r.ByteCalls != 2 || r.Calls != 3 {
		t.Fatalf("%+v", r)
	}
	if only := Aggregate([]proxy.Event{imported})[0]; only.ByteCalls != 0 || only.BytesUp != 0 {
		t.Fatalf("imported events have no bytes: %+v", only)
	}
}

func call2(prov, model string, in, out int, bytesUp, bytesDown int64, src string) proxy.Event {
	return proxy.Event{Time: time.Now(), Provider: prov, Source: src, Status: 200, HasUsage: true,
		BytesUp: bytesUp, BytesDown: bytesDown,
		Usage: usage.Usage{Model: model, InputTokens: in, OutputTokens: out}}
}

func sums(t *testing.T) map[string]netpath.Summary {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	return map[string]netpath.Summary{
		"api.anthropic.com": {Host: "api.anthropic.com", Countries: []string{"FR"}, SavedAt: t0},
		"api.openai.com":    {Host: "api.openai.com", Countries: []string{"FR", "GB"}, SavedAt: t0.Add(-2 * time.Hour)},
		"api.x.ai":          {Host: "api.x.ai", Countries: nil, SavedAt: t0.Add(time.Hour)},
	}
}

func TestByCountryAttributesToEveryCountryOnTheProvidersPath(t *testing.T) {
	events := []proxy.Event{
		call2("anthropic", "claude-x", 100, 10, 1000, 5000, ""),
		call2("anthropic", "claude-x", 200, 20, 2000, 6000, ""),
		call2("openai", "gpt-x", 50, 5, 300, 400, ""),
	}
	rep := ByCountry(events, sums(t))

	if rep.Total.Calls != 3 || rep.Total.TokensIn != 350 || rep.Total.TokensOut != 35 || rep.Total.BytesUp != 3300 || rep.Total.BytesDown != 11400 {
		t.Fatalf("total counts each call once: %+v", rep.Total)
	}
	fr, gb := rep.Rows[0], rep.Rows[1]
	if fr.Label != "FR" || fr.Calls != 3 || fr.TokensIn != 350 || fr.TokensOut != 35 || fr.BytesUp != 3300 || fr.BytesDown != 11400 || fr.ByteCalls != 3 {
		t.Fatalf("FR is on both providers' paths: %+v", fr)
	}
	if gb.Label != "GB" || gb.Calls != 1 || gb.TokensIn != 50 || strings.Join(gb.Providers, ",") != "openai" {
		t.Fatalf("GB only on openai's path: %+v", gb)
	}
	if strings.Join(fr.Providers, ",") != "anthropic,openai" {
		t.Fatalf("providers: %v", fr.Providers)
	}
	// FR + GB counts the openai call twice: the rows exceed the total, by design
	if fr.Calls+gb.Calls <= rep.Total.Calls {
		t.Error("rows must add up to more than the total when a call crosses several countries")
	}
	if want := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC); !rep.Oldest.Equal(want) {
		t.Errorf("oldest path used: %v, want %v", rep.Oldest, want)
	}
}

func TestByCountryPseudoRowsForUndeterminedAndMissingPaths(t *testing.T) {
	events := []proxy.Event{
		call2("xai", "grok", 10, 1, 0, 0, ""),                  // path seen, no country determined
		call2("groq", "llama", 20, 2, 0, 0, ""),                // never traced
		call2("anthropic", "c", 30, 3, 0, 0, ""),               // FR
		{Time: time.Now(), Provider: "anthropic", Status: 429}, // a failed call: a call, no tokens
	}
	rep := ByCountry(events, sums(t))
	got := map[string]CountryRow{}
	for _, r := range rep.Rows {
		got[r.Label] = r
	}
	if got["(country not determined)"].Calls != 1 || got["(country not determined)"].TokensIn != 10 {
		t.Fatalf("%+v", got["(country not determined)"])
	}
	if got["(no path data)"].Calls != 1 || got["(no path data)"].TokensIn != 20 {
		t.Fatalf("%+v", got["(no path data)"])
	}
	if fr := got["FR"]; fr.Calls != 2 || fr.TokensIn != 30 {
		t.Fatalf("a failed call counts as a call but adds no tokens: %+v", fr)
	}
	// pseudo-rows come last, after real countries
	if n := len(rep.Rows); rep.Rows[n-2].Kind != "undetermined" || rep.Rows[n-1].Kind != "no_path" {
		t.Fatalf("order: %+v", rep.Rows)
	}
	if rep.Total.Calls != 4 {
		t.Fatalf("total: %+v", rep.Total)
	}
}

func TestByCountryBytesOnlyFromProxiedCalls(t *testing.T) {
	events := []proxy.Event{
		call2("anthropic", "c", 100, 10, 500, 900, ""),
		call2("anthropic", "c", 100, 10, 0, 0, "claude-code"), // imported: tokens but no bytes
	}
	fr := ByCountry(events, sums(t)).Rows[0]
	if fr.Calls != 2 || fr.TokensIn != 200 || fr.BytesUp != 500 || fr.BytesDown != 900 || fr.ByteCalls != 1 {
		t.Fatalf("%+v", fr)
	}
}

func TestByCountryNothingToReport(t *testing.T) {
	rep := ByCountry(nil, nil)
	if len(rep.Rows) != 0 || rep.Total.Calls != 0 || !rep.Oldest.IsZero() {
		t.Fatalf("%+v", rep)
	}
	// ordering is deterministic: more tokens first, ties broken by calls then name
	paths := map[string]netpath.Summary{"api.anthropic.com": {Countries: []string{"DE", "FR", "GB"}}}
	rows := ByCountry([]proxy.Event{call2("anthropic", "c", 1, 1, 0, 0, "")}, paths).Rows
	if rows[0].Label != "DE" || rows[1].Label != "FR" || rows[2].Label != "GB" {
		t.Fatalf("ties by name: %+v", rows)
	}
}
