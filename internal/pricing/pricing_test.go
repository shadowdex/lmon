package pricing

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/usage"
)

func f(x float64) *float64 { return &x }

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %.12f want %.12f", got, want)
	}
}

// sonnet mirrors claude-sonnet-5-5 in the LiteLLM list.
var sonnet = Entry{Provider: "anthropic", partial: partial{
	Input: f(2e-6), Output: f(1e-5), CacheRead: f(2e-7), CacheWrite: f(2.5e-6), CacheWrite1h: f(4e-6)}}

func TestCostAllComponents(t *testing.T) {
	u := usage.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 10000, CacheWriteTokens: 2000, CacheWrite1hTokens: 500}
	// 1000*2e-6 + 500*1e-5 + 10000*2e-7 + 1500*2.5e-6 + 500*4e-6
	// = 0.002 + 0.005 + 0.002 + 0.00375 + 0.002
	near(t, Cost(u, sonnet.Rates(u.TotalInput())), 0.01475)
}

func TestCostClampsImpossible1hBreakdown(t *testing.T) {
	u := usage.Usage{CacheWriteTokens: 100, CacheWrite1hTokens: 999}
	near(t, Cost(u, sonnet.Rates(100)), 100*4e-6) // can't exceed total writes
}

func TestTierAppliesOnlyStrictlyAboveThreshold(t *testing.T) {
	e := Entry{Provider: "anthropic",
		partial: partial{Input: f(3e-6), Output: f(1.5e-5), CacheRead: f(3e-7)},
		Tiers:   []Tier{{Above: 200000, partial: partial{Input: f(6e-6), Output: f(2.25e-5), CacheRead: f(6e-7)}}}}

	over := usage.Usage{InputTokens: 50000, CacheReadTokens: 200000, OutputTokens: 1000} // 250k total
	// 50000*6e-6 + 1000*2.25e-5 + 200000*6e-7 = 0.3 + 0.0225 + 0.12
	near(t, Cost(over, e.Rates(over.TotalInput())), 0.4425)

	edge := usage.Usage{InputTokens: 200000, OutputTokens: 1000} // exactly 200k: not "above"
	near(t, Cost(edge, e.Rates(edge.TotalInput())), 200000*3e-6+1000*1.5e-5)
}

func TestHighestExceededTierWinsAndInheritsUnsetFields(t *testing.T) {
	e := Entry{Provider: "openai",
		partial: partial{Input: f(1e-6), Output: f(2e-6)},
		Tiers: []Tier{
			{Above: 128000, partial: partial{Input: f(2e-6)}},
			{Above: 272000, partial: partial{Input: f(4e-6), Output: f(8e-6)}},
		}}
	r := e.Rates(150000)
	if r.Input != 2e-6 || r.Output != 2e-6 {
		t.Fatalf("128k tier should override input only: %+v", r)
	}
	r = e.Rates(300000)
	if r.Input != 4e-6 || r.Output != 8e-6 {
		t.Fatalf("272k tier: %+v", r)
	}
	if r := e.Rates(1000); r.Input != 1e-6 {
		t.Fatalf("base: %+v", r)
	}
}

func TestMissingCachePricesFallBackToInputPrice(t *testing.T) {
	e := Entry{Provider: "openai", partial: partial{Input: f(1e-6), Output: f(2e-6)}}
	u := usage.Usage{CacheReadTokens: 1000, CacheWriteTokens: 1000, CacheWrite1hTokens: 400}
	near(t, Cost(u, e.Rates(2000)), 2000*1e-6) // no discount or surcharge listed
}

func TestExplicitZeroCacheWriteIsFree(t *testing.T) { // deepseek lists cache_creation = 0.0
	e := Entry{Provider: "deepseek", partial: partial{Input: f(2.8e-7), Output: f(4.2e-7), CacheWrite: f(0)}}
	u := usage.Usage{CacheWriteTokens: 1_000_000}
	near(t, Cost(u, e.Rates(1_000_000)), 0)
}

var lookupTable = &Table{Models: map[string]Entry{
	"claude-sonnet-5-5":                      sonnet,
	"gpt-4o":                                 {Provider: "openai", partial: partial{Input: f(2.5e-6), Output: f(1e-5)}},
	"gpt-4o-mini":                            {Provider: "openai", partial: partial{Input: f(1.5e-7), Output: f(6e-7)}},
	"xai/grok-4.3":                           {Provider: "xai", partial: partial{Input: f(1e-6), Output: f(2e-6)}},
	"deepseek-chat":                          {Provider: "deepseek", partial: partial{Input: f(1e-7), Output: f(2e-7)}},
	"openrouter/anthropic/claude-sonnet-4.5": {Provider: "openrouter", partial: partial{Input: f(3e-6), Output: f(1.5e-5)}},
}}

func TestLookup(t *testing.T) {
	cases := []struct{ prov, model, wantKey string }{
		{"anthropic", "claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"anthropic", "claude-sonnet-5-5-20260101", "claude-sonnet-5-5"}, // snapshot date stripped
		{"openai", "gpt-4o-2024-11-20", "gpt-4o"},
		{"openai", "gpt-4o-mini-2024-07-18", "gpt-4o-mini"}, // must NOT become gpt-4o
		{"xai", "grok-4.3", "xai/grok-4.3"},
		{"deepseek", "deepseek-chat", "deepseek-chat"},
		{"openrouter", "anthropic/claude-sonnet-4.5", "openrouter/anthropic/claude-sonnet-4.5"},
		{"openai", "claude-sonnet-5-5", ""}, // bare key belongs to another provider
		{"openai", "gpt-4", ""},             // never guess by prefix
		{"openai", "gpt-4o-mini-audio", ""},
		{"mystery", "gpt-4o", ""}, // provider without a naming rule
		{"openai", "", ""},
	}
	for _, c := range cases {
		key, _, ok := lookupTable.Lookup(c.prov, c.model)
		if c.wantKey == "" && ok || c.wantKey != "" && (!ok || key != c.wantKey) {
			t.Errorf("Lookup(%q,%q) = %q,%v want %q", c.prov, c.model, key, ok, c.wantKey)
		}
	}
	var nilTable *Table
	if _, ok := nilTable.CostOf("openai", usage.Usage{Model: "gpt-4o"}); ok {
		t.Error("nil table must report unknown, not panic")
	}
}

func TestCostOfUsesModelFromUsage(t *testing.T) {
	c, ok := lookupTable.CostOf("openai", usage.Usage{Model: "gpt-4o-mini", InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok {
		t.Fatal("not found")
	}
	near(t, c, 0.15+0.60)
	if _, ok := lookupTable.CostOf("openai", usage.Usage{Model: "gpt-unknown"}); ok {
		t.Fatal("unknown model must be unpriced")
	}
}

func TestSaveLoadRoundTripKeepsExplicitZeroAndTiers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "prices.json")
	in := &Table{Models: map[string]Entry{
		"m": {Provider: "deepseek", partial: partial{Input: f(1e-6), Output: f(2e-6), CacheWrite: f(0)},
			Tiers: []Tier{{Above: 200000, partial: partial{Input: f(2e-6)}}}},
	}}
	if err := in.Save(p); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil || out == nil {
		t.Fatalf("%v %v", out, err)
	}
	e := out.Models["m"]
	if e.CacheWrite == nil || *e.CacheWrite != 0 {
		t.Fatal("explicit zero lost in round trip (would fall back to input price)")
	}
	if len(e.Tiers) != 1 || *e.Tiers[0].Input != 2e-6 {
		t.Fatalf("tiers: %+v", e.Tiers)
	}
}

func TestLoadMissingIsNilAndCorruptIsError(t *testing.T) {
	dir := t.TempDir()
	if tb, err := Load(filepath.Join(dir, "none.json")); tb != nil || err != nil {
		t.Fatalf("missing: %v %v", tb, err)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "prices update") {
		t.Fatalf("err = %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte(`{"models":{}}`), 0o644)
	if _, err := Load(empty); err == nil {
		t.Fatal("empty table should be an error")
	}
}

func TestFormatUSD(t *testing.T) {
	for in, want := range map[float64]string{0: "$0", 0.00421: "$0.0042", 0.01475: "$0.015", 0.5: "$0.500", 0.99: "$0.990", 1: "$1.00", 12.5: "$12.50", 1234.4: "$1234"} {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%v) = %q want %q", in, got, want)
		}
	}
}

const sampleList = `{
 "sample_spec": {"input_cost_per_token": 0, "litellm_provider": "anthropic", "mode": "chat"},
 "claude-sonnet-x": {"litellm_provider":"anthropic","mode":"chat",
    "input_cost_per_token":3e-6,"output_cost_per_token":1.5e-5,
    "cache_read_input_token_cost":3e-7,"cache_creation_input_token_cost":3.75e-6,
    "cache_creation_input_token_cost_above_1hr":6e-6,
    "input_cost_per_token_above_200k_tokens":6e-6,"output_cost_per_token_above_200k_tokens":2.25e-5,
    "cache_creation_input_token_cost_above_1hr_above_200k_tokens":1.2e-5,
    "input_cost_per_token_batches":1.5e-6,"input_cost_per_token_priority":9e-6,
    "input_cost_per_token_above_200k_tokens_batches":3e-6,
    "search_context_cost_per_query":{"search_context_size_low":0.01}},
 "gpt-x": {"litellm_provider":"openai","mode":"responses","input_cost_per_token":1e-6,"output_cost_per_token":2e-6,
    "input_cost_per_token_above_272k_tokens":2e-6,"input_cost_per_token_above_128k_tokens":1.5e-6,
    "max_tokens":"not a cost field"},
 "dall-e": {"litellm_provider":"openai","mode":"image_generation","input_cost_per_token":1e-6,"output_cost_per_token":2e-6},
 "bedrock-claude": {"litellm_provider":"bedrock","mode":"chat","input_cost_per_token":1e-6,"output_cost_per_token":2e-6},
 "no-output": {"litellm_provider":"openai","mode":"chat","input_cost_per_token":1e-6},
 "weird": {"litellm_provider":"openai","mode":"chat","input_cost_per_token":"oops","output_cost_per_token":2e-6},
 "negative": {"litellm_provider":"openai","mode":"chat","input_cost_per_token":-1,"output_cost_per_token":2e-6},
 "xai/grok-x": {"litellm_provider":"xai","mode":"chat","input_cost_per_token":5e-6,"output_cost_per_token":1e-5}
}`

func TestReduceFiltersAndParsesTiers(t *testing.T) {
	tb, err := Reduce([]byte(sampleList))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"sample_spec", "dall-e", "bedrock-claude", "no-output", "weird", "negative"} {
		if _, ok := tb.Models[gone]; ok {
			t.Errorf("%s should have been filtered out", gone)
		}
	}
	if len(tb.Models) != 3 {
		t.Fatalf("models = %v", len(tb.Models))
	}
	c := tb.Models["claude-sonnet-x"]
	if *c.Input != 3e-6 || *c.CacheWrite1h != 6e-6 || *c.CacheRead != 3e-7 {
		t.Fatalf("base: %+v", c.partial)
	}
	if len(c.Tiers) != 1 || c.Tiers[0].Above != 200000 || *c.Tiers[0].Input != 6e-6 || *c.Tiers[0].CacheWrite1h != 1.2e-5 {
		t.Fatalf("tiers (batches/priority variants must be ignored): %+v", c.Tiers)
	}
	g := tb.Models["gpt-x"]
	if len(g.Tiers) != 2 || g.Tiers[0].Above != 128000 || g.Tiers[1].Above != 272000 {
		t.Fatalf("tiers must be ascending: %+v", g.Tiers)
	}
	// End to end through the reduced table, priced above the 200k tier:
	u := usage.Usage{Model: "claude-sonnet-x", InputTokens: 300000, OutputTokens: 1000}
	cost, ok := tb.CostOf("anthropic", u)
	if !ok {
		t.Fatal("lookup failed")
	}
	near(t, cost, 300000*6e-6+1000*2.25e-5)
}

func TestReduceRejectsGarbageAndEmpty(t *testing.T) {
	if _, err := Reduce([]byte("<html>")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Reduce([]byte(`{"x":{"litellm_provider":"bedrock","mode":"chat","input_cost_per_token":1,"output_cost_per_token":1}}`)); err == nil {
		t.Fatal("nothing priceable should be an error")
	}
}

func TestUpdateWritesAtomicallyAndKeepsOldTableOnFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sampleList)) }))
	defer good.Close()
	dest := filepath.Join(t.TempDir(), "prices.json")
	tb, err := Update(context.Background(), UpdateOptions{URL: good.URL, Dest: dest})
	if err != nil || len(tb.Models) != 3 || tb.Source != good.URL || tb.Updated.IsZero() {
		t.Fatalf("update: %+v %v", tb, err)
	}
	loaded, err := Load(dest)
	if err != nil || len(loaded.Models) != 3 {
		t.Fatalf("reload: %v %v", loaded, err)
	}
	before, _ := os.ReadFile(dest)

	for name, h := range map[string]http.HandlerFunc{
		"404":     http.NotFound,
		"garbage": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) },
		"empty":   func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) },
	} {
		srv := httptest.NewServer(h)
		if _, err := Update(context.Background(), UpdateOptions{URL: srv.URL, Dest: dest}); err == nil {
			t.Errorf("%s: expected error", name)
		}
		srv.Close()
		after, _ := os.ReadFile(dest)
		if string(after) != string(before) {
			t.Errorf("%s: existing table was modified", name)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".prices-*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Validated against Claude Code's own cost-state: claude-haiku-4-5 with 131747
// input and 3435 output tokens plus 4 web searches recorded costUSD 0.188922.
func TestWebSearchIsBilledPerQuery(t *testing.T) {
	tb, err := Reduce([]byte(`{"claude-haiku-4-5-20251001":{"litellm_provider":"anthropic","mode":"chat",
		"input_cost_per_token":1e-6,"output_cost_per_token":5e-6,
		"search_context_cost_per_query":{"search_context_size_high":0.01,"search_context_size_low":0.01,"search_context_size_medium":0.01}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tb.CostOf("anthropic", usage.Usage{Model: "claude-haiku-4-5-20251001", InputTokens: 131747, OutputTokens: 3435, WebSearchRequests: 4})
	if !ok {
		t.Fatal("not found")
	}
	near(t, got, 0.188922)
}

func TestSearchPricePicksMediumElseCheapest(t *testing.T) {
	if p := searchPrice(map[string]any{"search_context_cost_per_query": map[string]any{"search_context_size_low": 0.03, "search_context_size_medium": 0.05, "search_context_size_high": 0.08}}); p == nil || *p != 0.05 {
		t.Fatalf("medium: %v", p)
	}
	if p := searchPrice(map[string]any{"search_context_cost_per_query": map[string]any{"search_context_size_high": 0.08, "search_context_size_low": 0.03}}); p == nil || *p != 0.03 {
		t.Fatalf("cheapest: %v", p)
	}
	if searchPrice(map[string]any{}) != nil || searchPrice(map[string]any{"search_context_cost_per_query": "x"}) != nil {
		t.Fatal("absent or malformed must be nil")
	}
}

func TestReloaderPicksUpNewTableAndKeepsLastGoodOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prices.json")
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Reloader{Path: p, Every: 30 * time.Second, now: func() time.Time { return clock }}
	if r.Get() != nil {
		t.Fatal("no file yet: expected nil")
	}
	write := func(models int, mod time.Time) {
		m := map[string]Entry{}
		for i := 0; i < models; i++ {
			m[fmt.Sprintf("m%d", i)] = Entry{Provider: "openai", partial: partial{Input: f(1), Output: f(1)}}
		}
		if err := (&Table{Models: m}).Save(p); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mod, mod)
	}
	write(1, clock)
	clock = clock.Add(31 * time.Second)
	if tb := r.Get(); tb == nil || len(tb.Models) != 1 {
		t.Fatalf("first load: %+v", tb)
	}
	write(3, clock.Add(time.Minute)) // `prices update` replaced the file
	clock = clock.Add(5 * time.Second)
	if tb := r.Get(); len(tb.Models) != 1 {
		t.Fatal("must not re-stat within the Every interval")
	}
	clock = clock.Add(31 * time.Second)
	if tb := r.Get(); len(tb.Models) != 3 {
		t.Fatalf("new table not picked up: %d", len(tb.Models))
	}
	os.WriteFile(p, []byte("{corrupt"), 0o644)
	os.Chtimes(p, clock.Add(time.Hour), clock.Add(time.Hour))
	clock = clock.Add(31 * time.Second)
	if tb := r.Get(); tb == nil || len(tb.Models) != 3 {
		t.Fatal("a corrupt file must keep serving the last good table")
	}
	os.Remove(p)
	clock = clock.Add(31 * time.Second)
	if r.Get() != nil {
		t.Fatal("deleted file: costs should switch off")
	}
}
