package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shadowdex/lmon/internal/probe"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
)

// Set by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const usageText = `lmon - LLM usage, latency and endpoint monitor

Usage:
  lmon proxy [--port 8787] [--log PATH]   run the local recording proxy
  lmon stats [--since 24h] [--json]       summarize recorded calls
  lmon probe <host> [--geoip FILE] [--json]
                                          DNS, geo and connection timing for an endpoint
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
	fs.Parse(args)

	lg, err := proxy.NewLogger(*logPath)
	if err != nil {
		return err
	}
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
	fs.Parse(args)

	f, err := os.Open(*logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no events yet at %s; run `lmon proxy` and send some requests", *logPath)
		}
		return err
	}
	defer f.Close()
	var cutoff time.Time
	if *since > 0 {
		cutoff = time.Now().Add(-*since)
	}
	events, err := stats.Load(f, cutoff)
	if err != nil {
		return err
	}
	rows := stats.Aggregate(events)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no calls in this window")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tMODEL\tCALLS\tERR\tINPUT\tOUTPUT\tCACHE-R\tCACHE-W\tHIT%\tAVG\tP50\tP95\tTTFB")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.0f%%\t%.0fms\t%.0fms\t%.0fms\t%.0fms\n",
			r.Provider, r.Model, r.Calls, r.Errors, r.Input, r.Output, r.CacheRead, r.CacheWrite,
			r.CacheHitRate*100, r.AvgTotalMs, r.P50TotalMs, r.P95TotalMs, r.AvgTTFBMs)
	}
	return tw.Flush()
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	geo := fs.String("geoip", os.Getenv("LMON_GEOIP_DB"), "path to a GeoLite2-City .mmdb (or $LMON_GEOIP_DB)")
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

	res := probe.Run(context.Background(), host, *geo, *timeout)
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
	if *geo == "" {
		fmt.Println("  (no geo data: pass --geoip or set LMON_GEOIP_DB to a GeoLite2-City .mmdb)")
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
