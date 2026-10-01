package netpath

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func ips(h Hop) []string {
	var out []string
	for _, r := range h.Responders {
		out = append(out, r.IP.String())
	}
	return out
}

func TestParseUnixMacOSv4(t *testing.T) {
	hops := ParseUnix(fixture(t, "macos_v4.txt"))
	if len(hops) != 10 {
		t.Fatalf("got %d hops", len(hops))
	}
	// hop 1: three replies from one router, smallest RTT kept
	h := hops[0]
	if h.TTL != 1 || len(h.Responders) != 1 || h.Responders[0].Replies != 3 || h.Responders[0].MinRTT != 3.729 {
		t.Fatalf("hop 1: %+v", h)
	}
	// hop 2: "* 203.0.113.1 7.241 ms" then a continuation line from a second router (load balancing)
	h = hops[1]
	if h.Timeouts != 1 || strings.Join(ips(h), ",") != "203.0.113.1,203.0.113.2" {
		t.Fatalf("hop 2: %+v", h)
	}
	// hop 5 is entirely silent
	if !hops[4].Silent() || hops[4].Timeouts != 3 {
		t.Fatalf("hop 5: %+v", hops[4])
	}
	// hop 6: two lost probes, then one reply
	if h = hops[5]; h.Timeouts != 2 || len(h.Responders) != 1 || h.Responders[0].IP != ip("203.0.113.5") {
		t.Fatalf("hop 6: %+v", h)
	}
	// hop 9: three different routers on continuation lines
	if h = hops[8]; len(h.Responders) != 3 || h.Responders[2].MinRTT != 9.734 {
		t.Fatalf("hop 9: %+v", h)
	}
	if h = hops[9]; h.Responders[0].MinRTT != 7.146 || h.Responders[0].Replies != 3 {
		t.Fatalf("hop 10: %+v", h)
	}
}

func TestParseUnixMacOSv6MixedStarsAndAddresses(t *testing.T) {
	hops := ParseUnix(fixture(t, "macos_v6.txt"))
	if len(hops) != 12 {
		t.Fatalf("got %d hops", len(hops))
	}
	// "4  *" then "    2001:db8::4  7.373 ms *": one timeout, a reply, another timeout
	h := hops[3]
	if h.Timeouts != 2 || len(h.Responders) != 1 || h.Responders[0].IP != ip("2001:db8::4") || h.Responders[0].Replies != 1 {
		t.Fatalf("hop 4: %+v", h)
	}
	if h = hops[5]; h.Responders[0].Replies != 1 || h.Timeouts != 2 {
		t.Fatalf("hop 6: %+v", h)
	}
	for _, i := range []int{6, 7, 8} {
		if !hops[i].Silent() {
			t.Errorf("hop %d should be silent", i+1)
		}
	}
	if len(hops[11].Responders) != 3 {
		t.Fatalf("hop 12: %+v", hops[11])
	}
}

func TestParseUnixDebianSingleLineLoadBalancingAndFlags(t *testing.T) {
	hops := ParseUnix(fixture(t, "debian_multi.txt"))
	if len(hops) != 5 {
		t.Fatalf("got %d hops", len(hops))
	}
	// Debian keeps every probe on one line, switching responder mid-line
	h := hops[1]
	if len(h.Responders) != 2 || h.Responders[0].IP != ip("203.0.113.1") || h.Responders[0].Replies != 2 || h.Responders[1].Replies != 1 {
		t.Fatalf("hop 2: %+v", h)
	}
	if h = hops[2]; h.Timeouts != 2 || len(h.Responders) != 1 {
		t.Fatalf("hop 3: %+v", h)
	}
	if h = hops[3]; len(h.Flags) != 1 || h.Flags[0] != "!H" || h.Timeouts != 2 {
		t.Fatalf("hop 4 should carry the !H flag: %+v", h)
	}
}

func TestParseUnixRealDockerCaptureMostlySilent(t *testing.T) {
	hops := ParseUnix(fixture(t, "debian_docker_silent.txt"))
	if len(hops) != 10 || hops[0].Silent() {
		t.Fatalf("got %d hops, first silent=%v", len(hops), hops[0].Silent())
	}
	for _, h := range hops[1:] {
		if !h.Silent() || h.Timeouts != 3 {
			t.Fatalf("hop %d: %+v", h.TTL, h)
		}
	}
}

func TestParseUnixIgnoresHostnamesAndZones(t *testing.T) {
	text := "traceroute to x (203.0.113.9), 30 hops max\n" +
		" 1  gw.example.net (192.168.0.1)  1.0 ms  1.1 ms\n" +
		" 2  fe80::1%en0  2.5 ms\n"
	hops := ParseUnix(text)
	if len(hops) != 2 || hops[0].Responders[0].IP != ip("192.168.0.1") || hops[0].Responders[0].Replies != 2 {
		t.Fatalf("named output: %+v", hops)
	}
	if hops[1].Responders[0].IP != ip("fe80::1") {
		t.Fatalf("zone must be stripped: %+v", hops[1])
	}
}

func TestParseUnixGarbage(t *testing.T) {
	for _, s := range []string{"", "\n\n", "traceroute: socket: Operation not permitted\n", "traceroute to x (1.2.3.4), 30 hops max\n", "random text 12 ms"} {
		if hops := ParseUnix(s); len(hops) != 0 {
			t.Errorf("%q parsed as %+v", s, hops)
		}
	}
}

func TestParseWindows(t *testing.T) {
	hops := ParseWindows(fixture(t, "windows_tracert.txt"))
	if len(hops) != 5 {
		t.Fatalf("got %d hops", len(hops))
	}
	if h := hops[0]; h.Responders[0].Replies != 3 || h.Responders[0].MinRTT != 0.5 {
		t.Fatalf("<1 ms should become 0.5: %+v", h)
	}
	if h := hops[1]; h.Responders[0].IP != ip("203.0.113.1") || h.Responders[0].MinRTT != 7 {
		t.Fatalf("hop 2: %+v", h)
	}
	if h := hops[2]; !h.Silent() || h.Timeouts != 3 {
		t.Fatalf("Request timed out: %+v", h)
	}
	if h := hops[3]; h.Timeouts != 1 || h.Responders[0].Replies != 2 {
		t.Fatalf("hop 4: %+v", h)
	}
}

func TestParseWindowsIPv6WithZoneAndHeaderLines(t *testing.T) {
	hops := ParseWindows(fixture(t, "windows_tracert_v6.txt"))
	if len(hops) != 4 {
		t.Fatalf("got %d hops", len(hops))
	}
	if hops[2].Responders[0].IP != ip("fe80::1") {
		t.Fatalf("zone not stripped: %+v", hops[2])
	}
	if hops[3].Responders[0].IP != ip("2001:db8::ffff") {
		t.Fatalf("hop 4: %+v", hops[3])
	}
}

func TestPrimaryPicksMostRepliesThenLowestRTT(t *testing.T) {
	h := Hop{Responders: []Responder{{IP: ip("203.0.113.1"), MinRTT: 5, Replies: 1}, {IP: ip("203.0.113.2"), MinRTT: 9, Replies: 2}, {IP: ip("203.0.113.3"), MinRTT: 7, Replies: 2}}}
	p, ok := h.Primary()
	if !ok || p.IP != ip("203.0.113.3") {
		t.Fatalf("got %+v", p)
	}
	if _, ok := (Hop{}).Primary(); ok {
		t.Fatal("silent hop has no primary")
	}
}

func TestCommandPerPlatform(t *testing.T) {
	v4, v6 := ip("203.0.113.9"), ip("2001:db8::9")
	cases := []struct {
		goos string
		dest netip.Addr
		want string
	}{
		{"darwin", v4, "traceroute -n -q 2 -w 1 -m 30 203.0.113.9"},
		{"darwin", v6, "traceroute6 -n -q 2 -w 1 -m 30 2001:db8::9"},
		{"freebsd", v6, "traceroute6 -n -q 2 -w 1 -m 30 2001:db8::9"},
		{"linux", v4, "traceroute -n -q 2 -w 1 -m 30 203.0.113.9"},
		{"linux", v6, "traceroute -6 -n -q 2 -w 1 -m 30 2001:db8::9"},
		{"windows", v4, "tracert -4 -d -h 30 -w 1000 203.0.113.9"},
		{"windows", v6, "tracert -6 -d -h 30 -w 1000 2001:db8::9"},
	}
	for _, c := range cases {
		name, args := Runner{GOOS: c.goos}.defaults().command(c.dest)
		if got := name + " " + strings.Join(args, " "); got != c.want {
			t.Errorf("%s %v:\n got  %s\n want %s", c.goos, c.dest, got, c.want)
		}
	}
	// the wait is rounded up to whole seconds (macOS traceroute takes an integer)
	_, args := Runner{GOOS: "darwin", Wait: 1500 * time.Millisecond}.defaults().command(v4)
	if strings.Join(args, " ") != "-n -q 2 -w 2 -m 30 203.0.113.9" {
		t.Errorf("wait rounding: %v", args)
	}
}

func fakeExec(out string, err error) func(context.Context, string, ...string) ([]byte, error) {
	return func(context.Context, string, ...string) ([]byte, error) { return []byte(out), err }
}

func TestTraceReachedAndPartial(t *testing.T) {
	out := "traceroute to 203.0.113.250\n 1  192.168.0.1  1 ms\n 2  203.0.113.250  8 ms\n"
	r := Runner{GOOS: "linux", Exec: fakeExec(out, nil), Look: func(string) error { return nil }}
	tr, err := r.Trace(context.Background(), ip("203.0.113.250"))
	if err != nil || !tr.Reached || tr.Partial || len(tr.Hops) != 2 {
		t.Fatalf("%+v %v", tr, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the tool was killed by the deadline, but printed two hops first
	r.Exec = fakeExec("traceroute to x\n 1  192.168.0.1  1 ms\n 2  203.0.113.1  5 ms\n", errors.New("signal: killed"))
	tr, err = r.Trace(ctx, ip("203.0.113.250"))
	if err != nil || !tr.Partial || tr.Reached || len(tr.Hops) != 2 {
		t.Fatalf("partial output must be kept: %+v %v", tr, err)
	}
}

func TestTraceErrors(t *testing.T) {
	r := Runner{GOOS: "linux", Look: func(string) error { return errors.New("not found") }, Exec: fakeExec("", nil)}
	_, err := r.Trace(context.Background(), ip("203.0.113.1"))
	var nt ErrNoTraceroute
	if !errors.As(err, &nt) || !strings.Contains(err.Error(), "apt install traceroute") {
		t.Fatalf("want ErrNoTraceroute with an install hint, got %v", err)
	}
	for goos, want := range map[string]string{"darwin": "/usr/sbin/traceroute", "windows": "System32"} {
		_, err = Runner{GOOS: goos, Look: func(string) error { return errors.New("x") }}.Trace(context.Background(), ip("203.0.113.1"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s hint: %v", goos, err)
		}
	}

	r = Runner{GOOS: "linux", Look: func(string) error { return nil }, Exec: fakeExec("traceroute: socket: Operation not permitted\n", errors.New("exit status 1"))}
	_, err = r.Trace(context.Background(), ip("203.0.113.1"))
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("the tool's own message must reach the user: %v", err)
	}
}

func traceWith(t *testing.T, out string, maxHops int, ctx context.Context) Trace {
	t.Helper()
	r := Runner{GOOS: "linux", MaxHops: maxHops, Exec: fakeExec(out, nil), Look: func(string) error { return nil }}
	tr, err := r.Trace(ctx, ip("203.0.113.250"))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Real behaviour seen against an anycast endpoint: the trace ended early with a
// reply from a different address than the one probed.
func TestEarlyEndOnAPlainReplyCountsAsReached(t *testing.T) {
	out := "traceroute to x\n 1  192.168.0.1  1 ms\n 2  203.0.113.7  5 ms\n 3  203.0.113.99  6 ms\n"
	tr := traceWith(t, out, 30, context.Background())
	if !tr.Reached || tr.Terminal != ip("203.0.113.99") {
		t.Fatalf("%+v", tr)
	}
}

func TestExactDestinationMatchLeavesTerminalUnset(t *testing.T) {
	tr := traceWith(t, "traceroute to x\n 1  192.168.0.1  1 ms\n 2  203.0.113.250  5 ms\n", 30, context.Background())
	if !tr.Reached || tr.Terminal.IsValid() {
		t.Fatalf("%+v", tr)
	}
}

func TestEndsThatAreNotReaching(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		out  string
		max  int
		ctx  context.Context
	}{
		{"last hop silent", "x\n 1  192.168.0.1  1 ms\n 2  203.0.113.7  5 ms\n 3  * * *\n", 30, context.Background()},
		{"an unreachable flag", "x\n 1  192.168.0.1  1 ms\n 2  203.0.113.7  5 ms !H\n", 30, context.Background()},
		{"hit the hop limit", "x\n 1  192.168.0.1  1 ms\n 2  203.0.113.7  5 ms\n 3  203.0.113.8  6 ms\n", 3, context.Background()},
		{"cut off by the timeout", "x\n 1  192.168.0.1  1 ms\n 2  203.0.113.7  5 ms\n", 30, cancelled},
	}
	for _, c := range cases {
		if tr := traceWith(t, c.out, c.max, c.ctx); tr.Reached || tr.Terminal.IsValid() {
			t.Errorf("%s: must not count as reached: %+v", c.name, tr)
		}
	}
}

func TestRealV6FixtureEndsOnADifferentAddressAndIsReached(t *testing.T) {
	r := Runner{GOOS: "darwin", Exec: fakeExec(fixture(t, "macos_v6.txt"), nil), Look: func(string) error { return nil }}
	tr, err := r.Trace(context.Background(), ip("2001:db8::ffff"))
	if err != nil || !tr.Reached || tr.Terminal != ip("2001:db8::9") {
		t.Fatalf("%+v %v", tr, err)
	}
}
