package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/stats"
	"github.com/shadowdex/lmon/internal/usage"
)

// loadPaths reads the saved path summaries. A missing file is normal (nothing
// has been traced yet); an unreadable one is reported but never fatal.
func loadPaths() map[string]netpath.Summary {
	paths, err := netpath.Store{Path: netpath.DefaultStorePath()}.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lmon: saved paths:", err)
		return map[string]netpath.Summary{}
	}
	return paths
}

// entersOf is where a provider's traffic enters its network, or "-" if unknown.
func entersOf(paths map[string]netpath.Summary, provider string) string {
	if s, ok := paths[usage.UpstreamHost(provider)]; ok {
		return s.Enters()
	}
	return "-"
}

// printByCountry writes the by-country report as a table, or as JSON.
func printByCountry(w io.Writer, rep stats.CountryReport, havePaths, asJSON bool, now time.Time) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if rep.Total.Calls == 0 {
		fmt.Fprintln(w, "no calls in this window")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COUNTRY\tCALLS\tTOKENS IN\tTOKENS OUT\tBYTES UP\tBYTES DOWN\tPROVIDERS")
	line := func(label string, r stats.CountryRow) {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", label, r.Calls,
			stats.HumanCount(r.TokensIn), stats.HumanCount(r.TokensOut),
			r.BytesCell(r.BytesUp), r.BytesCell(r.BytesDown), strings.Join(r.Providers, ", "))
	}
	for _, r := range rep.Rows {
		line(r.Label, r)
	}
	line("beyond the edge: not observable", rep.Total)
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	if !havePaths {
		fmt.Fprintln(w, "No saved paths yet. Run `lmon path` first, so calls can be attributed to countries.")
	} else {
		age := ""
		if !rep.Oldest.IsZero() {
			age = fmt.Sprintf(" (oldest measured %s)", stats.AgoPhrase(now.Sub(rep.Oldest)))
		}
		fmt.Fprintf(w, "Countries are those observed between you and each provider's edge%s, applied to every call to that provider.\n", age)
		fmt.Fprintln(w, "A call counts in every country it crosses, so the rows add up to more than the total.")
		fmt.Fprintln(w, "The last row is every call: what continues beyond the edge is not observable from here.")
	}
	for _, r := range append([]stats.CountryRow{rep.Total}, rep.Rows...) {
		if r.ByteCalls < r.Calls && r.ByteCalls > 0 {
			fmt.Fprintln(w, "* bytes cover only calls that went through the proxy; Claude Code sessions have none.")
			break
		}
	}
	return nil
}

// seenProviders is the set of providers that have had traffic through the proxy.
type seenProviders struct {
	mu sync.Mutex
	m  map[string]bool
}

func (s *seenProviders) add(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]bool{}
	}
	s.m[p] = true
}

// hosts returns the API hosts of the providers seen so far, sorted.
func (s *seenProviders) hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.m {
		if h := usage.UpstreamHost(p); h != "" {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}
