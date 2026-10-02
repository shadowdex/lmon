package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/shadowdex/lmon/internal/claudecode"
	"github.com/shadowdex/lmon/internal/codex"
	"github.com/shadowdex/lmon/internal/geoip"
	"github.com/shadowdex/lmon/internal/metrics"
	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/probe"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
	"github.com/shadowdex/lmon/internal/tui"
	"github.com/shadowdex/lmon/internal/usage"
)

// Set by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const usageText = `lmon - LLM usage, latency and endpoint monitor

Usage:
  lmon proxy [--port 8787] [--log PATH] [--max-size-mb 50] [--keep 3] [--probe-host H] [--metrics-addr :9464]
                                          run the local recording proxy (rotates its log by size;
                                          Prometheus metrics at /metrics)
  lmon stats [--since 24h] [--json] [--claude] [--codex] [--by-country]
                                          summarize recorded calls (--claude / --codex add those tools' sessions;
                                          --by-country breaks them down by country)
  lmon top [--window 15m] [--claude] [--codex]  live terminal view (keys: w window, s sort, p pause, q quit)
  lmon probe <host> [--geoip FILE] [--json]
                                          DNS, geo and connection timing for an endpoint
  lmon prices update                      download model prices (enables cost estimates)
  lmon prices show <provider> <model>     show the rates lmon would use for a model
  lmon path [host...] [--html map.html] [-v] [--json]
                                          which countries the route to an endpoint crosses
                                          (default: every provider endpoint lmon knows)
  lmon geoip update                       download the free DB-IP city database (~/.lmon)
  lmon geoip path                         print the database path in use
  lmon version

Point your SDK at the proxy:
  ANTHROPIC_BASE_URL=http://localhost:8787/anthropic
  OPENAI_BASE_URL=http://localhost:8787/openai/v1
  (also: /xai /groq /mistral /deepseek /openrouter)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "proxy":
		err = runProxy(args)
	case "stats":
		err = runStats(args)
	case "probe":
		err = runProbe(args)
	case "top":
		err = runTop(args)
	case "geoip":
		err = runGeoIP(args)
	case "prices":
		err = runPrices(args)
	case "path":
		err = runPath(args)
	case "version", "--version", "-v":
		v, c := versionInfo()
		fmt.Printf("lmon %s (%s)\n", v, c)
	case "help", "--help", "-h":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "lmon: unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lmon:", err)
		os.Exit(1)
	}
}

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	port := fs.Int("port", 8787, "listen port")
	logPath := fs.String("log", proxy.DefaultLogPath(), "event log (JSONL)")
	maxMB := fs.Int("max-size-mb", int(proxy.DefaultRotation.MaxBytes>>20), "rotate the log at this size in MB (0 = never rotate)")
	keep := fs.Int("keep", proxy.DefaultRotation.Keep, "rotated log files to keep")
	metricsAddr := fs.String("metrics-addr", "", "also serve /metrics on this separate address, e.g. 0.0.0.0:9464 so a Prometheus in Docker can scrape it (metrics hold no API keys, but do show models and usage)")
	probeHosts := fs.String("probe-host", "", "comma-separated hosts to probe in the background for /metrics (e.g. api.anthropic.com); makes outbound HTTPS requests")
	probeEvery := fs.Duration("probe-every", time.Minute, "how often to probe --probe-host hosts")
	pathEvery := fs.Duration("path-every", 0, "re-trace the route to each provider you have used, this often (e.g. 1h), so `lmon top` and `lmon stats --by-country` stay current; 0 = off. Sends traceroute probes.")
	fs.Parse(args)
	if *maxMB < 0 || *keep < 0 {
		return fmt.Errorf("--max-size-mb and --keep must not be negative")
	}
	if *probeEvery < 10*time.Second {
		return fmt.Errorf("--probe-every must be at least 10s")
	}
	if *pathEvery != 0 && *pathEvery < 5*time.Minute {
		return fmt.Errorf("--path-every must be at least 5m (or 0 to turn it off)")
	}
	var hosts []string
	for _, h := range strings.Split(*probeHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://"), "/"))
		}
	}

	lg, err := proxy.NewRotatingLogger(*logPath, proxy.Rotation{MaxBytes: int64(*maxMB) << 20, Keep: *keep})
	if err != nil {
		return err
	}
	lg.OnError = func(err error) { fmt.Fprintln(os.Stderr, "lmon: log:", err) }
	defer lg.Close()

	// Prices reload on their own, so `lmon prices update` needs no restart.
	prices := &pricing.Reloader{Path: pricing.DefaultPath()}
	v, _ := versionInfo()
	reg := metrics.New(v, prices.Get)
	seen := &seenProviders{}
	lg.OnEvent = func(e proxy.Event) {
		reg.Observe(e)
		seen.add(e.Provider)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(hosts) > 0 {
		go reg.RunProbes(ctx, hosts, *probeEvery, func() string { return geoip.Resolve(os.Getenv("LMON_GEOIP_DB")) }, 10*time.Second)
	}

	if *pathEvery > 0 {
		go refreshPathsLoop(ctx, *pathEvery, seen)
	}

	api := proxy.Handler(lg, nil)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			reg.ServeHTTP(w, r)
			return
		}
		defer reg.Track()()
		api.ServeHTTP(w, r)
	})

	// Bind to loopback only: the proxy forwards your API keys.
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: handler}
	var msrv *http.Server
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", reg)
		msrv = &http.Server{Addr: *metricsAddr, Handler: mux}
		go func() {
			if err := msrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "lmon: metrics listener:", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "lmon metrics also on http://%s/metrics\n", *metricsAddr)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if msrv != nil {
			msrv.Shutdown(sctx)
		}
		srv.Shutdown(sctx)
	}()
	fmt.Fprintf(os.Stderr, "lmon proxy listening on http://%s, logging to %s, metrics at /metrics\n", srv.Addr, *logPath)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	logPath := fs.String("log", proxy.DefaultLogPath(), "event log (JSONL)")
	since := fs.Duration("since", 24*time.Hour, "only include calls newer than this (0 = all)")
	asJSON := fs.Bool("json", false, "JSON output")
	withClaude := fs.Bool("claude", false, "also include Claude Code sessions, read from its own logs (no latency data)")
	claudeDir := fs.String("claude-dir", "", "Claude config dir(s), comma separated (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
	withCodex := fs.Bool("codex", false, "also include Codex sessions, read from its own logs (no latency data)")
	byCountry := fs.Bool("by-country", false, "break traffic down by the countries on each provider's path (run `lmon path` first)")
	fs.Parse(args)

	files := proxy.LogFiles(*logPath)
	if len(files) == 0 && !*withClaude && !*withCodex {
		return fmt.Errorf("no events yet at %s; run `lmon proxy` and send some requests (or add --claude or --codex)", *logPath)
	}
	var cutoff time.Time
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}
	events, err := stats.LoadFiles(files, cutoff)
	if err != nil {
		return err
	}
	if *withClaude {
		sc, err := claudeScanner(*claudeDir, cutoff)
		if err != nil {
			return err
		}
		if _, err := sc.Poll(); err != nil {
			fmt.Fprintln(os.Stderr, "lmon: claude logs:", err)
		}
		events = stats.Merge(events, sc.All())
	}
	if *withCodex {
		sc, err := codexScanner(cutoff)
		if err != nil {
			return err
		}
		if _, err := sc.Poll(); err != nil {
			fmt.Fprintln(os.Stderr, "lmon: codex logs:", err)
		}
		events = stats.Merge(events, sc.All())
	}
	paths := loadPaths()
	if *byCountry {
		return printByCountry(os.Stdout, stats.ByCountry(events, paths), len(paths) > 0, *asJSON, time.Now())
	}
	tbl := loadPrices()
	rows := stats.AggregateWithPrices(events, tbl)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no calls in this window")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	costHdr := ""
	if tbl != nil {
		costHdr = "\tCOST"
	}
	entersHdr := ""
	if len(paths) > 0 {
		entersHdr = "\tENTERS" // where traffic enters the provider; from `lmon path`
	}
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tCALLS\tERR\tINPUT\tOUTPUT\tCACHE-R\tCACHE-W\tHIT%\tAVG\tP50\tP95\tTTFB"+costHdr+entersHdr)
	var total float64
	var anyUnpriced bool
	for _, r := range rows {
		total += r.CostUSD
		anyUnpriced = anyUnpriced || r.UnpricedCalls > 0
		cost := ""
		if tbl != nil {
			cost = "\t" + r.CostLabel()
		}
		ms := func(v float64) string {
			if r.LatencyCalls == 0 {
				return "-" // e.g. Claude Code sessions: the proxy never saw them
			}
			return fmt.Sprintf("%.0fms", v)
		}
		enters := ""
		if len(paths) > 0 {
			enters = "\t" + entersOf(paths, r.Provider)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.0f%%\t%s\t%s\t%s\t%s%s%s\n",
			r.Provider, r.Model, r.Calls, r.Errors, r.Input, r.Output, r.CacheRead, r.CacheWrite,
			r.CacheHitRate*100, ms(r.AvgTotalMs), ms(r.P50TotalMs), ms(r.P95TotalMs), ms(r.AvgTTFBMs), cost, enters)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if *withClaude {
		fmt.Fprintln(os.Stderr, "\nnote: Claude Code rows come from its session logs, which omit some billed calls, so they are approximate and usually slightly low; they also have no latency (-)")
	}
	if *withCodex {
		fmt.Fprintln(os.Stderr, "\nnote: Codex rows come from its session logs, so they have no latency (-); cost is the API list price, not what a ChatGPT plan charges")
	}
	switch {
	case tbl == nil:
		fmt.Fprintln(os.Stderr, "\ncost estimates are off: run `lmon prices update` to enable them")
	case anyUnpriced:
		fmt.Printf("\nestimated total %s* (* some calls could not be priced, so this is a lower bound)\n", pricing.FormatUSD(total))
	default:
		fmt.Printf("\nestimated total %s\n", pricing.FormatUSD(total))
	}
	return nil
}

// loadPrices returns the local price table, or nil if there isn't one. A
// corrupt table is reported but never stops stats/top from working.
func loadPrices() *pricing.Table {
	tbl, err := pricing.Load(pricing.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "lmon: prices:", err)
		return nil
	}
	if tbl != nil && tbl.Age() > 45*24*time.Hour {
		fmt.Fprintf(os.Stderr, "lmon: note: price table is %d days old; `lmon prices update` refreshes it\n", int(tbl.Age().Hours()/24))
	}
	return tbl
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	geoFlag := fs.String("geoip", os.Getenv("LMON_GEOIP_DB"), "path to a City .mmdb (default: ~/.lmon database from `lmon geoip update`, or $LMON_GEOIP_DB)")
	asJSON := fs.Bool("json", false, "JSON output")
	timeout := fs.Duration("timeout", 10*time.Second, "per-step timeout")
	// Allow the host before or after flags.
	var host string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	fs.Parse(args)
	if host == "" && fs.NArg() > 0 {
		host = fs.Arg(0)
	}
	if host == "" {
		return fmt.Errorf("usage: lmon probe <host> [--geoip FILE] [--json]")
	}
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")

	geo := geoip.Resolve(*geoFlag)
	res := probe.Run(context.Background(), host, geo, *timeout)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	fmt.Printf("%s\n\nDNS\n", res.Host)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, d := range res.DNS {
		out := strings.Join(d.IPs, ", ")
		if d.Error != "" {
			out = "error: " + d.Error
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Resolver, d.Latency, out)
	}
	tw.Flush()

	fmt.Println("\nIPs")
	for _, ip := range res.IPs {
		loc := ""
		if ip.Geo != nil {
			loc = strings.Trim(strings.Join([]string{ip.Geo.City, ip.Geo.Country}, ", "), ", ")
		}
		fmt.Printf("  %s  %s\n", ip.IP, loc)
	}
	if geo == "" {
		fmt.Println("  (no geo data: run `lmon geoip update`, or pass --geoip FILE)")
	} else if geo == geoip.DefaultPath() {
		fmt.Println("  " + geoip.Attribution)
	}

	t := res.Timing
	fmt.Println("\nConnection")
	if t.Error != "" {
		fmt.Println("  error:", t.Error)
	}
	fmt.Printf("  remote %s  status %d\n  dns %s  tcp %s  tls %s  ttfb %s  total %s\n",
		t.Remote, t.Status, t.DNS, t.Connect, t.TLS, t.TTFB, t.Total)
	return nil
}

func runGeoIP(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lmon geoip update | path")
	}
	switch args[0] {
	case "update":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		_, err := geoip.Update(ctx, geoip.Options{Out: os.Stderr})
		return err
	case "path":
		p := geoip.Resolve(os.Getenv("LMON_GEOIP_DB"))
		if p == "" {
			return fmt.Errorf("no database yet; run `lmon geoip update`")
		}
		fmt.Println(p)
		if age, err := geoip.Age(p); err == nil && age > 45*24*time.Hour {
			fmt.Fprintf(os.Stderr, "note: database is %d days old; `lmon geoip update` refreshes it (published monthly)\n", int(age.Hours()/24))
		}
		return nil
	}
	return fmt.Errorf("unknown geoip command %q (want update or path)", args[0])
}

func runTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	logPath := fs.String("log", proxy.DefaultLogPath(), "event log (JSONL)")
	window := fs.Duration("window", 15*time.Minute, "initial window: rounds up to 5m, 15m, 1h or 24h")
	withClaude := fs.Bool("claude", false, "also include Claude Code sessions, read live from its own logs")
	claudeDir := fs.String("claude-dir", "", "Claude config dir(s), comma separated (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
	withCodex := fs.Bool("codex", false, "also include Codex sessions, read live from its own logs")
	fs.Parse(args)

	if !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("top needs an interactive terminal; use `lmon stats` for piped output")
	}
	var imp tui.Importer
	since := time.Now().Add(-tui.Retention())
	if *withClaude || *withCodex {
		var multi multiImport
		if *withClaude {
			sc, err := claudeScanner(*claudeDir, since)
			if err != nil {
				return err
			}
			multi = append(multi, namedScanner{sc, claudecode.Source})
		}
		if *withCodex {
			sc, err := codexScanner(since)
			if err != nil {
				return err
			}
			multi = append(multi, namedScanner{sc, codex.Source})
		}
		imp = multi
	}
	_, err := tea.NewProgram(tui.New(*logPath, *window, loadPrices(), imp, loadPathsQuiet)).Run()
	return err
}

func runPrices(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lmon prices update | show <provider> <model>")
	}
	switch args[0] {
	case "update":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		_, err := pricing.Update(ctx, pricing.UpdateOptions{Out: os.Stderr})
		return err
	case "show":
		if len(args) != 3 {
			return fmt.Errorf("usage: lmon prices show <provider> <model>")
		}
		tbl, err := pricing.Load(pricing.DefaultPath())
		if err != nil {
			return err
		}
		if tbl == nil {
			return fmt.Errorf("no price table yet; run `lmon prices update`")
		}
		key, e, ok := tbl.Lookup(args[1], args[2])
		if !ok {
			msg := fmt.Sprintf("no price for %s model %q", args[1], args[2])
			if sug := tbl.Suggest(args[1], args[2], 8); len(sug) > 0 {
				msg += "; similar: " + strings.Join(sug, ", ")
			}
			return fmt.Errorf("%s", msg)
		}
		fmt.Printf("%s (%s), USD per million tokens, table updated %s\n", key, e.Provider, tbl.Updated.Format("2006-01-02"))
		printRates := func(label string, r pricing.Rates) {
			fmt.Printf("  %-12s input %-8s output %-8s cache-read %-8s cache-write %-8s cache-write-1h %s\n", label,
				perM(r.Input), perM(r.Output), perM(r.CacheRead), perM(r.CacheWrite), perM(r.CacheWrite1h))
		}
		printRates("base", e.Rates(0))
		for _, t := range e.Tiers {
			printRates(fmt.Sprintf("> %dk input", t.Above/1000), e.Rates(t.Above+1))
		}
		return nil
	}
	return fmt.Errorf("unknown prices command %q (want update or show)", args[0])
}

func perM(perToken float64) string { return fmt.Sprintf("$%.4g", perToken*1e6) }

// claudeScanner builds a scanner over Claude Code's project logs. dirs is a
// comma-separated list of Claude config dirs ("" = the defaults).
func claudeScanner(dirs string, since time.Time) (*claudecode.Scanner, error) {
	roots := claudecode.DefaultRoots()
	if dirs != "" {
		roots = nil
		for _, d := range strings.Split(dirs, ",") {
			if d = strings.TrimSpace(d); d != "" {
				roots = append(roots, filepath.Join(d, "projects"))
			}
		}
	}
	var found []string
	for _, r := range roots {
		if st, err := os.Stat(r); err == nil && st.IsDir() {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no Claude Code logs found (looked for a projects/ dir in $CLAUDE_CONFIG_DIR, ~/.claude, ~/.config/claude); use --claude-dir")
	}
	return &claudecode.Scanner{Roots: found, Since: since}, nil
}

// codexScanner builds a scanner over Codex's session logs ($CODEX_HOME or ~/.codex).
func codexScanner(since time.Time) (*codex.Scanner, error) {
	roots := codex.DefaultRoots()
	if len(roots) == 0 {
		return nil, fmt.Errorf("no Codex logs found (looked for a sessions/ dir in $CODEX_HOME, ~/.codex)")
	}
	return &codex.Scanner{Roots: roots, Since: since}, nil
}

// namedScanner is a log-reading scanner plus its source name.
type namedScanner struct {
	poller interface{ Poll() ([]proxy.Event, error) }
	name   string
}

// multiImport polls several log-reading scanners as one tui.Importer.
type multiImport []namedScanner

func (m multiImport) Poll() ([]proxy.Event, error) {
	var out []proxy.Event
	var firstErr error
	for _, s := range m {
		evs, err := s.poller.Poll()
		out = append(out, evs...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out, firstErr
}

func (m multiImport) Label() string {
	names := make([]string, len(m))
	for i, s := range m {
		names[i] = s.name
	}
	return strings.Join(names, ", ")
}

func parseFrom(s string) (*netpath.Point, error) {
	var lat, lon float64
	if _, err := fmt.Sscanf(strings.ReplaceAll(s, " ", ""), "%f,%f", &lat, &lon); err != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, fmt.Errorf("--from wants LAT,LON such as 48.85,2.35, got %q", s)
	}
	return &netpath.Point{Lat: lat, Lon: lon, Label: fmt.Sprintf("%.2f, %.2f", lat, lon), Source: "given"}, nil
}

func runPath(args []string) error {
	fs := flag.NewFlagSet("path", flag.ExitOnError)
	v4 := fs.Bool("4", false, "trace the IPv4 address")
	v6 := fs.Bool("6", false, "trace the IPv6 address")
	asJSON := fs.Bool("json", false, "JSON output")
	verbose := fs.Bool("v", false, "show every hop")
	htmlOut := fs.String("html", "", "also write a self-contained HTML page with a world map to this file")
	noSave := fs.Bool("no-save", false, "don't save a summary for `lmon top` and `lmon stats --by-country`")
	from := fs.String("from", os.Getenv("LMON_FROM"), "your location as LAT,LON, e.g. 43.60,1.44 (default $LMON_FROM, else estimated from your first public router)")
	geoFlag := fs.String("geoip", os.Getenv("LMON_GEOIP_DB"), "path to a City .mmdb (default: the database from `lmon geoip update`)")
	noRDNS := fs.Bool("no-rdns", false, "don't look up router hostnames (they make locations more accurate)")
	maxHops := fs.Int("max-hops", 30, "give up after this many hops")
	timeout := fs.Duration("timeout", 90*time.Second, "time allowed per endpoint")
	parallel := fs.Int("parallel", 3, "endpoints to trace at once")

	// Allow flags before and after host names.
	var hosts []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(args[0], "https://"), "http://"), "/")
		hosts = append(hosts, h)
		args = args[1:]
	}
	if *v4 && *v6 {
		return fmt.Errorf("-4 and -6 are mutually exclusive")
	}
	family := 0
	if *v4 {
		family = 4
	} else if *v6 {
		family = 6
	}
	if len(hosts) == 0 {
		hosts = usage.ProviderHosts()
	}
	var vantage *netpath.Point
	if *from != "" {
		var err error
		if vantage, err = parseFrom(*from); err != nil {
			return err
		}
	}

	var geo netpath.Locator
	attribution := ""
	if path := geoip.Resolve(*geoFlag); path != "" {
		if path == geoip.DefaultPath() {
			attribution = geoip.Attribution
		}
		db, err := geoip.Open(path)
		if err != nil {
			return fmt.Errorf("opening GeoIP database %s: %w", path, err)
		}
		defer db.Close()
		geo = db
	} else {
		fmt.Fprintln(os.Stderr, "lmon: no GeoIP database, so locations come only from router hostnames. Run `lmon geoip update` for much better results.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	results := make([]netpath.Path, len(hosts))
	errs := make([]error, len(hosts))
	sem := make(chan struct{}, max(1, *parallel))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h string) {
			defer wg.Done()
			defer func() { <-sem }()
			fmt.Fprintf(os.Stderr, "tracing %s ...\n", h)
			hctx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			results[i], errs[i] = netpath.Run(hctx, h, netpath.Options{
				Family: family, Runner: netpath.Runner{MaxHops: *maxHops},
				Geo: geo, NoRDNS: *noRDNS, Vantage: vantage,
			})
		}(i, h)
	}
	wg.Wait()

	var paths []netpath.Path
	reported := map[string]bool{}
	for i := range hosts {
		if errs[i] != nil {
			msg := errs[i].Error()
			var nt netpath.ErrNoTraceroute
			if errors.As(errs[i], &nt) {
				msg = nt.Error() // the same for every host: say it once
			}
			if !reported[msg] {
				reported[msg] = true
				fmt.Fprintln(os.Stderr, "lmon:", msg)
			}
			continue
		}
		paths = append(paths, results[i])
	}
	if len(paths) == 0 {
		return fmt.Errorf("no endpoint could be traced")
	}
	if vantage == nil {
		labels := map[string]bool{}
		for _, p := range paths {
			if p.Vantage != nil {
				labels[p.Vantage.Label] = true
			}
		}
		if len(labels) > 1 {
			fmt.Fprintln(os.Stderr, "lmon: note: your location was estimated differently for different endpoints (IPv4 and IPv6 routers geolocate differently). Set it once with --from LAT,LON or $LMON_FROM for consistent results.")
		}
	}

	if !*noSave {
		store := netpath.Store{Path: netpath.DefaultStorePath()}
		now := time.Now()
		var sums []netpath.Summary
		for _, p := range paths {
			sums = append(sums, p.Summary(now))
		}
		if err := store.Save(sums...); err != nil {
			fmt.Fprintln(os.Stderr, "lmon: could not save path summaries:", err)
		} else {
			fmt.Fprintf(os.Stderr, "saved %d path summaries to %s (used by `lmon top` and `lmon stats --by-country`)\n", len(sums), store.Path)
		}
	}
	if *htmlOut != "" {
		f, err := os.Create(*htmlOut)
		if err != nil {
			return err
		}
		werr := netpath.WriteHTML(f, paths, attribution)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("writing %s: %w", *htmlOut, werr)
		}
		fmt.Fprintf(os.Stderr, "wrote %s (open it in a browser)\n", *htmlOut)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(paths)
	}
	fmt.Print(netpath.RenderText(paths, *verbose))
	if attribution != "" {
		fmt.Println("\n" + attribution)
	}
	return nil
}

// refreshPathsLoop re-traces the route to every provider that has had traffic,
// once a minute after start (to catch the first ones) and then every interval.
func refreshPathsLoop(ctx context.Context, every time.Duration, seen *seenProviders) {
	run := func() {
		hosts := seen.hosts()
		if len(hosts) == 0 {
			return
		}
		var geo netpath.Locator
		if path := geoip.Resolve(os.Getenv("LMON_GEOIP_DB")); path != "" {
			if db, err := geoip.Open(path); err == nil {
				defer db.Close()
				geo = db
			}
		}
		var vantage *netpath.Point
		if from := os.Getenv("LMON_FROM"); from != "" {
			vantage, _ = parseFrom(from)
		}
		_, errs := netpath.Refresh(ctx, hosts, netpath.Options{Geo: geo, Vantage: vantage},
			netpath.Store{Path: netpath.DefaultStorePath()}, 90*time.Second, nil)
		for _, err := range errs {
			fmt.Fprintln(os.Stderr, "lmon: path refresh:", err)
		}
	}
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			run()
		case <-tick.C:
			run()
		}
	}
}

// loadPathsQuiet is loadPaths for the live view: errors must not scribble over
// the screen, so an unreadable file just means no route information.
func loadPathsQuiet() map[string]netpath.Summary {
	paths, err := netpath.Store{Path: netpath.DefaultStorePath()}.Load()
	if err != nil {
		return nil
	}
	return paths
}
