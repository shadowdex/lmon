// Package probe measures DNS resolution, geo location and connection timing
// for an LLM API endpoint.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sort"
	"time"

	"github.com/shadowdex/lmon/internal/geoip"
)

// DefaultResolvers are queried in addition to the system resolver.
var DefaultResolvers = map[string]string{
	"cloudflare": "1.1.1.1:53",
	"google":     "8.8.8.8:53",
	"quad9":      "9.9.9.9:53",
}

type Geo struct {
	ISO     string  `json:"iso,omitempty"`
	Country string  `json:"country,omitempty"`
	City    string  `json:"city,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
}

type DNSResult struct {
	Resolver string   `json:"resolver"`
	IPs      []string `json:"ips,omitempty"`
	Latency  Duration `json:"latency"`
	Error    string   `json:"error,omitempty"`
}

type IPInfo struct {
	IP  string `json:"ip"`
	Geo *Geo   `json:"geo,omitempty"`
}

type Timing struct {
	DNS     Duration `json:"dns"`
	Connect Duration `json:"tcp_connect"`
	TLS     Duration `json:"tls_handshake"`
	TTFB    Duration `json:"ttfb"`
	Total   Duration `json:"total"`
	Remote  string   `json:"remote_addr,omitempty"`
	Status  int      `json:"http_status,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type Result struct {
	Host   string      `json:"host"`
	DNS    []DNSResult `json:"dns"`
	IPs    []IPInfo    `json:"ips"`
	Timing Timing      `json:"timing"`
}

// Duration marshals to milliseconds for readable JSON.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.2f", float64(time.Duration(d))/float64(time.Millisecond))), nil
}

func (d Duration) String() string { return time.Duration(d).Round(10 * time.Microsecond).String() }

// Run probes host. geoDB may be empty to skip geolocation.
func Run(ctx context.Context, host, geoDB string, timeout time.Duration) Result {
	res := Result{Host: host}

	// DNS across resolvers (system first, then public ones in stable order).
	names := []string{"system"}
	var pub []string
	for n := range DefaultResolvers {
		pub = append(pub, n)
	}
	sort.Strings(pub)
	names = append(names, pub...)

	seen := map[string]bool{}
	var ips []string
	for _, n := range names {
		r := resolverFor(n)
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		addrs, err := r.LookupIPAddr(cctx, host)
		d := DNSResult{Resolver: n, Latency: Duration(time.Since(start))}
		if err != nil {
			d.Error = err.Error()
		}
		for _, a := range addrs {
			s := a.IP.String()
			d.IPs = append(d.IPs, s)
			if !seen[s] {
				seen[s] = true
				ips = append(ips, s)
			}
		}
		cancel()
		res.DNS = append(res.DNS, d)
	}

	var db *geoip.DB
	if geoDB != "" {
		if d, err := geoip.Open(geoDB); err == nil {
			db = d
			defer db.Close()
		}
	}
	for _, s := range ips {
		res.IPs = append(res.IPs, IPInfo{IP: s, Geo: lookupGeo(db, s)})
	}

	res.Timing = timeRequest(ctx, host, timeout)
	return res
}

func resolverFor(name string) *net.Resolver {
	addr, ok := DefaultResolvers[name]
	if !ok {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

func lookupGeo(db *geoip.DB, s string) *Geo {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	loc, ok := db.Lookup(ip)
	if !ok {
		return nil
	}
	return &Geo{ISO: loc.ISO, Country: loc.Country, City: loc.City, Lat: loc.Lat, Lon: loc.Lon}
}

// timeRequest does one HTTPS GET / and records per-phase timings.
func timeRequest(ctx context.Context, host string, timeout time.Duration) Timing {
	var t Timing
	var dnsStart, connStart, tlsStart, start time.Time
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { t.DNS = Duration(time.Since(dnsStart)) },
		ConnectStart:         func(_, _ string) { connStart = time.Now() },
		ConnectDone:          func(_, addr string, _ error) { t.Connect = Duration(time.Since(connStart)); t.Remote = addr },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.TLS = Duration(time.Since(tlsStart)) },
		GotFirstResponseByte: func() { t.TTFB = Duration(time.Since(start)) },
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(cctx, trace), http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		t.Error = err.Error()
		return t
	}
	// Fresh connection each run so DNS/TCP/TLS are actually measured.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start = time.Now()
	resp, err := client.Do(req)
	t.Total = Duration(time.Since(start))
	if err != nil {
		t.Error = err.Error()
		return t
	}
	resp.Body.Close()
	t.Status = resp.StatusCode
	return t
}
