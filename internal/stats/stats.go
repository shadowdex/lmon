// Package stats aggregates recorded events into per-provider/model summaries.
package stats

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"time"

	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/proxy"
)

type Row struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Calls      int    `json:"calls"`
	Errors     int    `json:"errors"`
	Input      int    `json:"input_tokens"`
	Output     int    `json:"output_tokens"`
	CacheRead  int    `json:"cache_read_tokens"`
	CacheWrite int    `json:"cache_write_tokens"`
	// CacheHitRate is cache-read tokens / total input tokens.
	CacheHitRate float64 `json:"cache_hit_rate"`
	AvgTotalMs   float64 `json:"avg_total_ms"`
	P50TotalMs   float64 `json:"p50_total_ms"`
	P95TotalMs   float64 `json:"p95_total_ms"`
	AvgTTFBMs    float64 `json:"avg_ttfb_ms"`
	// LatencyCalls is how many calls the latency figures are based on. Events
	// imported from other tools have none, so this can be less than Calls (or 0).
	LatencyCalls int `json:"latency_calls"`

	// Estimated cost at standard rates; zero unless a price table was given.
	CostUSD     float64 `json:"cost_usd"`
	PricedCalls int     `json:"priced_calls"`
	// UnpricedCalls are successful calls that couldn't be costed (model not in
	// the table, or the response carried no usage). Failed calls cost nothing
	// and are not counted here.
	UnpricedCalls int `json:"unpriced_calls"`
}

// CostLabel is the display form of the cost: "$1.23", "$1.23*" when some calls
// couldn't be priced (so the figure is a lower bound), "n/a" when none could,
// and "-" when there was nothing to price.
func (r Row) CostLabel() string {
	switch {
	case r.PricedCalls == 0 && r.UnpricedCalls > 0:
		return "n/a"
	case r.PricedCalls == 0:
		return "-"
	case r.UnpricedCalls > 0:
		return pricing.FormatUSD(r.CostUSD) + "*"
	}
	return pricing.FormatUSD(r.CostUSD)
}

// PriceEvent prices one event. billable is false for failed calls that
// returned no usage, which are free and shouldn't count as "unpriced".
func PriceEvent(tbl *pricing.Table, e proxy.Event) (usd float64, priced, billable bool) {
	if tbl == nil {
		return 0, false, false
	}
	if !e.HasUsage {
		return 0, false, e.Status < 400
	}
	usd, priced = tbl.CostOf(e.Provider, e.Usage)
	return usd, priced, true
}

// Load reads JSONL events, keeping those at or after since (zero = all).
func Load(r io.Reader, since time.Time) ([]proxy.Event, error) {
	var out []proxy.Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e proxy.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // tolerate a torn/partial line
		}
		if e.Time.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

type acc struct {
	Row
	totals, ttfbs []float64
}

// Aggregate groups events by provider and model.
func Aggregate(events []proxy.Event) []Row { return AggregateWithPrices(events, nil) }

// AggregateWithPrices is Aggregate plus cost estimates from tbl (nil = none).
func AggregateWithPrices(events []proxy.Event, tbl *pricing.Table) []Row {
	m := map[[2]string]*acc{}
	for _, e := range events {
		model := e.Model
		if model == "" {
			model = "(unknown)"
		}
		k := [2]string{e.Provider, model}
		a := m[k]
		if a == nil {
			a = &acc{Row: Row{Provider: e.Provider, Model: model}}
			m[k] = a
		}
		a.Calls++
		if e.Status >= 400 {
			a.Errors++
		}
		a.Input += e.InputTokens
		a.Output += e.OutputTokens
		a.CacheRead += e.CacheReadTokens
		a.CacheWrite += e.CacheWriteTokens
		if usd, priced, billable := PriceEvent(tbl, e); priced {
			a.CostUSD += usd
			a.PricedCalls++
		} else if billable {
			a.UnpricedCalls++
		}
		if e.HasLatency() {
			a.LatencyCalls++
			a.totals = append(a.totals, e.TotalMs)
			a.ttfbs = append(a.ttfbs, e.TTFBMs)
		}
	}
	rows := make([]Row, 0, len(m))
	for _, a := range m {
		if tot := a.Input + a.CacheRead + a.CacheWrite; tot > 0 {
			a.CacheHitRate = float64(a.CacheRead) / float64(tot)
		}
		a.AvgTotalMs, a.P50TotalMs, a.P95TotalMs = mean(a.totals), pct(a.totals, 50), pct(a.totals, 95)
		a.AvgTTFBMs = mean(a.ttfbs)
		rows = append(rows, a.Row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Model < rows[j].Model
	})
	return rows
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// pct is the nearest-rank percentile.
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(p/100*float64(len(s))+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

// Percentile is the nearest-rank percentile (p in 0-100) of v.
func Percentile(v []float64, p float64) float64 { return pct(v, p) }

// LoadFiles reads several JSONL logs (oldest first, as returned by
// proxy.LogFiles). A file whose last write is before since can't contain
// qualifying events, so it is skipped without being read.
func LoadFiles(paths []string, since time.Time) ([]proxy.Event, error) {
	var all []proxy.Event
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // rotated away between listing and opening
			}
			return nil, err
		}
		if st, err := f.Stat(); err == nil && !since.IsZero() && st.ModTime().Before(since) {
			f.Close()
			continue
		}
		evs, err := Load(f, since)
		f.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	return all, nil
}

// Merge combines proxy-observed events with events imported from another tool.
// When both saw the same call (same non-empty response ID, e.g. Claude Code
// run through the proxy) the proxy's copy wins because it has latency, and the
// imported one is dropped so the call is counted once.
func Merge(observed, imported []proxy.Event) []proxy.Event {
	seen := make(map[string]bool, len(observed))
	for _, e := range observed {
		if e.ID != "" {
			seen[e.ID] = true
		}
	}
	out := append([]proxy.Event(nil), observed...)
	for _, e := range imported {
		if e.ID != "" && seen[e.ID] {
			continue
		}
		out = append(out, e)
	}
	return out
}
