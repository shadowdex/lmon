package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/shadowdex/lmon/internal/claudecode"
	"github.com/shadowdex/lmon/internal/geoip"
	"github.com/shadowdex/lmon/internal/pricing"
	"github.com/shadowdex/lmon/internal/probe"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
	"github.com/shadowdex/lmon/internal/tui"
)

// Set by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const usageText = `lmon - LLM usage, latency and endpoint monitor

Usage:
  lmon proxy [--port 8787] [--log PATH] [--max-size-mb 50] [--keep 3]
                                          run the local recording proxy (rotates its log by size)
  lmon stats [--since 24h] [--json] [--claude]
                                          summarize recorded calls (--claude adds Claude Code sessions)
  lmon top [--window 15m] [--claude]      live terminal view (keys: w window, s sort, p pause, q quit)
  lmon probe <host> [--geoip FILE] [--json]
                                          DNS, geo and connection timing for an endpoint
  lmon prices update                      download model prices (enables cost estimates)
  lmon prices show <provider> <model>     show the rates lmon would use for a model
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
	case "version", "--version", "-v":
		fmt.Printf("lmon %s (%s)\n", version, commit)
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
	fs.Parse(args)
	if *maxMB < 0 || *keep < 0 {
		return fmt.Errorf("--max-size-mb and --keep must not be negative")
	}

	lg, err := proxy.NewRotatingLogger(*logPath, proxy.Rotation{MaxBytes: int64(*maxMB) << 20, Keep: *keep})
	if err != nil {
		return err
	}
	lg.OnError = func(err error) { fmt.Fprintln(os.Stderr, "lmon: log:", err) }
	defer lg.Close()

	// Bind to loopback only: the proxy forwards your API keys.
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: proxy.Handler(lg, nil)}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	fmt.Fprintf(os.Stderr, "lmon proxy listening on http://%s, logging to %s\n", srv.Addr, *logPath)
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
	fs.Parse(args)

	files := proxy.LogFiles(*logPath)
	if len(files) == 0 && !*withClaude {
		return fmt.Errorf("no events yet at %s; run `lmon proxy` and send some requests (or add --claude)", *logPath)
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
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tCALLS\tERR\tINPUT\tOUTPUT\tCACHE-R\tCACHE-W\tHIT%\tAVG\tP50\tP95\tTTFB"+costHdr)
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
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.0f%%\t%s\t%s\t%s\t%s%s\n",
			r.Provider, r.Model, r.Calls, r.Errors, r.Input, r.Output, r.CacheRead, r.CacheWrite,
			r.CacheHitRate*100, ms(r.AvgTotalMs), ms(r.P50TotalMs), ms(r.P95TotalMs), ms(r.AvgTTFBMs), cost)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if *withClaude {
		fmt.Fprintln(os.Stderr, "\nnote: Claude Code rows come from its session logs, which omit some billed calls, so they are approximate and usually slightly low; they also have no latency (-)")
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
	fs.Parse(args)

	if !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("top needs an interactive terminal; use `lmon stats` for piped output")
	}
	var sc *claudecode.Scanner
	if *withClaude {
		var err error
		if sc, err = claudeScanner(*claudeDir, time.Now().Add(-tui.Retention())); err != nil {
			return err
		}
	}
	_, err := tea.NewProgram(tui.New(*logPath, *window, loadPrices(), sc)).Run()
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
