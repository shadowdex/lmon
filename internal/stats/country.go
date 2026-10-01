package stats

import (
	"sort"
	"time"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

// CountryRow is the traffic attributed to one country (or one of the two
// pseudo-rows).
type CountryRow struct {
	Label     string   `json:"label"` // "FR", "(country not determined)" or "(no path data)"
	Kind      string   `json:"kind"`  // "country", "undetermined" or "no_path"
	Calls     int      `json:"calls"`
	TokensIn  int      `json:"tokens_in"`  // all input tokens: uncached, cache reads and cache writes
	TokensOut int      `json:"tokens_out"` // output tokens
	BytesUp   int64    `json:"bytes_up"`
	BytesDown int64    `json:"bytes_down"`
	ByteCalls int      `json:"byte_calls"` // calls the byte figures cover (the proxy's; Claude Code sessions have none)
	Providers []string `json:"providers"`
}

// Tokens is input plus output.
func (r CountryRow) Tokens() int { return r.TokensIn + r.TokensOut }

func (r *CountryRow) add(e proxy.Event) {
	r.Calls++
	if e.HasUsage {
		r.TokensIn += e.TotalInput()
		r.TokensOut += e.OutputTokens
	}
	if e.HasBytes() {
		r.BytesUp += e.BytesUp
		r.BytesDown += e.BytesDown
		r.ByteCalls++
	}
	for _, p := range r.Providers {
		if p == e.Provider {
			return
		}
	}
	r.Providers = append(r.Providers, e.Provider)
}

// CountryReport is traffic broken down by the countries on each provider's path.
type CountryReport struct {
	// Rows are the countries (largest first), then the "not determined" and
	// "no path data" pseudo-rows when they have traffic.
	Rows []CountryRow `json:"rows"`
	// Total counts every call once. It is also what continues beyond the
	// provider's edge, which no traceroute can see into. Because a call counts in
	// every country it crosses, the rows add up to more than Total.
	Total CountryRow `json:"total"`
	// Oldest is when the oldest path used was measured (zero if none was).
	Oldest time.Time `json:"oldest_path,omitempty"`
}

// ByCountry attributes each call to the countries on its provider's most
// recently saved path. That is a model: it assumes every call took the route
// that was measured, and it only knows the part of the route up to the
// provider's edge.
func ByCountry(events []proxy.Event, paths map[string]netpath.Summary) CountryReport {
	byISO := map[string]*CountryRow{}
	undetermined := &CountryRow{Label: "(country not determined)", Kind: "undetermined"}
	noPath := &CountryRow{Label: "(no path data)", Kind: "no_path"}
	rep := CountryReport{Total: CountryRow{Label: "(all calls)", Kind: "total"}}

	for _, e := range events {
		rep.Total.add(e)
		sum, ok := paths[usage.UpstreamHost(e.Provider)]
		switch {
		case !ok:
			noPath.add(e)
		case len(sum.Countries) == 0:
			undetermined.add(e)
			noteOldest(&rep, sum)
		default:
			noteOldest(&rep, sum)
			for _, iso := range sum.Countries {
				r := byISO[iso]
				if r == nil {
					r = &CountryRow{Label: iso, Kind: "country"}
					byISO[iso] = r
				}
				r.add(e)
			}
		}
	}

	for _, r := range byISO {
		rep.Rows = append(rep.Rows, *r)
	}
	sort.Slice(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if a.Tokens() != b.Tokens() {
			return a.Tokens() > b.Tokens()
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return a.Label < b.Label
	})
	for _, r := range []*CountryRow{undetermined, noPath} {
		if r.Calls > 0 {
			rep.Rows = append(rep.Rows, *r)
		}
	}
	for i := range rep.Rows {
		sort.Strings(rep.Rows[i].Providers)
	}
	sort.Strings(rep.Total.Providers)
	return rep
}

func noteOldest(rep *CountryReport, s netpath.Summary) {
	if rep.Oldest.IsZero() || s.SavedAt.Before(rep.Oldest) {
		rep.Oldest = s.SavedAt
	}
}
