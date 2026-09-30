package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// DefaultSource is LiteLLM's community-maintained price list (MIT licensed).
const (
	DefaultSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	Attribution   = "Prices from LiteLLM's model price list (MIT)"
	maxDownload   = 64 << 20
)

var (
	// Only providers lmon has adapters for, and only token-billed text modes.
	keepProvider = map[string]bool{
		"anthropic": true, "openai": true, "text-completion-openai": true, "xai": true,
		"groq": true, "mistral": true, "deepseek": true, "openrouter": true,
	}
	keepMode = map[string]bool{"chat": true, "responses": true, "completion": true}

	// e.g. input_cost_per_token_above_200k_tokens. Suffixed variants such as
	// _priority, _batches and _flex don't match because of the $ anchor.
	tierField = regexp.MustCompile(`^(input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost|cache_creation_input_token_cost_above_1hr)_above_(\d+)k_tokens$`)
)

func num(m map[string]any, k string) *float64 {
	if f, ok := m[k].(float64); ok && f >= 0 {
		return &f
	}
	return nil
}

// Reduce extracts the models lmon can price from LiteLLM's JSON. Entries
// without both an input and an output price can't be costed and are skipped.
func Reduce(raw []byte) (*Table, error) {
	var all map[string]map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("not a LiteLLM price list: %w", err)
	}
	t := &Table{Models: map[string]Entry{}}
	for key, m := range all {
		if key == "sample_spec" {
			continue
		}
		prov, _ := m["litellm_provider"].(string)
		mode, _ := m["mode"].(string)
		if !keepProvider[prov] || !keepMode[mode] {
			continue
		}
		e := Entry{Provider: prov, partial: partial{
			Input:        num(m, "input_cost_per_token"),
			Output:       num(m, "output_cost_per_token"),
			CacheRead:    num(m, "cache_read_input_token_cost"),
			CacheWrite:   num(m, "cache_creation_input_token_cost"),
			CacheWrite1h: num(m, "cache_creation_input_token_cost_above_1hr"),
		}}
		if e.Input == nil || e.Output == nil {
			continue
		}
		e.WebSearch = searchPrice(m)
		tiers := map[int]*Tier{}
		for field := range m {
			g := tierField.FindStringSubmatch(field)
			if g == nil {
				continue
			}
			k, _ := strconv.Atoi(g[2])
			above := k * 1000
			tr := tiers[above]
			if tr == nil {
				tr = &Tier{Above: above}
				tiers[above] = tr
			}
			v := num(m, field)
			switch g[1] {
			case "input_cost_per_token":
				tr.Input = v
			case "output_cost_per_token":
				tr.Output = v
			case "cache_read_input_token_cost":
				tr.CacheRead = v
			case "cache_creation_input_token_cost":
				tr.CacheWrite = v
			case "cache_creation_input_token_cost_above_1hr":
				tr.CacheWrite1h = v
			}
		}
		for _, tr := range tiers {
			e.Tiers = append(e.Tiers, *tr)
		}
		sort.Slice(e.Tiers, func(i, j int) bool { return e.Tiers[i].Above < e.Tiers[j].Above })
		t.Models[key] = e
	}
	if len(t.Models) == 0 {
		return nil, fmt.Errorf("no priceable models found in the download")
	}
	return t, nil
}

// searchPrice reads search_context_cost_per_query, which LiteLLM keys by
// context size. Anthropic charges one flat price, so any size is the same; for
// others we use the medium tier, else the cheapest listed.
func searchPrice(m map[string]any) *float64 {
	q, ok := m["search_context_cost_per_query"].(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := q["search_context_size_medium"].(float64); ok && v >= 0 {
		return &v
	}
	var best *float64
	for _, x := range q {
		if v, ok := x.(float64); ok && v >= 0 && (best == nil || v < *best) {
			c := v
			best = &c
		}
	}
	return best
}

type UpdateOptions struct {
	URL    string // default DefaultSource
	Dest   string // default DefaultPath()
	Client *http.Client
	Out    io.Writer
}

// Update downloads the price list, reduces it, and atomically replaces the
// local table. On any failure the existing table is left untouched.
func Update(ctx context.Context, o UpdateOptions) (*Table, error) {
	if o.URL == "" {
		o.URL = DefaultSource
	}
	if o.Dest == "" {
		o.Dest = DefaultPath()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	say := func(f string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, f+"\n", a...)
		}
	}
	say("downloading %s", o.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", o.URL, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, err
	}
	t, err := Reduce(raw)
	if err != nil {
		return nil, err
	}
	t.Updated, t.Source = time.Now().UTC(), o.URL
	if err := t.Save(o.Dest); err != nil {
		return nil, err
	}
	say("saved %d models to %s", len(t.Models), o.Dest)
	say("%s", Attribution)
	return t, nil
}
