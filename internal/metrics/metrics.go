// Package metrics exposes lmon's proxy and endpoint-probe data in the
// Prometheus text exposition format (version 0.0.4).
//
// It is hand-rolled instead of using client_golang: lmon aims to stay one small
// static binary and the text format is simple. The output should be checked
// with Prometheus' own tool after changes: curl .../metrics | promtool check metrics
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/probe"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
)

// maxModels bounds how many distinct (provider, model) pairs get their own
// series; later ones are folded into model="other" so a misbehaving upstream
// can't grow memory or the scrape without limit.
const maxModels = 200

// Seconds. Covers fast completions through multi-minute streamed responses.
var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type mKey struct{ provider, model string }
type reqKey struct {
	provider, model, code string
}
type tokKey struct {
	provider, model, typ string
}
type byteKey struct {
	provider, model, dir string
}

type hist struct {
	counts []uint64 // per bucket, not cumulative; last slot is +Inf
	sum    float64
	count  uint64
}

func newHist() *hist { return &hist{counts: make([]uint64, len(buckets)+1)} }

func (h *hist) observe(v float64) {
	i := sort.SearchFloat64s(buckets, v) // first bucket with upper bound >= v
	h.counts[i]++
	h.sum += v
	h.count++
}

// ProbeSample is the latest result of probing one host.
type ProbeSample struct {
	Result probe.Result
	At     time.Time
}

type Registry struct {
	version string
	prices  func() *pricing.Table // may return nil

	inflight atomic.Int64

	mu       sync.Mutex
	models   map[mKey]bool
	requests map[reqKey]uint64
	tokens   map[tokKey]uint64
	bytes    map[byteKey]uint64
	search   map[mKey]uint64
	cost     map[mKey]float64
	unpriced map[mKey]uint64
	dur      map[mKey]*hist
	ttfb     map[mKey]*hist
	probes   map[string]ProbeSample
}

// New creates a registry. prices may be nil or return nil, in which case no
// cost metrics are produced.
func New(version string, prices func() *pricing.Table) *Registry {
	return &Registry{
		version: version, prices: prices,
		models:   map[mKey]bool{},
		requests: map[reqKey]uint64{}, tokens: map[tokKey]uint64{}, bytes: map[byteKey]uint64{}, search: map[mKey]uint64{},
		cost: map[mKey]float64{}, unpriced: map[mKey]uint64{},
		dur: map[mKey]*hist{}, ttfb: map[mKey]*hist{},
		probes: map[string]ProbeSample{},
	}
}

// Track marks a request as in flight until the returned func is called.
func (r *Registry) Track() (done func()) {
	r.inflight.Add(1)
	return func() { r.inflight.Add(-1) }
}

func (r *Registry) key(provider, model string) mKey {
	if model == "" {
		model = "unknown"
	}
	k := mKey{provider, model}
	if !r.models[k] {
		if len(r.models) >= maxModels {
			return mKey{provider, "other"}
		}
		r.models[k] = true
	}
	return k
}

// Observe records one call seen by the proxy. Wire it to Logger.OnEvent.
func (r *Registry) Observe(e proxy.Event) {
	var tbl *pricing.Table
	if r.prices != nil {
		tbl = r.prices()
	}
	usd, priced, billable := stats.PriceEvent(tbl, e)

	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(e.Provider, e.Model)
	r.requests[reqKey{k.provider, k.model, strconv.Itoa(e.Status)}]++
	if e.HasLatency() {
		h := r.dur[k]
		if h == nil {
			h = newHist()
			r.dur[k] = h
		}
		h.observe(e.TotalMs / 1000)
		if e.TTFBMs > 0 { // upstream errors never got a first byte
			h := r.ttfb[k]
			if h == nil {
				h = newHist()
				r.ttfb[k] = h
			}
			h.observe(e.TTFBMs / 1000)
		}
	}
	if e.BytesUp > 0 {
		r.bytes[byteKey{k.provider, k.model, "up"}] += uint64(e.BytesUp)
	}
	if e.BytesDown > 0 {
		r.bytes[byteKey{k.provider, k.model, "down"}] += uint64(e.BytesDown)
	}
	if e.HasUsage {
		add := func(typ string, n int) {
			if n > 0 {
				r.tokens[tokKey{k.provider, k.model, typ}] += uint64(n)
			}
		}
		add("input", e.InputTokens)
		add("output", e.OutputTokens)
		add("cache_read", e.CacheReadTokens)
		add("cache_write", e.CacheWriteTokens)
		if e.WebSearchRequests > 0 {
			r.search[k] += uint64(e.WebSearchRequests)
		}
	}
	if priced {
		r.cost[k] += usd
	} else if billable && tbl != nil {
		r.unpriced[k]++
	}
}

// SetProbe stores the latest probe result for host.
func (r *Registry) SetProbe(host string, res probe.Result, at time.Time) {
	r.mu.Lock()
	r.probes[host] = ProbeSample{Result: res, At: at}
	r.mu.Unlock()
}

// RunProbes probes each host every interval until ctx ends, starting at once.
// geoDB returns the GeoIP database path to use ("" for none).
func (r *Registry) RunProbes(ctx context.Context, hosts []string, every time.Duration, geoDB func() string, timeout time.Duration) {
	run := func() {
		for _, h := range hosts {
			if ctx.Err() != nil {
				return
			}
			db := ""
			if geoDB != nil {
				db = geoDB()
			}
			r.SetProbe(h, probe.Run(ctx, h, db, timeout), time.Now())
		}
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ---- exposition ----

func esc(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// lbl renders {k="v",...} from alternating names and values, skipping empty values.
func lbl(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			parts = append(parts, kv[i]+`="`+esc(kv[i+1])+`"`)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// family collects the series of one metric so they render together, sorted.
type family struct {
	name, help, typ string
	lines           []string // each "name{labels} value"
}

func (f *family) add(suffix, labels, value string) {
	f.lines = append(f.lines, f.name+suffix+labels+" "+value)
}

func (f *family) write(w io.Writer) {
	if len(f.lines) == 0 {
		return
	}
	sort.Strings(f.lines)
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
	for _, l := range f.lines {
		fmt.Fprintln(w, l)
	}
}

func histLines(f *family, k mKey, h *hist) {
	var cum uint64
	for i, le := range buckets {
		cum += h.counts[i]
		f.add("_bucket", lbl("model", k.model, "provider", k.provider, "le", num(le)), strconv.FormatUint(cum, 10))
	}
	f.add("_bucket", lbl("model", k.model, "provider", k.provider, "le", "+Inf"), strconv.FormatUint(h.count, 10))
	f.add("_sum", lbl("model", k.model, "provider", k.provider), num(h.sum))
	f.add("_count", lbl("model", k.model, "provider", k.provider), strconv.FormatUint(h.count, 10))
}

// Write renders every metric.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info := &family{name: "lmon_build_info", help: "lmon version.", typ: "gauge"}
	info.add("", lbl("version", r.version), "1")
	info.write(w)

	infl := &family{name: "lmon_inflight_requests", help: "Proxied requests currently in flight.", typ: "gauge"}
	infl.add("", "", strconv.FormatInt(r.inflight.Load(), 10))
	infl.write(w)

	reqs := &family{name: "lmon_requests_total", help: "Proxied LLM API calls by provider, model and HTTP status code.", typ: "counter"}
	for k, n := range r.requests {
		reqs.add("", lbl("code", k.code, "model", k.model, "provider", k.provider), strconv.FormatUint(n, 10))
	}
	reqs.write(w)

	toks := &family{name: "lmon_tokens_total", help: "Tokens by provider, model and type (input is uncached input; cache_read and cache_write are separate).", typ: "counter"}
	for k, n := range r.tokens {
		toks.add("", lbl("model", k.model, "provider", k.provider, "type", k.typ), strconv.FormatUint(n, 10))
	}
	toks.write(w)

	byt := &family{name: "lmon_bytes_total", help: "Payload bytes through the proxy: up is the request sent to the provider, down the response (after decompression). Excludes headers and TLS overhead.", typ: "counter"}
	for k, n := range r.bytes {
		byt.add("", lbl("direction", k.dir, "model", k.model, "provider", k.provider), strconv.FormatUint(n, 10))
	}
	byt.write(w)

	srch := &family{name: "lmon_web_search_requests_total", help: "Server-side web search queries.", typ: "counter"}
	for k, n := range r.search {
		srch.add("", lbl("model", k.model, "provider", k.provider), strconv.FormatUint(n, 10))
	}
	srch.write(w)

	cost := &family{name: "lmon_cost_usd_total", help: "Estimated cost in USD at standard list prices. Needs `lmon prices update`.", typ: "counter"}
	for k, v := range r.cost {
		cost.add("", lbl("model", k.model, "provider", k.provider), num(v))
	}
	cost.write(w)

	unp := &family{name: "lmon_unpriced_requests_total", help: "Successful calls that could not be priced (model not in the price table, or no usage in the response).", typ: "counter"}
	for k, n := range r.unpriced {
		unp.add("", lbl("model", k.model, "provider", k.provider), strconv.FormatUint(n, 10))
	}
	unp.write(w)

	d := &family{name: "lmon_request_duration_seconds", help: "Total time to complete a proxied call, including streaming.", typ: "histogram"}
	for k, h := range r.dur {
		histLines(d, k, h)
	}
	writeHist(w, d)
	tf := &family{name: "lmon_time_to_first_byte_seconds", help: "Time until the upstream started responding.", typ: "histogram"}
	for k, h := range r.ttfb {
		histLines(tf, k, h)
	}
	writeHist(w, tf)

	r.writeProbes(w)
}

// writeHist keeps each histogram series' buckets in numeric order: a plain
// sort of the lines would put le="10" before le="2.5".
func writeHist(w io.Writer, f *family) {
	if len(f.lines) == 0 {
		return
	}
	// Lines were appended series by series, each already in bucket order.
	// Sort whole series by their label prefix while preserving the order inside.
	type series struct {
		key   string
		lines []string
	}
	var all []series
	cur := -1
	for _, l := range f.lines {
		key := seriesKey(l)
		if cur < 0 || all[cur].key != key {
			all = append(all, series{key: key})
			cur++
		}
		all[cur].lines = append(all[cur].lines, l)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].key < all[j].key })
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
	for _, s := range all {
		for _, l := range s.lines {
			fmt.Fprintln(w, l)
		}
	}
}

// seriesKey is a histogram line's labels without le, so all lines of one
// series (buckets, sum, count) share it.
func seriesKey(line string) string {
	i := strings.IndexByte(line, '{')
	j := strings.LastIndexByte(line, '}')
	if i < 0 || j < i {
		return line
	}
	lab := line[i+1 : j]
	if k := strings.Index(lab, `,le="`); k >= 0 {
		lab = lab[:k] + lab[strings.IndexByte(lab[k+5:], '"')+k+6:]
	}
	return lab
}

func (r *Registry) writeProbes(w io.Writer) {
	if len(r.probes) == 0 {
		return
	}
	hosts := make([]string, 0, len(r.probes))
	for h := range r.probes {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	ok := &family{name: "lmon_probe_success", help: "1 if the last probe of the host completed an HTTPS request.", typ: "gauge"}
	status := &family{name: "lmon_probe_http_status", help: "HTTP status of the last probe (404 from an API root is normal).", typ: "gauge"}
	last := &family{name: "lmon_probe_last_run_timestamp_seconds", help: "When the host was last probed.", typ: "gauge"}
	phase := &family{name: "lmon_probe_phase_seconds", help: "Timing of the last probe by phase.", typ: "gauge"}
	dnsT := &family{name: "lmon_probe_dns_seconds", help: "DNS lookup time per resolver in the last probe.", typ: "gauge"}
	dnsOK := &family{name: "lmon_probe_dns_success", help: "1 if the resolver answered in the last probe.", typ: "gauge"}
	dnsN := &family{name: "lmon_probe_dns_answers", help: "Number of addresses each resolver returned.", typ: "gauge"}
	ipInfo := &family{name: "lmon_probe_ip_info", help: "Addresses the host resolved to, with location when a GeoIP database is available. A changing label set means the endpoint moved.", typ: "gauge"}

	for _, h := range hosts {
		s := r.probes[h]
		t := s.Result.Timing
		succ := "1"
		if t.Error != "" || t.Status == 0 {
			succ = "0"
		}
		ok.add("", lbl("host", h), succ)
		status.add("", lbl("host", h), strconv.Itoa(t.Status))
		last.add("", lbl("host", h), num(float64(s.At.UnixNano())/1e9))
		for _, p := range []struct {
			name string
			d    probe.Duration
		}{{"dns", t.DNS}, {"tcp_connect", t.Connect}, {"tls_handshake", t.TLS}, {"ttfb", t.TTFB}, {"total", t.Total}} {
			phase.add("", lbl("host", h, "phase", p.name), num(time.Duration(p.d).Seconds()))
		}
		for _, d := range s.Result.DNS {
			dnsT.add("", lbl("host", h, "resolver", d.Resolver), num(time.Duration(d.Latency).Seconds()))
			good := "1"
			if d.Error != "" {
				good = "0"
			}
			dnsOK.add("", lbl("host", h, "resolver", d.Resolver), good)
			dnsN.add("", lbl("host", h, "resolver", d.Resolver), strconv.Itoa(len(d.IPs)))
		}
		for _, ip := range s.Result.IPs {
			country, city := "", ""
			if ip.Geo != nil {
				country, city = ip.Geo.Country, ip.Geo.City
			}
			ipInfo.add("", lbl("city", city, "country", country, "host", h, "ip", ip.IP), "1")
		}
	}
	for _, f := range []*family{ok, status, last, phase, dnsT, dnsOK, dnsN, ipInfo} {
		f.write(w)
	}
}

// ServeHTTP serves the exposition.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if req.Method == http.MethodHead {
		return
	}
	r.Write(w)
}
