// Package pricing turns token usage into estimated dollar cost.
//
// Prices come from a local table (~/.lmon/prices.json) built by `lmon prices
// update` from LiteLLM's community price list (MIT licensed). Nothing is
// compiled in, because list prices change often and a stale built-in table
// would silently give wrong numbers.
//
// Costs are estimates at standard (non-batch, non-priority) rates, computed
// when displayed, so updating the table also reprices old events.
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shadowdex/lmon/internal/usage"
)

// Rates are USD per token with every field resolved (no missing values).
type Rates struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h float64
}

// A field that is nil means "not listed" so it can fall back (see resolve).
type partial struct {
	Input        *float64 `json:"input,omitempty"`
	Output       *float64 `json:"output,omitempty"`
	CacheRead    *float64 `json:"cache_read,omitempty"`
	CacheWrite   *float64 `json:"cache_write,omitempty"`
	CacheWrite1h *float64 `json:"cache_write_1h,omitempty"`
}

// Tier holds the rates that replace the base ones when a request's total input
// exceeds Above tokens. Providers bill the whole request at the higher rate.
type Tier struct {
	Above int `json:"above_tokens"`
	partial
}

type Entry struct {
	Provider string `json:"provider"`
	partial
	Tiers []Tier `json:"tiers,omitempty"` // ascending by Above
}

func override(base *float64, over *float64) *float64 {
	if over != nil {
		return over
	}
	return base
}

func deref(p *float64, fallback float64) float64 {
	if p != nil {
		return *p
	}
	return fallback
}

// Rates returns the rates for a request with totalInput input tokens (cached
// and uncached together). Missing cache prices fall back the way providers
// bill: cached reads and writes cost the normal input price unless a
// discount or surcharge is listed, and 1h writes cost the 5m write price.
func (e Entry) Rates(totalInput int) Rates {
	p := e.partial
	for _, t := range e.Tiers { // ascending: the highest threshold exceeded wins
		if totalInput > t.Above {
			p = partial{
				Input:        override(p.Input, t.Input),
				Output:       override(p.Output, t.Output),
				CacheRead:    override(p.CacheRead, t.CacheRead),
				CacheWrite:   override(p.CacheWrite, t.CacheWrite),
				CacheWrite1h: override(p.CacheWrite1h, t.CacheWrite1h),
			}
		}
	}
	var r Rates
	r.Input = deref(p.Input, 0)
	r.Output = deref(p.Output, 0)
	r.CacheRead = deref(p.CacheRead, r.Input)
	r.CacheWrite = deref(p.CacheWrite, r.Input)
	r.CacheWrite1h = deref(p.CacheWrite1h, r.CacheWrite)
	return r
}

// Cost is the estimated USD cost of one response at the given rates.
func Cost(u usage.Usage, r Rates) float64 {
	w1h := u.CacheWrite1hTokens
	if w1h > u.CacheWriteTokens {
		w1h = u.CacheWriteTokens
	}
	w5m := u.CacheWriteTokens - w1h
	return float64(u.InputTokens)*r.Input +
		float64(u.OutputTokens)*r.Output +
		float64(u.CacheReadTokens)*r.CacheRead +
		float64(w5m)*r.CacheWrite +
		float64(w1h)*r.CacheWrite1h
}

// Table is a loaded price list.
type Table struct {
	Updated time.Time        `json:"updated"`
	Source  string           `json:"source"`
	Models  map[string]Entry `json:"models"`
}

// DefaultPath is ~/.lmon/prices.json.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lmon", "prices.json")
}

// Load reads a table. A missing file returns (nil, nil): costs are optional.
func Load(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w (run `lmon prices update` to rebuild it)", path, err)
	}
	if len(t.Models) == 0 {
		return nil, fmt.Errorf("%s has no models (run `lmon prices update`)", path)
	}
	return &t, nil
}

func (t *Table) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prices-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// How each lmon provider names its models in the LiteLLM list, and which
// litellm_provider values are acceptable for it.
var (
	keyPrefix = map[string]string{
		"anthropic": "", "openai": "", "xai": "xai/", "groq": "groq/",
		"mistral": "mistral/", "deepseek": "deepseek/", "openrouter": "openrouter/",
	}
	allowedProviders = map[string][]string{
		"anthropic":  {"anthropic"},
		"openai":     {"openai", "text-completion-openai"},
		"xai":        {"xai"},
		"groq":       {"groq"},
		"mistral":    {"mistral"},
		"deepseek":   {"deepseek"},
		"openrouter": {"openrouter"},
	}
	datePatterns = []*regexp.Regexp{
		regexp.MustCompile(`-\d{8}$`),
		regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`),
		regexp.MustCompile(`@\d{8}$`),
	}
)

func stripDate(m string) (string, bool) {
	for _, re := range datePatterns {
		if loc := re.FindStringIndex(m); loc != nil {
			return m[:loc[0]], true
		}
	}
	return m, false
}

// Lookup finds the price entry for a model as reported by provider. It tries
// the exact name, then the name without a trailing snapshot date, and never
// guesses by prefix (gpt-4o-mini must not be priced as gpt-4o). It returns the
// table key it matched.
func (t *Table) Lookup(provider, model string) (string, Entry, bool) {
	if t == nil || model == "" {
		return "", Entry{}, false
	}
	prefix, known := keyPrefix[provider]
	if !known {
		return "", Entry{}, false
	}
	names := []string{model}
	if s, ok := stripDate(model); ok {
		names = append(names, s)
	}
	for _, n := range names {
		for _, key := range []string{prefix + n, n} {
			e, ok := t.Models[key]
			if !ok {
				continue
			}
			for _, p := range allowedProviders[provider] {
				if e.Provider == p {
					return key, e, true
				}
			}
		}
	}
	return "", Entry{}, false
}

// CostOf prices one response. ok is false when the model isn't in the table.
func (t *Table) CostOf(provider string, u usage.Usage) (usd float64, ok bool) {
	_, e, found := t.Lookup(provider, u.Model)
	if !found {
		return 0, false
	}
	return Cost(u, e.Rates(u.TotalInput())), true
}

// Age is how long ago the table was built.
func (t *Table) Age() time.Duration { return time.Since(t.Updated) }

// FormatUSD renders a cost compactly: $0.0042, $0.015, $12.50, $1234. Small
// amounts keep more digits because single calls routinely cost fractions of a cent.
func FormatUSD(x float64) string {
	switch {
	case x == 0:
		return "$0"
	case x < 0.01:
		return fmt.Sprintf("$%.4f", x)
	case x < 1:
		return fmt.Sprintf("$%.3f", x)
	case x < 1000:
		return fmt.Sprintf("$%.2f", x)
	}
	return fmt.Sprintf("$%.0f", x)
}

// Suggest lists table keys containing model, for `lmon prices show` hints.
func (t *Table) Suggest(provider, model string, n int) []string {
	prefix := keyPrefix[provider]
	needle := strings.ToLower(model)
	var out []string
	for k, e := range t.Models {
		ok := false
		for _, p := range allowedProviders[provider] {
			ok = ok || e.Provider == p
		}
		if ok && strings.Contains(strings.ToLower(strings.TrimPrefix(k, prefix)), needle) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
