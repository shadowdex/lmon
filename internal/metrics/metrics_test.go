package metrics_test

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/metrics"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/probe"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/usage"
)

func prices(t *testing.T) func() *pricing.Table {
	t.Helper()
	tb, err := pricing.Reduce([]byte(`{"claude-x":{"litellm_provider":"anthropic","mode":"chat","input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`))
	if err != nil {
		t.Fatal(err)
	}
	return func() *pricing.Table { return tb }
}

func scrape(r *metrics.Registry) string {
	var b bytes.Buffer
	r.Write(&b)
	return b.String()
}

// value returns the sample value of the first line starting with prefix.
func value(t *testing.T, out, prefix string) float64 {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix+" ") {
			v, err := strconv.ParseFloat(strings.TrimPrefix(l, prefix+" "), 64)
			if err != nil {
				t.Fatalf("bad value in %q", l)
			}
			return v
		}
	}
	t.Fatalf("no sample %q in:\n%s", prefix, out)
	return 0
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %.12g want %.12g", got, want)
	}
}

func event(model string, status int, totalMs, ttfbMs float64, u usage.Usage, hasUsage bool) proxy.Event {
	u.Model = model
	return proxy.Event{Time: time.Now(), Provider: "anthropic", Status: status, TotalMs: totalMs, TTFBMs: ttfbMs, Usage: u, HasUsage: hasUsage}
}

func TestObserveMapsAnEventToCountersCostAndHistograms(t *testing.T) {
	r := metrics.New("v1", prices(t))
	r.Observe(event("claude-x", 200, 1500, 300, usage.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 100, WebSearchRequests: 2}, true))
	out := scrape(r)

	L := `{model="claude-x",provider="anthropic"`
	near(t, value(t, out, `lmon_requests_total{code="200",model="claude-x",provider="anthropic"}`), 1)
	for typ, want := range map[string]float64{"input": 1000, "output": 500, "cache_read": 2000, "cache_write": 100} {
		near(t, value(t, out, `lmon_tokens_total`+L+`,type="`+typ+`"}`), want)
	}
	near(t, value(t, out, `lmon_web_search_requests_total`+L+`}`), 2)
	// no cache prices listed -> cached reads/writes billed as input: 1000+500*2+2000+100 = 4100 micro-dollars
	near(t, value(t, out, `lmon_cost_usd_total`+L+`}`), 0.0041)
	near(t, value(t, out, `lmon_request_duration_seconds_sum`+L+`}`), 1.5)
	near(t, value(t, out, `lmon_request_duration_seconds_count`+L+`}`), 1)
	near(t, value(t, out, `lmon_request_duration_seconds_bucket{model="claude-x",provider="anthropic",le="1"}`), 0)
	near(t, value(t, out, `lmon_request_duration_seconds_bucket{model="claude-x",provider="anthropic",le="2.5"}`), 1)
	near(t, value(t, out, `lmon_time_to_first_byte_seconds_sum`+L+`}`), 0.3)
	near(t, value(t, out, `lmon_build_info{version="v1"}`), 1)
}

func TestFailedCallsCountAsRequestsButNotTokensCostOrTTFB(t *testing.T) {
	r := metrics.New("v", prices(t))
	r.Observe(event("", 429, 200, 0, usage.Usage{}, false))
	out := scrape(r)
	near(t, value(t, out, `lmon_requests_total{code="429",model="unknown",provider="anthropic"}`), 1)
	for _, none := range []string{"lmon_tokens_total", "lmon_cost_usd_total", "lmon_unpriced_requests_total", "lmon_time_to_first_byte_seconds"} {
		if strings.Contains(out, none) {
			t.Errorf("%s should not appear for a failed call with no usage:\n%s", none, out)
		}
	}
}

func TestUnknownModelIsUnpricedNotFree(t *testing.T) {
	r := metrics.New("v", prices(t))
	r.Observe(event("claude-mystery", 200, 100, 50, usage.Usage{InputTokens: 10}, true))
	out := scrape(r)
	near(t, value(t, out, `lmon_unpriced_requests_total{model="claude-mystery",provider="anthropic"}`), 1)
	if strings.Contains(out, `lmon_cost_usd_total{model="claude-mystery"`) {
		t.Error("an unpriced model must not get a $0 cost series")
	}
}

func TestNoPriceTableMeansNoCostFamilies(t *testing.T) {
	for name, fn := range map[string]func() *pricing.Table{"nil func": nil, "nil table": func() *pricing.Table { return nil }} {
		r := metrics.New("v", fn)
		r.Observe(event("claude-x", 200, 100, 50, usage.Usage{InputTokens: 10}, true))
		out := scrape(r)
		if strings.Contains(out, "lmon_cost_usd_total") || strings.Contains(out, "lmon_unpriced") {
			t.Errorf("%s: cost metrics without prices:\n%s", name, out)
		}
		near(t, value(t, out, `lmon_tokens_total{model="claude-x",provider="anthropic",type="input"}`), 10)
	}
}

var leRe = regexp.MustCompile(`le="([^"]+)"`)

func TestHistogramsAreCumulativeOrderedAndConsistent(t *testing.T) {
	r := metrics.New("v", nil)
	for _, ms := range []float64{10, 60, 120, 400, 900, 2000, 7000, 7000, 45000, 999999} {
		r.Observe(event("claude-x", 200, ms, ms/4, usage.Usage{}, false))
	}
	out := scrape(r)

	var les []string
	var counts []float64
	var inf float64
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "lmon_request_duration_seconds_bucket") {
			continue
		}
		le := leRe.FindStringSubmatch(l)[1]
		v, _ := strconv.ParseFloat(l[strings.LastIndex(l, " ")+1:], 64)
		if le == "+Inf" {
			inf = v
			continue
		}
		les = append(les, le)
		counts = append(counts, v)
	}
	// numeric order: a lexical sort would put "10" before "2.5"
	var prev float64 = -1
	for _, le := range les {
		f, _ := strconv.ParseFloat(le, 64)
		if f <= prev {
			t.Fatalf("buckets out of numeric order: %v", les)
		}
		prev = f
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Fatalf("buckets must be cumulative: %v", counts)
		}
	}
	if inf != 10 || near2(value(t, out, `lmon_request_duration_seconds_count{model="claude-x",provider="anthropic"}`), 10) {
		t.Fatalf("+Inf=%v must equal count=10", inf)
	}
	if counts[len(counts)-1] != 9 { // 999.999s falls only in +Inf (largest bucket is 300s)
		t.Fatalf("last finite bucket = %v, want 9", counts[len(counts)-1])
	}
}

func near2(got, want float64) bool { return math.Abs(got-want) > 1e-12 }

func TestExpositionStructure(t *testing.T) {
	r := metrics.New("v", prices(t))
	r.Observe(event("claude-x", 200, 100, 50, usage.Usage{InputTokens: 10, OutputTokens: 5}, true))
	r.Observe(event("claude-y", 500, 100, 0, usage.Usage{}, false))
	out := scrape(r)

	if !strings.HasSuffix(out, "\n") || strings.Contains(out, "\n\n") {
		t.Error("output must end with a newline and have no blank lines")
	}
	help, typ := map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "# HELP "):
			help[strings.Fields(l)[2]]++
		case strings.HasPrefix(l, "# TYPE "):
			typ[strings.Fields(l)[2]]++
			seen[strings.Fields(l)[2]] = true
		default:
			name := l[:strings.IndexAny(l, "{ ")]
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			if !seen[name] && !seen[base] {
				t.Errorf("sample %q appears before its # TYPE line", l)
			}
		}
	}
	for name, n := range typ {
		if n != 1 || help[name] != 1 {
			t.Errorf("%s: %d TYPE and %d HELP lines, want exactly one each", name, n, help[name])
		}
	}
	if scrape(r) != out {
		t.Error("output must be deterministic between scrapes")
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := metrics.New("v", nil)
	r.Observe(event("a\"b\\c\nd", 200, 1, 1, usage.Usage{}, false))
	if out := scrape(r); !strings.Contains(out, `model="a\"b\\c\nd"`) {
		t.Fatalf("label not escaped:\n%s", out)
	}
}

func TestDistinctModelsAreCapped(t *testing.T) {
	r := metrics.New("v", nil)
	for i := 0; i < 500; i++ {
		r.Observe(event(fmt.Sprintf("model-%d", i), 200, 1, 1, usage.Usage{}, false))
	}
	out := scrape(r)
	models := map[string]bool{}
	for _, m := range regexp.MustCompile(`lmon_requests_total\{code="200",model="([^"]+)"`).FindAllStringSubmatch(out, -1) {
		models[m[1]] = true
	}
	if len(models) != 201 || !models["other"] { // 200 real + "other"
		t.Fatalf("got %d model values (other=%v)", len(models), models["other"])
	}
	near(t, value(t, out, `lmon_requests_total{code="200",model="other",provider="anthropic"}`), 300)
}

func TestConcurrentObserveAndScrape(t *testing.T) {
	r := metrics.New("v", prices(t))
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.Observe(event(fmt.Sprintf("claude-x"), 200, float64(i), float64(i)/2, usage.Usage{InputTokens: 1}, true))
				if i%20 == 0 {
					scrape(r)
				}
			}
		}(w)
	}
	wg.Wait()
	near(t, value(t, scrape(r), `lmon_requests_total{code="200",model="claude-x",provider="anthropic"}`), 1600)
}

func TestInFlightGauge(t *testing.T) {
	r := metrics.New("v", nil)
	done := r.Track()
	done2 := r.Track()
	near(t, value(t, scrape(r), "lmon_inflight_requests"), 2)
	done()
	done2()
	near(t, value(t, scrape(r), "lmon_inflight_requests"), 0)
}

func TestProbeMetrics(t *testing.T) {
	r := metrics.New("v", nil)
	at := time.Unix(1_800_000_000, 0)
	r.SetProbe("api.example.com", probe.Result{
		Host: "api.example.com",
		DNS: []probe.DNSResult{
			{Resolver: "system", IPs: []string{"1.2.3.4", "2001:db8::1"}, Latency: probe.Duration(12 * time.Millisecond)},
			{Resolver: "cloudflare", Error: "timeout", Latency: probe.Duration(2 * time.Second)},
		},
		IPs: []probe.IPInfo{
			{IP: "1.2.3.4", Geo: &probe.Geo{Country: "United States", City: "San Francisco"}},
			{IP: "2001:db8::1"}, // no GeoIP database entry
		},
		Timing: probe.Timing{DNS: probe.Duration(time.Millisecond), Connect: probe.Duration(5 * time.Millisecond),
			TLS: probe.Duration(20 * time.Millisecond), TTFB: probe.Duration(100 * time.Millisecond), Total: probe.Duration(101 * time.Millisecond), Status: 404},
	}, at)
	out := scrape(r)

	near(t, value(t, out, `lmon_probe_success{host="api.example.com"}`), 1)
	near(t, value(t, out, `lmon_probe_http_status{host="api.example.com"}`), 404)
	near(t, value(t, out, `lmon_probe_last_run_timestamp_seconds{host="api.example.com"}`), 1.8e9)
	near(t, value(t, out, `lmon_probe_phase_seconds{host="api.example.com",phase="tls_handshake"}`), 0.02)
	near(t, value(t, out, `lmon_probe_phase_seconds{host="api.example.com",phase="total"}`), 0.101)
	near(t, value(t, out, `lmon_probe_dns_seconds{host="api.example.com",resolver="system"}`), 0.012)
	near(t, value(t, out, `lmon_probe_dns_success{host="api.example.com",resolver="system"}`), 1)
	near(t, value(t, out, `lmon_probe_dns_success{host="api.example.com",resolver="cloudflare"}`), 0)
	near(t, value(t, out, `lmon_probe_dns_answers{host="api.example.com",resolver="system"}`), 2)
	near(t, value(t, out, `lmon_probe_ip_info{city="San Francisco",country="United States",host="api.example.com",ip="1.2.3.4"}`), 1)
	near(t, value(t, out, `lmon_probe_ip_info{host="api.example.com",ip="2001:db8::1"}`), 1) // unknown geo: labels omitted, not empty

	r.SetProbe("down.example.com", probe.Result{Timing: probe.Timing{Error: "connection refused"}}, at)
	near(t, value(t, scrape(r), `lmon_probe_success{host="down.example.com"}`), 0)
}

func TestHTTPHandler(t *testing.T) {
	r := metrics.New("v", nil)
	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if !strings.Contains(string(body), "lmon_build_info") {
		t.Fatalf("body: %s", body)
	}
	if resp, _ := http.Post(srv.URL, "text/plain", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	if resp, _ := http.Head(srv.URL); resp.StatusCode != 200 {
		t.Fatalf("HEAD = %d", resp.StatusCode)
	}
}

// Through the real proxy handler: the logger hook must feed the registry.
func TestEndToEndThroughProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":0}}`)
	}))
	defer up.Close()
	lg, err := proxy.NewLogger(filepath.Join(t.TempDir(), "e.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	reg := metrics.New("v", prices(t))
	lg.OnEvent = reg.Observe
	px := httptest.NewServer(proxy.Handler(lg, map[string]string{"anthropic": up.URL}))
	defer px.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Post(px.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	out := scrape(reg)
	near(t, value(t, out, `lmon_requests_total{code="200",model="claude-x",provider="anthropic"}`), 3)
	near(t, value(t, out, `lmon_tokens_total{model="claude-x",provider="anthropic",type="output"}`), 1500)
	near(t, value(t, out, `lmon_cost_usd_total{model="claude-x",provider="anthropic"}`), 3*(1000*1e-6+500*2e-6))
}

// Not a real test: dumps a rich scrape for `promtool check metrics`.
//
//	LMON_DUMP_METRICS=/tmp/m.txt go test ./internal/metrics -run DumpForPromtool
func TestDumpForPromtool(t *testing.T) {
	path := os.Getenv("LMON_DUMP_METRICS")
	if path == "" {
		t.Skip("set LMON_DUMP_METRICS to dump a scrape")
	}
	r := metrics.New("v1.2.3", prices(t))
	r.Observe(event("claude-x", 200, 1500, 300, usage.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 100, WebSearchRequests: 2}, true))
	r.Observe(event("claude-x", 200, 45000, 900, usage.Usage{InputTokens: 10}, true))
	r.Observe(event("claude-mystery", 200, 100, 50, usage.Usage{InputTokens: 10}, true))
	r.Observe(event("", 429, 200, 0, usage.Usage{}, false))
	r.Observe(event("a\"b\\c\nd", 200, 1, 1, usage.Usage{}, false))
	r.SetProbe("api.example.com", probe.Result{
		DNS:    []probe.DNSResult{{Resolver: "system", IPs: []string{"1.2.3.4"}, Latency: probe.Duration(12 * time.Millisecond)}, {Resolver: "cloudflare", Error: "timeout"}},
		IPs:    []probe.IPInfo{{IP: "1.2.3.4", Geo: &probe.Geo{Country: "United States", City: "San Francisco"}}},
		Timing: probe.Timing{Status: 404, Total: probe.Duration(100 * time.Millisecond)},
	}, time.Now())
	done := r.Track()
	defer done()
	if err := os.WriteFile(path, []byte(scrape(r)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBytesCountersByDirection(t *testing.T) {
	r := metrics.New("v", nil)
	e := event("claude-x", 200, 100, 50, usage.Usage{InputTokens: 1}, true)
	e.BytesUp, e.BytesDown = 1200, 34000
	r.Observe(e)
	r.Observe(e)
	r.Observe(event("claude-x", 200, 100, 50, usage.Usage{InputTokens: 1}, true)) // no byte data: adds nothing
	out := scrape(r)
	near(t, value(t, out, `lmon_bytes_total{direction="up",model="claude-x",provider="anthropic"}`), 2400)
	near(t, value(t, out, `lmon_bytes_total{direction="down",model="claude-x",provider="anthropic"}`), 68000)

	none := metrics.New("v", nil)
	none.Observe(event("claude-x", 200, 100, 50, usage.Usage{InputTokens: 1}, true))
	if strings.Contains(scrape(none), "lmon_bytes_total") {
		t.Error("no byte series without byte data")
	}
}
