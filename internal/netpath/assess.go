package netpath

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/shadowdex/lmon/internal/geoip"
)

// Locator finds where an address is registered. *geoip.DB implements it.
type Locator interface {
	Lookup(netip.Addr) (geoip.Location, bool)
}

// Point is a place on Earth, used as the viewpoint the path starts from.
type Point struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Label  string  `json:"label,omitempty"`
	Source string  `json:"source"` // "given" or "first public hop"
}

// Located is a hop with our best judgement of where it is.
type Located struct {
	TTL        int          `json:"ttl"`
	Class      string       `json:"class"` // "private", "public" or "silent"
	IP         netip.Addr   `json:"ip,omitempty"`
	Also       []netip.Addr `json:"also,omitempty"` // other routers that answered at this hop (load balancing)
	RTT        float64      `json:"rtt_ms,omitempty"`
	Host       string       `json:"host,omitempty"`
	ISO        string       `json:"iso,omitempty"`
	City       string       `json:"city,omitempty"`
	Lat        float64      `json:"lat,omitempty"`
	Lon        float64      `json:"lon,omitempty"`
	Source     string       `json:"source,omitempty"`     // "geoip", "rdns" or "rdns+geoip"
	GeoISO     string       `json:"geoip_iso,omitempty"`  // what GeoIP said when we did not use it
	Confidence string       `json:"confidence,omitempty"` // "high", "medium" or "low"
	Notes      []string     `json:"notes,omitempty"`
	Dest       bool         `json:"destination,omitempty"`
}

// HasCoords reports whether the hop can be drawn on a map.
func (l Located) HasCoords() bool { return l.Lat != 0 || l.Lon != 0 }

// Edge describes where traffic enters the provider, which is as far as a
// traceroute can see. What happens behind it is not observable.
type Edge struct {
	// Kind is "nearby" (latency rules out the registered location, so this is an
	// anycast or edge server close to you), "consistent" (latency fits the
	// registered location; consistent with, not proof of) or "unknown".
	Kind       string         `json:"kind"`
	MaxKm      float64        `json:"max_km,omitempty"` // the round trip allows the destination no farther than this
	RTT        float64        `json:"rtt_ms,omitempty"`
	ISO        string         `json:"iso,omitempty"`
	City       string         `json:"city,omitempty"`
	Lat        float64        `json:"lat,omitempty"`
	Lon        float64        `json:"lon,omitempty"`
	Registered geoip.Location `json:"registered"`
	Note       string         `json:"note,omitempty"`
}

// Path is the assessed route to one endpoint.
type Path struct {
	Host      string     `json:"host"`
	Dest      netip.Addr `json:"dest"`
	Reached   bool       `json:"reached"`
	Partial   bool       `json:"partial,omitempty"`
	Vantage   *Point     `json:"vantage,omitempty"`
	Hops      []Located  `json:"hops"`
	Countries []string   `json:"countries"` // ISO codes in path order, from hops we trust at least somewhat
	Edge      Edge       `json:"edge"`
	Warnings  []string   `json:"warnings,omitempty"`
}

const (
	// Light in optical fibre covers about 200 km per millisecond (2/3 of c).
	fibreKmPerMs = 200.0
	// Slack added to the distance the round trip allows, for GeoIP being
	// city-level at best and for the viewpoint being approximate.
	slackKm = 150.0
	// Cables are longer than the straight line between two cities: about 1.5x
	// on typical internet routes, but only 1.1-1.3x along dense corridors such
	// as Toulouse-Paris. 1.25 rejects a hop that would need an almost perfectly
	// straight cable (900 km in 8 ms) without discarding real ones. A heuristic.
	routeFactor = 1.25
)

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// maxDistanceKm is the farthest away a host can be given its round trip time.
// Real paths are longer than the straight line, so the true distance is
// usually well below this; it is a bound, not an estimate.
func maxDistanceKm(rttMs float64) float64 { return rttMs / 2 * fibreKmPerMs }

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isLocalNet is true for addresses that belong to no country: your own
// network, carrier-grade NAT, loopback and link-local.
func isLocalNet(ip netip.Addr) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// ResolveNames looks up the reverse-DNS name of each address, in parallel,
// with a short timeout per address. Missing names are simply absent.
func ResolveNames(ctx context.Context, ips []netip.Addr, timeout time.Duration) map[netip.Addr]string {
	out := make(map[netip.Addr]string, len(ips))
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip netip.Addr) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			names, err := net.DefaultResolver.LookupAddr(cctx, ip.String())
			if err != nil || len(names) == 0 {
				return
			}
			mu.Lock()
			out[ip] = names[0]
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	return out
}

// Assess turns a raw trace into a located path. geo may be nil, names may be
// nil, and vantage may be nil (then the first public hop stands in for it).
func Assess(host string, tr Trace, geo Locator, names map[netip.Addr]string, vantage *Point) Path {
	p := Path{Host: host, Dest: tr.Dest, Reached: tr.Reached, Partial: tr.Partial, Vantage: vantage}

	for i, h := range tr.Hops {
		l := Located{TTL: h.TTL}
		prim, ok := h.Primary()
		if !ok {
			l.Class = "silent"
			p.Hops = append(p.Hops, l)
			continue
		}
		for _, r := range h.Responders { // when the destination itself answered, it is the one that counts
			if r.IP == tr.Dest {
				prim = r
			}
		}
		l.IP, l.RTT = prim.IP, prim.MinRTT
		for _, r := range h.Responders {
			if r.IP != prim.IP {
				l.Also = append(l.Also, r.IP)
			}
		}
		l.Dest = prim.IP == tr.Dest
		if tr.Terminal.IsValid() && i == len(tr.Hops)-1 && prim.IP == tr.Terminal {
			l.Dest = true
			l.Notes = append(l.Notes, "answered from a different address than the one probed, which is typical of anycast and load-balanced endpoints")
		}
		// TTL 1 is by definition your own gateway, even when an IPv6 router has a global address.
		if isLocalNet(prim.IP) || h.TTL == 1 {
			l.Class = "private"
			p.Hops = append(p.Hops, l)
			continue
		}
		l.Class = "public"
		l.Host = names[prim.IP]
		locate(&l, geo)
		p.Hops = append(p.Hops, l)
	}

	if tr.ConnectMs > 0 {
		for i := range p.Hops {
			h := &p.Hops[i]
			if h.Dest && (h.RTT == 0 || tr.ConnectMs < h.RTT) {
				h.RTT = tr.ConnectMs
				h.Notes = append(h.Notes, fmt.Sprintf("round trip taken from a TCP handshake (%.1f ms), which is more reliable than a router reply", tr.ConnectMs))
			}
		}
	}
	// A packet to the destination passes through every earlier hop, so no hop can
	// be farther away than the fastest answer from it or any later hop allows.
	// Router replies are slow and noisy (they come from the control plane), which
	// is why a single hop's own round trip is too loose a bound.
	eff := effectiveRTTs(p.Hops)
	if p.Vantage == nil {
		p.Vantage = bestVantage(p.Hops, eff)
	}
	if p.Vantage != nil {
		for i := range p.Hops {
			checkPlausible(&p.Hops[i], *p.Vantage, eff[i])
		}
		p.Warnings = viewpointWarnings(p.Hops, p.Vantage)
	}
	p.Countries = countries(p.Hops, p.Dest)
	p.Edge = assessEdge(p, geo)
	return p
}

// locate decides where a public hop is, preferring the router's own hostname
// over GeoIP: databases place routers at their owner's headquarters, while
// operators put the city code in the name.
func locate(l *Located, geo Locator) {
	hint, hasHint := HintFromHostname(l.Host)
	var g geoip.Location
	var hasGeo bool
	if geo != nil {
		g, hasGeo = geo.Lookup(l.IP)
	}
	switch {
	case hasHint && hint.Kind == "city":
		l.ISO, l.City, l.Lat, l.Lon = hint.ISO, hint.City, hint.Lat, hint.Lon
		if hasGeo && g.ISO == hint.ISO {
			l.Source, l.Confidence = "rdns+geoip", "high"
		} else {
			l.Source, l.Confidence = "rdns", "medium"
			if hasGeo {
				l.GeoISO = g.ISO
				l.Notes = append(l.Notes, fmt.Sprintf("GeoIP says %s, the hostname says %s; using the hostname", place(g), hint.City))
			}
		}
	case hasHint: // a bare country label
		if hasGeo && g.ISO == hint.ISO {
			l.ISO, l.City, l.Lat, l.Lon = g.ISO, g.City, g.Lat, g.Lon
			l.Source, l.Confidence = "rdns+geoip", "high"
		} else {
			l.ISO, l.Source, l.Confidence = hint.ISO, "rdns", "medium"
			if hasGeo {
				l.GeoISO = g.ISO
				l.Notes = append(l.Notes, fmt.Sprintf("GeoIP says %s, the hostname says country %s; using the hostname", place(g), hint.ISO))
			}
		}
	case hasGeo:
		l.ISO, l.City, l.Lat, l.Lon = g.ISO, g.City, g.Lat, g.Lon
		l.Source, l.Confidence = "geoip", "medium"
	default:
		l.Confidence = "low"
		l.Notes = append(l.Notes, "no location data for this address")
	}
}

func place(g geoip.Location) string {
	if g.City != "" {
		return g.City + ", " + g.ISO
	}
	return g.ISO
}

func effectiveRTTs(hops []Located) []float64 {
	eff := make([]float64, len(hops))
	best := math.Inf(1)
	for i := len(hops) - 1; i >= 0; i-- {
		if r := hops[i].RTT; r > 0 && r < best {
			best = r
		}
		eff[i] = best
	}
	return eff
}

// conflicts reports how a hop's claimed location sits against a viewpoint:
// "impossible" if the round trip rules it out, "stretched" if it would need an
// almost perfectly straight cable, "" if it fits.
func conflict(l Located, v Point, rtt float64) (kind string, dist, bound float64) {
	if l.Class != "public" || !l.HasCoords() || l.RTT <= 0 || math.IsInf(rtt, 1) {
		return "", 0, 0
	}
	dist = haversineKm(v.Lat, v.Lon, l.Lat, l.Lon)
	bound = maxDistanceKm(rtt)
	switch {
	case dist > bound+slackKm:
		return "impossible", dist, bound
	case dist > bound/routeFactor+slackKm:
		return "stretched", dist, bound
	}
	return "", dist, bound
}

// bestVantage stands in for "where you are". Candidates are the places of the
// routers we located, and the winner is the one that contradicts the fewest
// hops whose location the hostname and GeoIP agree on, because those are the
// most trustworthy claims we have. Ties go to the earliest hop, which is the
// closest to you.
func bestVantage(hops []Located, eff []float64) *Point {
	var order []int // hostname-confirmed hops first, then the rest, each in path order
	for _, want := range []string{"high", ""} {
		for i, h := range hops {
			if h.Class == "public" && h.HasCoords() && ((want == "high") == (h.Confidence == "high")) {
				order = append(order, i)
			}
		}
	}
	best, bestScore := -1, 1<<30
	for _, i := range order {
		v := Point{Lat: hops[i].Lat, Lon: hops[i].Lon}
		score := 0
		for j, h := range hops {
			if j != i && h.Confidence == "high" {
				if k, _, _ := conflict(h, v, eff[j]); k != "" {
					score++
				}
			}
		}
		if score < bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return nil
	}
	h := hops[best]
	return &Point{Lat: h.Lat, Lon: h.Lon, Label: pointLabel(h), Source: "first public hop"}
}

// viewpointWarnings tells the user when the viewpoint contradicts a router whose
// place the hostname and GeoIP agree on: then the viewpoint is the likelier error.
func viewpointWarnings(hops []Located, v *Point) []string {
	var out []string
	for _, h := range hops {
		if h.Confidence != "low" || h.Source != "rdns+geoip" {
			continue
		}
		out = append(out, fmt.Sprintf("hop %d (%s) is placed in %s by both its hostname and GeoIP, yet it conflicts with your viewpoint %s: %s",
			h.TTL, h.IP, placeLabel(h.ISO, h.City), v.Label, strings.Join(h.Notes, "; ")))
	}
	if len(out) == 0 {
		return nil
	}
	return append(out[:min(len(out), 2)], "the viewpoint may be wrong, not the routers: set it with --from LAT,LON (or $LMON_FROM)")
}

func pointLabel(l Located) string {
	if l.City != "" {
		return l.City + ", " + l.ISO
	}
	return l.ISO
}

// checkPlausible downgrades a hop whose round trip is too short for the place
// we put it: the signal would have had to travel faster than light in fibre.
// rtt is the effective round trip, the smallest seen at this hop or any later one.
func checkPlausible(l *Located, v Point, rtt float64) {
	kind, dist, bound := conflict(*l, v, rtt)
	if kind == "" {
		return
	}
	via := fmt.Sprintf("a %.0f ms round trip", rtt)
	if rtt < l.RTT {
		via = fmt.Sprintf("a later hop on the same path answered in %.0f ms, so a %.0f ms round trip", rtt, rtt)
	}
	l.Confidence = "low"
	if kind == "impossible" {
		l.Notes = append(l.Notes, fmt.Sprintf("%s rules out a location %.0f km away (limit about %.0f km)", via, dist, bound))
		return
	}
	l.Notes = append(l.Notes, fmt.Sprintf("%s would need an almost perfectly straight cable to a location %.0f km away; real routes are longer, so not trusted", via, dist))
}

func countries(hops []Located, dest netip.Addr) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range hops {
		// The probed address's own location is where it is registered, which is not
		// an observation; the endpoint line reports it. A different address that
		// answered for it (h.Dest with another IP) is a real router and does count.
		if h.Class != "public" || h.ISO == "" || h.Confidence == "low" || seen[h.ISO] || (h.Dest && h.IP == dest) {
			continue
		}
		seen[h.ISO] = true
		out = append(out, h.ISO)
	}
	return out
}

func assessEdge(p Path, geo Locator) Edge {
	var e Edge
	if geo != nil {
		e.Registered, _ = geo.Lookup(p.Dest)
	}
	var dest *Located
	for i := range p.Hops {
		if p.Hops[i].Dest {
			dest = &p.Hops[i]
		}
	}
	if !p.Reached || dest == nil {
		e.Kind = "unknown"
		e.Note = "the destination never answered the probes, so the path ends before it; firewalls often block them"
		return e
	}
	e.RTT = dest.RTT
	// Anycast and load-balanced endpoints often answer from their own unicast
	// address. Where that router is located says more than where the probed
	// address is registered.
	if dest.IP != p.Dest && dest.Confidence != "low" && dest.ISO != "" {
		e.MaxKm = maxDistanceKm(dest.RTT)
		e.ISO, e.City, e.Lat, e.Lon = dest.ISO, dest.City, dest.Lat, dest.Lon
		if e.Registered.ISO == "" || e.Registered.ISO == dest.ISO {
			e.Kind = "consistent"
			e.Note = fmt.Sprintf("answered from %s (%.0f ms round trip), in the same country as its registration, %s", placeLabel(dest.ISO, dest.City), dest.RTT, place(e.Registered))
		} else {
			e.Kind = "nearby"
			e.Note = fmt.Sprintf("registered in %s, but answered from %s (%.0f ms round trip): an anycast or edge server close to you, not a machine in %s",
				place(e.Registered), placeLabel(dest.ISO, dest.City), dest.RTT, e.Registered.ISO)
		}
		return e
	}
	if p.Vantage == nil || !e.Registered.HasCoords() || dest.RTT <= 0 {
		e.Kind = "unknown"
		e.Note = "not enough location data to compare the round trip with the registered location"
		return e
	}
	dist := haversineKm(p.Vantage.Lat, p.Vantage.Lon, e.Registered.Lat, e.Registered.Lon)
	e.MaxKm = maxDistanceKm(dest.RTT)
	if dist > e.MaxKm+slackKm {
		e.Kind = "nearby"
		// Where traffic enters: the last trustworthy router before the destination.
		for i := len(p.Hops) - 1; i >= 0; i-- {
			h := p.Hops[i]
			if h.Dest || h.Class != "public" || h.Confidence == "low" || h.ISO == "" {
				continue
			}
			e.ISO, e.City, e.Lat, e.Lon = h.ISO, h.City, h.Lat, h.Lon
			break
		}
		e.Note = fmt.Sprintf("registered in %s, but it answers within about %.0f km of you (%.0f ms round trip): an anycast or edge server close to you, not a machine in %s",
			place(e.Registered), e.MaxKm, dest.RTT, e.Registered.ISO)
		return e
	}
	e.Kind = "consistent"
	e.ISO, e.City, e.Lat, e.Lon = e.Registered.ISO, e.Registered.City, e.Registered.Lat, e.Registered.Lon
	e.Note = fmt.Sprintf("registered in %s; the %.0f ms round trip is consistent with that (it would also fit somewhere nearer)", place(e.Registered), dest.RTT)
	return e
}
