package netpath

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// noConnect keeps tests off the network: no TCP handshake is measured.
var noConnect = func(context.Context, netip.Addr) float64 { return 0 }

func fakeRunner(t *testing.T, fixtureName string) Runner {
	t.Helper()
	b, err := os.ReadFile("testdata/" + fixtureName)
	if err != nil {
		t.Fatal(err)
	}
	return Runner{GOOS: "darwin", Look: func(string) error { return nil },
		Exec: func(context.Context, string, ...string) ([]byte, error) { return b, nil }}
}

func TestRunEndToEndWithFixtureAndFakes(t *testing.T) {
	var asked []netip.Addr
	o := Options{
		Connect: noConnect,
		Runner:  fakeRunner(t, "macos_v4.txt"),
		Dial: func(context.Context, string, int) (netip.Addr, error) {
			return netip.MustParseAddr("203.0.113.11"), nil
		},
		Geo: fakeGeo{netip.MustParseAddr("203.0.113.1"): toulouse, netip.MustParseAddr("203.0.113.3"): paris, netip.MustParseAddr("203.0.113.11"): sanFran},
		Names: func(_ context.Context, ips []netip.Addr, _ time.Duration) map[netip.Addr]string {
			asked = ips
			return map[netip.Addr]string{netip.MustParseAddr("203.0.113.7"): "lag-1.ear1.paris1.example.net"}
		},
	}
	p, err := Run(context.Background(), "api.example.test", o)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dest != netip.MustParseAddr("203.0.113.11") || !p.Reached || len(p.Hops) != 10 {
		t.Fatalf("%+v", p)
	}
	// only public addresses are sent to reverse DNS: never the user's own gateway
	for _, ip := range asked {
		if isLocalNet(ip) {
			t.Errorf("a private address was looked up: %v", ip)
		}
	}
	if len(asked) == 0 {
		t.Error("public hops should have been looked up")
	}
	// hop 8 is 203.0.113.7, whose hostname says paris1; GeoIP knows nothing about it
	if h := p.Hops[7]; h.IP != netip.MustParseAddr("203.0.113.7") || h.City != "Paris" || h.Source != "rdns" {
		t.Fatalf("the hostname hint should have been applied: %+v", h)
	}
	if p.Edge.Kind != "nearby" {
		t.Fatalf("edge: %+v", p.Edge)
	}
}

func TestRunNoRDNSSkipsLookups(t *testing.T) {
	called := false
	o := Options{
		Connect: noConnect,
		Runner:  fakeRunner(t, "macos_v4.txt"), NoRDNS: true,
		Dial: func(context.Context, string, int) (netip.Addr, error) {
			return netip.MustParseAddr("203.0.113.11"), nil
		},
		Names: func(context.Context, []netip.Addr, time.Duration) map[netip.Addr]string { called = true; return nil },
	}
	if _, err := Run(context.Background(), "x", o); err != nil || called {
		t.Fatalf("err=%v lookups=%v", err, called)
	}
}

func TestRunErrorsNameTheEndpoint(t *testing.T) {
	o := Options{
		Connect: noConnect, Dial: func(context.Context, string, int) (netip.Addr, error) {
			return netip.Addr{}, errors.New("no such host")
		}}
	if _, err := Run(context.Background(), "nope.example.test", o); err == nil || !strings.Contains(err.Error(), "nope.example.test") {
		t.Fatalf("dial error: %v", err)
	}
	o = Options{
		Connect: noConnect,
		Dial:    func(context.Context, string, int) (netip.Addr, error) { return netip.MustParseAddr("203.0.113.9"), nil },
		Runner:  Runner{GOOS: "linux", Look: func(string) error { return errors.New("x") }},
	}
	_, err := Run(context.Background(), "api.example.test", o)
	var nt ErrNoTraceroute
	if !errors.As(err, &nt) || !strings.Contains(err.Error(), "api.example.test") {
		t.Fatalf("traceroute error should wrap ErrNoTraceroute and name the host: %v", err)
	}
}

func TestRunPassesTheHandshakeTimeToTheAssessment(t *testing.T) {
	var gotDest netip.Addr
	o := Options{
		Runner: fakeRunner(t, "macos_v4.txt"), NoRDNS: true,
		Dial: func(context.Context, string, int) (netip.Addr, error) {
			return netip.MustParseAddr("203.0.113.11"), nil
		},
		Connect: func(_ context.Context, d netip.Addr) float64 { gotDest = d; return 6.5 },
	}
	p, err := Run(context.Background(), "x", o)
	if err != nil {
		t.Fatal(err)
	}
	last := p.Hops[len(p.Hops)-1]
	if gotDest != netip.MustParseAddr("203.0.113.11") || !last.Dest || last.RTT != 6.5 {
		t.Fatalf("dest=%v last=%+v", gotDest, last)
	}
}

func TestMeasureConnectBestOfSeveralAndZeroOnFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen: ", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ap := netip.MustParseAddrPort(ln.Addr().String())
	if ms := measureConnect(context.Background(), ap, 3); ms <= 0 || ms > 1000 {
		t.Fatalf("a local handshake should be fast and positive, got %v", ms)
	}
	ln.Close()
	if ms := measureConnect(context.Background(), ap, 2); ms != 0 {
		t.Fatalf("a refused connection gives 0, got %v", ms)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if ms := measureConnect(cancelled, ap, 2); ms != 0 {
		t.Fatalf("a cancelled context gives 0, got %v", ms)
	}
}

// Found on Windows CI: a fast successful connect measured as 0 ms and was thrown away.
func TestConnectMsIsNeverZeroForASuccessfulConnect(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Millisecond, 100 * time.Nanosecond} {
		if got := connectMs(d); got <= 0 {
			t.Errorf("connectMs(%v) = %v, must stay positive", d, got)
		}
	}
	if got := connectMs(5 * time.Millisecond); got != 5 {
		t.Errorf("real timings pass through unchanged, got %v", got)
	}
	if got := connectMs(1500 * time.Microsecond); got != 1.5 {
		t.Errorf("got %v", got)
	}
}
