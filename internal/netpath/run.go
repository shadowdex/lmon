package netpath

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Options configures Run. The zero value is usable.
type Options struct {
	Family      int // 0 = whatever a client would connect to, 4 or 6 to force
	Runner      Runner
	Geo         Locator
	NoRDNS      bool
	Vantage     *Point
	RDNSTimeout time.Duration // default 1.5s per address

	// Test seams.
	Connect func(ctx context.Context, dest netip.Addr) float64
	Dial    func(ctx context.Context, host string, family int) (netip.Addr, error)
	Names   func(ctx context.Context, ips []netip.Addr, timeout time.Duration) map[netip.Addr]string
}

// ChooseDest picks the address to trace. By default that is the address a
// client would actually connect to (a real TCP connection, so the system's own
// IPv4/IPv6 preference and fallback apply), which matters because the two
// families can take very different routes.
func ChooseDest(ctx context.Context, host string, family int) (netip.Addr, error) {
	lookup := func(network string) (netip.Addr, error) {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
		if err != nil {
			return netip.Addr{}, err
		}
		if len(addrs) == 0 {
			return netip.Addr{}, fmt.Errorf("%s: no addresses", host)
		}
		return addrs[0].Unmap(), nil
	}
	switch family {
	case 4:
		return lookup("ip4")
	case 6:
		return lookup("ip6")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	if conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443")); err == nil {
		defer conn.Close()
		if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			return ta.AddrPort().Addr().Unmap(), nil
		}
	}
	return lookup("ip") // the connection failed (firewall?); trace the first address anyway
}

// MeasureConnect returns the best of a few TCP handshake times to addr:443 in
// milliseconds, or 0 if every attempt failed. A handshake is answered by the
// server's network stack, not a router's control plane, so unlike a traceroute
// reply it is a clean measure of the round trip.
func MeasureConnect(ctx context.Context, dest netip.Addr) float64 {
	return measureConnect(ctx, netip.AddrPortFrom(dest, 443), 3)
}

func measureConnect(ctx context.Context, ap netip.AddrPort, tries int) float64 {
	best := 0.0
	for i := 0; i < tries; i++ {
		start := time.Now()
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", ap.String())
		if err != nil {
			continue
		}
		ms := float64(time.Since(start)) / float64(time.Millisecond)
		conn.Close()
		if best == 0 || ms < best {
			best = ms
		}
	}
	return best
}

// Run traces one endpoint and assesses the path.
func Run(ctx context.Context, host string, o Options) (Path, error) {
	dial := o.Dial
	if dial == nil {
		dial = ChooseDest
	}
	dest, err := dial(ctx, host, o.Family)
	if err != nil {
		return Path{}, fmt.Errorf("%s: %w", host, err)
	}
	connect := o.Connect
	if connect == nil {
		connect = MeasureConnect
	}
	connectMs := connect(ctx, dest)
	tr, err := o.Runner.Trace(ctx, dest)
	if err != nil {
		return Path{}, fmt.Errorf("%s (%s): %w", host, dest, err)
	}
	tr.ConnectMs = connectMs

	var names map[netip.Addr]string
	if !o.NoRDNS {
		resolve := o.Names
		if resolve == nil {
			resolve = ResolveNames
		}
		timeout := o.RDNSTimeout
		if timeout == 0 {
			timeout = 1500 * time.Millisecond
		}
		var ips []netip.Addr
		seen := map[netip.Addr]bool{}
		for _, h := range tr.Hops {
			for _, r := range h.Responders {
				if !isLocalNet(r.IP) && !seen[r.IP] {
					seen[r.IP] = true
					ips = append(ips, r.IP)
				}
			}
		}
		names = resolve(ctx, ips, timeout)
	}
	return Assess(host, tr, o.Geo, names, o.Vantage), nil
}
