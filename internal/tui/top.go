// Package tui implements `lmon top`, a live terminal view of recorded calls.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
)

var (
	Windows    = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour}
	sortKeys   = []string{"calls", "tokens", "p95", "errors", "name"}
	retention  = Windows[len(Windows)-1]
	pollEvery  = time.Second
	trendCells = 16
)

var (
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	head   = lipgloss.NewStyle().Bold(true).Reverse(true)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

// Retention is how far back `top` keeps events (its largest window).
func Retention() time.Duration { return retention }

type tickMsg time.Time

// Model is the Bubble Tea model for `lmon top`.
type Model struct {
	tailer *stats.Tailer
	events []proxy.Event
	now    func() time.Time
	prices *pricing.Table // nil = no cost column

	// Optional sources read from other tools' own logs (Claude Code, Codex).
	// Responses are upserted by ID because a response's output_tokens keeps
	// growing while it streams.
	claude   Importer
	imported map[string]proxy.Event

	// Saved route summaries (from `lmon path`), reloaded now and then. Nil
	// pathsFn means none: no ENTERS column and an empty Countries panel.
	pathsFn pathsLoader
	paths   map[string]netpath.Summary
	pathsAt time.Time

	view   View
	window int // index into Windows
	sortBy int // index into sortKeys
	paused bool
	w      int
	err    error
}

// New creates the model and loads existing history immediately. prices may be
// nil (costs are not shown), claude may be nil (proxy events only) and paths may
// be nil (no route information).
func New(logPath string, window time.Duration, prices *pricing.Table, claude Importer, paths pathsLoader) *Model {
	m := &Model{tailer: &stats.Tailer{Path: logPath}, now: time.Now, prices: prices,
		claude: claude, imported: map[string]proxy.Event{}, pathsFn: paths, paths: map[string]netpath.Summary{}}
	m.window = len(Windows) - 1
	for i, w := range Windows {
		if w >= window {
			m.window = i
			break
		}
	}
	// Seed from rotated files (everything but the active log) so a window that
	// spans a rotation isn't cut short; the tailer then follows the active file.
	var older []string
	for _, f := range proxy.LogFiles(logPath) {
		if f != logPath {
			older = append(older, f)
		}
	}
	if evs, err := stats.LoadFiles(older, m.now().Add(-retention)); err == nil {
		m.events = evs
	}
	m.poll()
	return m
}

func (m *Model) poll() {
	if m.pathsFn != nil && (m.pathsAt.IsZero() || m.now().Sub(m.pathsAt) >= 5*time.Second) {
		if p := m.pathsFn(); p != nil {
			m.paths = p
		}
		m.pathsAt = m.now()
	}
	evs, err := m.tailer.Poll()
	m.err = err
	m.events = append(m.events, evs...)
	cutoff := m.now().Add(-retention)
	i := 0
	for i < len(m.events) && m.events[i].Time.Before(cutoff) {
		i++
	}
	m.events = m.events[i:]

	if m.claude != nil {
		evs, err := m.claude.Poll()
		if err != nil && m.err == nil {
			m.err = err
		}
		for _, e := range evs {
			m.imported[e.ID] = e
		}
		for id, e := range m.imported {
			if e.Time.Before(cutoff) {
				delete(m.imported, id)
			}
		}
	}
}

// Importer yields calls recorded by other tools' own logs rather than the proxy.
type Importer interface {
	Poll() ([]proxy.Event, error)
	Label() string // shown in the header, e.g. "claude-code, codex"
}

// all is every event to show: proxy-observed ones plus Claude Code's, with a
// call seen by both counted once (the proxy's copy, which has latency).
func (m *Model) all() []proxy.Event {
	if m.claude == nil {
		return m.events
	}
	imp := make([]proxy.Event, 0, len(m.imported))
	for _, e := range m.imported {
		imp = append(imp, e)
	}
	return stats.Merge(m.events, imp)
}

func tick() tea.Cmd {
	return tea.Tick(pollEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Init() tea.Cmd { return tick() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w = msg.Width
	case tickMsg:
		if !m.paused {
			m.poll()
		}
		return m, tick()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "w":
			m.window = (m.window + 1) % len(Windows)
		case "s":
			m.sortBy = (m.sortBy + 1) % len(sortKeys)
		case "p":
			m.paused = !m.paused
		case "c", "tab":
			m.view = (m.view + 1) % 2
		}
	}
	return m, nil
}

func (m *Model) View() tea.View {
	v := tea.NewView(m.Render(m.w))
	v.AltScreen = true
	return v
}

// ---- rendering ----

type rowData struct {
	stats.Row
	trend []float64 // mean latency per bucket, NaN-free; -1 marks an empty bucket
}

// Render draws the whole screen for the given terminal width (0 = 100).
func (m *Model) Render(width int) string {
	if width <= 0 {
		width = 100
	}
	now := m.now()
	win := Windows[m.window]
	var in []proxy.Event
	for _, e := range m.all() {
		if !e.Time.Before(now.Add(-win)) {
			in = append(in, e)
		}
	}

	var b strings.Builder
	state := ""
	if m.paused {
		state = yellow.Render(" PAUSED")
	}
	src := ""
	if m.claude != nil {
		src = "  + " + m.claude.Label() + " (approx., no latency)"
	}
	sortNote := ""
	if m.view == ViewModels {
		sortNote = "  sort " + sortKeys[m.sortBy]
	}
	fit := lipgloss.NewStyle().Width(width) // wrap, rather than overflow, on narrow terminals
	b.WriteString(fit.Render(bold.Render("lmon top")+dim.Render(fmt.Sprintf("  %s  window %s%s%s", m.view, fmtWindow(win), sortNote, src))+state) + "\n")
	other := ViewCountries
	if m.view == ViewCountries {
		other = ViewModels
	}
	keys := fmt.Sprintf("[c] %s  [w] window  ", other)
	if m.view == ViewModels {
		keys += "[s] sort  "
	}
	b.WriteString(fit.Render(dim.Render(keys+"[p] pause  [q] quit")) + "\n\n")

	if m.err != nil {
		b.WriteString(red.Render("error reading log: "+m.err.Error()) + "\n\n")
	}
	if len(in) == 0 {
		b.WriteString("Waiting for calls in the last " + fmtWindow(win) + "...\n\n")
		b.WriteString(dim.Render("Is `lmon proxy` running, and is your SDK pointed at it?\n  ANTHROPIC_BASE_URL=http://localhost:8787/anthropic\n  OPENAI_BASE_URL=http://localhost:8787/openai/v1") + "\n")
		return b.String()
	}

	b.WriteString(fit.Render(summary(in, now, win, m.prices)) + "\n")
	b.WriteString(dim.Render("activity ") + cyan.Render(sparkline(bucketCounts(in, now, win, 40))) + "\n\n")
	if m.view == ViewCountries {
		b.WriteString(m.renderCountries(in, now, win, width))
		return b.String()
	}
	b.WriteString(m.table(in, now, win, width))
	return b.String()
}

func summary(in []proxy.Event, now time.Time, win time.Duration, prices *pricing.Table) string {
	var calls, errs, inTok, outTok, cacheR, totIn, unpriced int
	var lat []float64
	var cost float64
	first := now
	for _, e := range in {
		if usd, priced, billable := stats.PriceEvent(prices, e); priced {
			cost += usd
		} else if billable {
			unpriced++
		}
		if e.Time.Before(first) {
			first = e.Time
		}
		calls++
		if e.Status >= 400 {
			errs++
		}
		inTok += e.InputTokens
		outTok += e.OutputTokens
		cacheR += e.CacheReadTokens
		totIn += e.TotalInput()
		if e.HasLatency() {
			lat = append(lat, e.TotalMs)
		}
	}
	perMin := float64(calls) / win.Minutes()
	parts := []string{
		bold.Render(fmt.Sprintf("%d calls", calls)) + dim.Render(fmt.Sprintf(" (%.1f/min)", perMin)),
		fmt.Sprintf("in %s  out %s", human(inTok+cacheR), human(outTok)),
		"cache hit " + hitStyled(cacheR, totIn),
	}
	if errs > 0 {
		parts = append(parts, red.Render(fmt.Sprintf("%d errors", errs)))
	}
	if prices != nil && (cost > 0 || unpriced == 0) {
		mark := ""
		if unpriced > 0 {
			mark = "*" // some calls couldn't be priced: a lower bound
		}
		parts = append(parts, bold.Render("cost "+pricing.FormatUSD(cost)+mark)+dim.Render(" ("+burnRate(cost, now.Sub(first), win)+")"))
	}
	if len(lat) > 0 {
		parts = append(parts, fmt.Sprintf("p50 %s  p95 %s", fmtMs(stats.Percentile(lat, 50)), fmtMs(stats.Percentile(lat, 95))))
	}
	return strings.Join(parts, dim.Render("  ·  "))
}

// burnRate projects cost per hour from the span the data actually covers, so a
// 24h window with ten minutes of traffic isn't diluted by 23h50m of silence.
// The span is at least one minute (to avoid wild extrapolation from one call).
func burnRate(cost float64, span, win time.Duration) string {
	if span < time.Minute {
		span = time.Minute
	}
	if span > win {
		span = win
	}
	return pricing.FormatUSD(cost/span.Hours()) + "/h"
}

// latCell shows a latency, or a dim dash when none of the row's calls went
// through the proxy (e.g. imported Claude Code sessions).
func latCell(r rowData, ms float64, w int) string {
	if r.LatencyCalls == 0 {
		return dim.Render(pad("-", w, false))
	}
	return pad(fmtMs(ms), w, false)
}

type col struct {
	title string
	w     int
	left  bool
	drop  int // 0 = never dropped; higher drops first when narrow
	cell  func(r rowData, w int) string
}

func (m *Model) table(in []proxy.Event, now time.Time, win time.Duration, width int) string {
	rows := buildRows(in, now, win, m.prices)
	sortRows(rows, sortKeys[m.sortBy])

	cols := []col{
		{"PROVIDER", 10, true, 2, func(r rowData, w int) string { return pad(r.Provider, w, true) }},
		{"MODEL", 0, true, 0, func(r rowData, w int) string { return pad(ellipsize(r.Model, w), w, true) }},
		{"CALLS", 6, false, 0, func(r rowData, w int) string { return pad(fmt.Sprint(r.Calls), w, false) }},
		{"ERR", 4, false, 6, func(r rowData, w int) string {
			s := pad(fmt.Sprint(r.Errors), w, false)
			if r.Errors > 0 {
				return red.Render(s)
			}
			return dim.Render(s)
		}},
		{"IN", 7, false, 5, func(r rowData, w int) string { return pad(human(r.Input+r.CacheRead+r.CacheWrite), w, false) }},
		{"OUT", 7, false, 7, func(r rowData, w int) string { return pad(human(r.Output), w, false) }},
		{"CACHE-R", 8, false, 8, func(r rowData, w int) string { return pad(human(r.CacheRead), w, false) }},
		{"HIT%", 5, false, 0, func(r rowData, w int) string {
			return hitStyledW(r.CacheRead, r.Input+r.CacheRead+r.CacheWrite, w)
		}},
		{"COST", 9, false, 3, func(r rowData, w int) string {
			if m.prices == nil {
				return pad("", w, false)
			}
			l := r.CostLabel()
			s := pad(l, w, false)
			if l == "n/a" || l == "-" {
				return dim.Render(s)
			}
			return s
		}},
		{"P50", 7, false, 4, func(r rowData, w int) string { return latCell(r, r.P50TotalMs, w) }},
		{"P95", 7, false, 0, func(r rowData, w int) string { return latCell(r, r.P95TotalMs, w) }},
		{"TTFB", 7, false, 9, func(r rowData, w int) string { return latCell(r, r.AvgTTFBMs, w) }},
		{"ENTERS", 13, true, 10, func(r rowData, w int) string { return pad(ellipsize(m.enters(r.Provider), w), w, true) }},
		{"TREND", trendCells, true, 11, func(r rowData, w int) string { return cyan.Render(pad(sparkline(r.trend), w, true)) }},
	}

	if m.prices == nil {
		for i, c := range cols {
			if c.title == "COST" {
				cols = append(cols[:i], cols[i+1:]...)
				break
			}
		}
	}

	if len(m.paths) == 0 {
		for i, c := range cols {
			if c.title == "ENTERS" {
				cols = append(cols[:i], cols[i+1:]...)
				break
			}
		}
	}

	const gap = 2
	total := func(cs []col) int {
		t := gap * (len(cs) - 1)
		for _, c := range cs {
			if c.w == 0 {
				t += 10 // minimum for the flex MODEL column
			} else {
				t += c.w
			}
		}
		return t
	}
	// Drop optional columns, highest rank first, until it fits.
	for total(cols) > width {
		best, bestRank := -1, 0
		for i, c := range cols {
			if c.drop > bestRank {
				best, bestRank = i, c.drop
			}
		}
		if best < 0 {
			break
		}
		cols = append(cols[:best], cols[best+1:]...)
	}
	fixed := total(cols) - 10
	flex := width - fixed
	if flex < 10 {
		flex = 10
	}
	if flex > 34 {
		flex = 34
	}
	for i := range cols {
		if cols[i].w == 0 {
			cols[i].w = flex
		}
	}

	var b strings.Builder
	var hdr []string
	for _, c := range cols {
		hdr = append(hdr, pad(c.title, c.w, c.left))
	}
	b.WriteString(head.Render(strings.Join(hdr, strings.Repeat(" ", gap))) + "\n")
	for _, r := range rows {
		var cells []string
		for _, c := range cols {
			cells = append(cells, c.cell(r, c.w))
		}
		b.WriteString(strings.Join(cells, strings.Repeat(" ", gap)) + "\n")
	}
	return b.String()
}

func buildRows(in []proxy.Event, now time.Time, win time.Duration, prices *pricing.Table) []rowData {
	agg := stats.AggregateWithPrices(in, prices)
	type key struct{ p, m string }
	byKey := map[key][]proxy.Event{}
	for _, e := range in {
		model := e.Model
		if model == "" {
			model = "(unknown)"
		}
		k := key{e.Provider, model}
		if e.HasLatency() {
			byKey[k] = append(byKey[k], e)
		}
	}
	rows := make([]rowData, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, rowData{Row: r, trend: bucketLatency(byKey[key{r.Provider, r.Model}], now, win, trendCells)})
	}
	return rows
}

func sortRows(rows []rowData, by string) {
	tok := func(r rowData) int { return r.Input + r.Output + r.CacheRead + r.CacheWrite }
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch by {
		case "tokens":
			if tok(a) != tok(b) {
				return tok(a) > tok(b)
			}
		case "p95":
			if a.P95TotalMs != b.P95TotalMs {
				return a.P95TotalMs > b.P95TotalMs
			}
		case "errors":
			if a.Errors != b.Errors {
				return a.Errors > b.Errors
			}
		case "name":
		default: // calls
			if a.Calls != b.Calls {
				return a.Calls > b.Calls
			}
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
}

// ---- bucketing and formatting helpers ----

func bucketIndex(t, now time.Time, win time.Duration, n int) (int, bool) {
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	if age >= win {
		return 0, false
	}
	// oldest bucket first, newest last
	return n - 1 - int(float64(age)/float64(win)*float64(n)), true
}

func bucketCounts(in []proxy.Event, now time.Time, win time.Duration, n int) []float64 {
	out := make([]float64, n)
	for _, e := range in {
		if i, ok := bucketIndex(e.Time, now, win, n); ok {
			out[i]++
		}
	}
	return out
}

// bucketLatency returns mean total latency per bucket; -1 marks empty buckets.
func bucketLatency(in []proxy.Event, now time.Time, win time.Duration, n int) []float64 {
	sum := make([]float64, n)
	cnt := make([]int, n)
	for _, e := range in {
		if i, ok := bucketIndex(e.Time, now, win, n); ok {
			sum[i] += e.TotalMs
			cnt[i]++
		}
	}
	out := make([]float64, n)
	for i := range out {
		if cnt[i] == 0 {
			out[i] = -1
		} else {
			out[i] = sum[i] / float64(cnt[i])
		}
	}
	return out
}

var blocks = []rune("▁▂▃▄▅▆▇█")

// sparkline scales values (ignoring negatives, which render as blanks) to
// eight block heights. A series with no spread renders mid-height.
func sparkline(v []float64) string {
	lo, hi, any := 0.0, 0.0, false
	for _, x := range v {
		if x < 0 {
			continue
		}
		if !any || x < lo {
			lo = x
		}
		if !any || x > hi {
			hi = x
		}
		any = true
	}
	var b strings.Builder
	for _, x := range v {
		switch {
		case x < 0:
			b.WriteRune(' ')
		case hi == lo:
			b.WriteRune(blocks[len(blocks)/2])
		default:
			i := int((x - lo) / (hi - lo) * float64(len(blocks)-1))
			b.WriteRune(blocks[i])
		}
	}
	return b.String()
}

func pad(s string, w int, left bool) string {
	n := lipgloss.Width(s)
	if n >= w {
		return s
	}
	if left {
		return s + strings.Repeat(" ", w-n)
	}
	return strings.Repeat(" ", w-n) + s
}

func ellipsize(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

func human(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func fmtMs(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func fmtWindow(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func hitStyled(read, total int) string { return hitStyledW(read, total, 0) }

func hitStyledW(read, total, w int) string {
	if total == 0 {
		return dim.Render(pad("-", w, false))
	}
	p := float64(read) / float64(total) * 100
	s := pad(fmt.Sprintf("%.0f%%", p), w, false)
	switch {
	case p >= 80:
		return green.Render(s)
	case p >= 40:
		return yellow.Render(s)
	}
	return dim.Render(s)
}
