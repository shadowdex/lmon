package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
	"github.com/shadowdex/lmon/internal/usage"
)

// View selects what `top` shows.
type View int

const (
	ViewModels View = iota
	ViewCountries
)

func (v View) String() string {
	if v == ViewCountries {
		return "countries"
	}
	return "models"
}

// barColors run from blue through yellow to red, like the bars in a dashboard.
var barColors = []string{"#6f9bf0", "#8da8d0", "#b0b296", "#d9c071", "#efbc5c", "#f0aa4c", "#ee974a", "#eb814b", "#e56d4e", "#dc5b52"}

// bar draws a fixed-width gradient bar for frac (0..1). Filled cells are
// coloured by their position; anything above zero shows at least one cell.
func bar(frac float64, cells int) string {
	if cells <= 0 {
		return ""
	}
	frac = math.Max(0, math.Min(1, frac))
	filled := int(math.Round(frac * float64(cells)))
	if frac > 0 && filled == 0 {
		filled = 1
	}
	var b strings.Builder
	for i := 0; i < cells; i++ {
		if i < filled {
			c := barColors[i*len(barColors)/cells]
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render("█"))
		} else {
			b.WriteString(dim.Render("░"))
		}
	}
	return b.String()
}

// hatched is an empty bar, used for rows that have no meaningful share.
func hatched(cells int) string { return dim.Render(strings.Repeat("░", cells)) }

func (m *Model) enters(provider string) string {
	if s, ok := m.paths[usage.UpstreamHost(provider)]; ok {
		return s.Enters()
	}
	return "-"
}

// labelMin keeps room for "beyond the edge: not observable" in the country
// column; provMin is the least width worth showing the provider names in.
const (
	labelMin = 31
	provMin  = 18
)

type ccol struct {
	title string
	w     int // 0 = flexible
	left  bool
	drop  int // 0 = never dropped; higher drops first when narrow
}

// renderCountries draws the Countries panel for the events in the window.
func (m *Model) renderCountries(in []proxy.Event, now time.Time, win time.Duration, width int) string {
	cw := width - 6 // border and padding
	if cw > 130 {
		cw = 130
	}
	if cw < 40 {
		cw = 40
	}
	rep := stats.ByCountry(in, m.paths)

	cols := []ccol{
		{"", 10, true, 4}, // the bar
		{"COUNTRY", labelMin, true, 0},
		{"CALLS", 6, false, 0},
		{"TOKENS IN", 9, false, 0},
		{"TOKENS OUT", 10, false, 1},
		{"BYTES UP", 9, false, 3},
		{"BYTES DOWN", 10, false, 2},
		{"PROVIDERS", 0, true, 5}, // takes whatever width is left
	}
	const gap = 2
	total := func() int {
		t := gap * (len(cols) - 1)
		for _, c := range cols {
			if c.w == 0 {
				t += provMin
			} else {
				t += c.w
			}
		}
		return t
	}
	for total() > cw {
		best, rank := -1, 0
		for i, c := range cols {
			if c.drop > rank {
				best, rank = i, c.drop
			}
		}
		if best < 0 {
			break
		}
		cols = append(cols[:best], cols[best+1:]...)
	}
	// Nothing droppable is left; if the core still does not fit, the country column
	// gives way and long labels are cut with an ellipsis.
	if over := total() - cw; over > 0 {
		for i := range cols {
			if cols[i].title == "COUNTRY" {
				cols[i].w = max(12, cols[i].w-over)
			}
		}
	}
	flex := cw - (total() - provMin)
	if flex > 48 {
		flex = 48
	}
	for i := range cols {
		if cols[i].w == 0 {
			cols[i].w = flex
		}
	}

	maxTok := 0
	for _, r := range rep.Rows {
		if r.Kind == "country" && r.Tokens() > maxTok {
			maxTok = r.Tokens()
		}
	}
	cell := func(c ccol, r stats.CountryRow, isTotal bool) string {
		switch c.title {
		case "":
			if r.Kind != "country" {
				return hatched(c.w)
			}
			frac := 0.0
			if maxTok > 0 {
				frac = float64(r.Tokens()) / float64(maxTok)
			}
			return bar(frac, c.w)
		case "COUNTRY":
			return pad(ellipsize(r.Label, c.w), c.w, true)
		case "CALLS":
			return pad(fmt.Sprint(r.Calls), c.w, false)
		case "TOKENS IN":
			return pad(stats.HumanCount(r.TokensIn), c.w, false)
		case "TOKENS OUT":
			return pad(stats.HumanCount(r.TokensOut), c.w, false)
		case "BYTES UP":
			return pad(r.BytesCell(r.BytesUp), c.w, false)
		case "BYTES DOWN":
			return pad(r.BytesCell(r.BytesDown), c.w, false)
		}
		return pad(ellipsize(strings.Join(r.Providers, ", "), c.w), c.w, true)
	}
	line := func(r stats.CountryRow, faint bool) string {
		var cells []string
		for _, c := range cols {
			s := cell(c, r, false)
			if faint && c.title != "" {
				s = dim.Render(s)
			}
			cells = append(cells, s)
		}
		return strings.Join(cells, strings.Repeat(" ", gap))
	}

	var b strings.Builder
	fit := lipgloss.NewStyle().Width(cw)
	b.WriteString(fit.Render(cyan.Bold(true).Render("Countries on the path to each provider's edge")+dim.Render("  ·  window "+fmtWindow(win))) + "\n")
	b.WriteString(fit.Render(dim.Render("your side of the connection, up to where it enters the provider")) + "\n\n")
	var hdr []string
	for _, c := range cols {
		hdr = append(hdr, dim.Render(pad(c.title, c.w, c.left)))
	}
	b.WriteString(strings.Join(hdr, strings.Repeat(" ", gap)) + "\n")
	for _, r := range rep.Rows {
		b.WriteString(line(r, r.Kind != "country") + "\n")
	}
	beyond := rep.Total
	beyond.Label = "beyond the edge: not observable"
	beyond.Kind = "total"
	b.WriteString("\n" + line(beyond, true) + "\n")

	wrap := func(s string) string { return dim.Render(lipgloss.NewStyle().Width(cw).Render(s)) }
	b.WriteString("\n")
	if len(m.paths) == 0 {
		b.WriteString(yellow.Render(lipgloss.NewStyle().Width(cw).Render("No saved paths yet. Run `lmon path` to measure the route to each provider, or start `lmon proxy --path-every 1h`.")) + "\n")
	} else {
		age := ""
		if !rep.Oldest.IsZero() {
			age = fmt.Sprintf(" (oldest measured %s)", stats.AgoPhrase(now.Sub(rep.Oldest)))
		}
		b.WriteString(wrap("Each call counts in every country on its provider's measured path"+age+", so rows add up to more than the total. The last row is every call: what continues beyond the edge cannot be observed.") + "\n")
	}
	for _, r := range append([]stats.CountryRow{rep.Total}, rep.Rows...) {
		if r.ByteCalls > 0 && r.ByteCalls < r.Calls {
			b.WriteString(wrap("* bytes cover only calls that went through the proxy; Claude Code sessions have none.") + "\n")
			break
		}
	}
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1)
	return panel.Render(strings.TrimRight(b.String(), "\n")) + "\n"
}

// pathsLoader returns the saved route summaries by endpoint host (nil if unreadable).
type pathsLoader = func() map[string]netpath.Summary
