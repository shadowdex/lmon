package stats

import (
	"testing"
	"time"

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
