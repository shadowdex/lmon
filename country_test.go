package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shadowdex/lmon/internal/netpath"
	"github.com/shadowdex/lmon/internal/proxy"
	"github.com/shadowdex/lmon/internal/stats"
	"github.com/shadowdex/lmon/internal/usage"
)

var now0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func call(prov, model string, in, out int, up, down int64, src string) proxy.Event {
	return proxy.Event{Time: now0, Provider: prov, Source: src, Status: 200, HasUsage: true, BytesUp: up, BytesDown: down,
		Usage: usage.Usage{Model: model, InputTokens: in, OutputTokens: out}}
}

func testPaths() map[string]netpath.Summary {
	return map[string]netpath.Summary{
		"api.anthropic.com": {Host: "api.anthropic.com", Countries: []string{"FR"}, EdgeKind: "nearby", EdgeISO: "FR", EdgeCity: "Paris", SavedAt: now0.Add(-3 * time.Hour)},
		"api.openai.com":    {Host: "api.openai.com", Countries: []string{"FR", "GB"}, EdgeKind: "consistent", EdgeISO: "US", EdgeCity: "Seattle", SavedAt: now0.Add(-3 * time.Hour)},
	}
}

func TestPrintByCountryTable(t *testing.T) {
	events := []proxy.Event{
		call("anthropic", "c", 9_100_000, 310_000, 38_000_000, 2_100_000, ""),
		call("openai", "g", 1_200_000, 41_000, 5_000_000, 300_000, ""),
		call("groq", "l", 1000, 10, 0, 0, ""), // never traced
	}
	var b bytes.Buffer
	if err := printByCountry(&b, stats.ByCountry(events, testPaths()), true, false, now0); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"COUNTRY", "TOKENS IN", "BYTES UP", "PROVIDERS",
		"FR", "GB", "anthropic, openai",
		"10.3M", "351k", "43 MB", "2.4 MB", // FR carries both providers: 9.1M+1.2M in, 310k+41k out, 38+5 MB up, 2.1+0.3 MB down
		"43 MB*", // the last row covers the groq call too, which has no byte data
		"(no path data)",
		"beyond the edge: not observable",
		"oldest measured 3h ago",
		"add up to more than the total",
		"not observable from here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPrintByCountryMarksPartialByteCoverageAndDashes(t *testing.T) {
	events := []proxy.Event{
		call("anthropic", "c", 100, 10, 1000, 5000, ""),
		call("anthropic", "c", 100, 10, 0, 0, "claude-code"),
	}
	var b bytes.Buffer
	printByCountry(&b, stats.ByCountry(events, testPaths()), true, false, now0)
	if out := b.String(); !strings.Contains(out, "1.0 kB*") || !strings.Contains(out, "* bytes cover only calls that went through the proxy") {
		t.Fatalf("partial coverage must be marked:\n%s", out)
	}
	b.Reset()
	printByCountry(&b, stats.ByCountry([]proxy.Event{call("anthropic", "c", 5, 1, 0, 0, "claude-code")}, testPaths()), true, false, now0)
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(l, "FR") && !strings.Contains(l, " - ") {
			t.Errorf("a row with no byte data should show dashes: %q", l)
		}
	}
}

func TestPrintByCountryWithoutAnySavedPaths(t *testing.T) {
	var b bytes.Buffer
	printByCountry(&b, stats.ByCountry([]proxy.Event{call("anthropic", "c", 10, 1, 0, 0, "")}, nil), false, false, now0)
	out := b.String()
	if !strings.Contains(out, "No saved paths yet. Run `lmon path`") || !strings.Contains(out, "(no path data)") {
		t.Fatalf("%s", out)
	}
}

func TestPrintByCountryJSONAndEmpty(t *testing.T) {
	var b bytes.Buffer
	printByCountry(&b, stats.ByCountry([]proxy.Event{call("anthropic", "c", 10, 1, 7, 9, "")}, testPaths()), true, true, now0)
	var rep stats.CountryReport
	if err := json.Unmarshal(b.Bytes(), &rep); err != nil || len(rep.Rows) != 1 || rep.Rows[0].Label != "FR" || rep.Total.BytesDown != 9 {
		t.Fatalf("%v %s", err, b.String())
	}
	b.Reset()
	printByCountry(&b, stats.ByCountry(nil, nil), false, false, now0)
	if !strings.Contains(b.String(), "no calls in this window") {
		t.Fatalf("%s", b.String())
	}
}

func TestEntersOf(t *testing.T) {
	p := testPaths()
	if got := entersOf(p, "anthropic"); got != "FR Paris" {
		t.Errorf("anthropic: %q", got)
	}
	if got := entersOf(p, "openai"); got != "≈ US Seattle" {
		t.Errorf("openai: %q", got)
	}
	if got := entersOf(p, "groq"); got != "-" {
		t.Errorf("untraced provider: %q", got)
	}
	if got := entersOf(p, "no-such-provider"); got != "-" {
		t.Errorf("unknown provider: %q", got)
	}
}

func TestSeenProvidersGivesSortedUniqueHostsAndIgnoresUnknown(t *testing.T) {
	var s seenProviders
	if len(s.hosts()) != 0 {
		t.Fatal("empty at first")
	}
	for _, p := range []string{"openai", "anthropic", "openai", "mystery", ""} {
		s.add(p)
	}
	got := strings.Join(s.hosts(), ",")
	if got != "api.anthropic.com,api.openai.com" {
		t.Fatalf("%q", got)
	}
}
