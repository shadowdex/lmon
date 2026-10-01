package netpath

import (
	"math"
	"net/netip"
	"strings"
	"testing"

	"github.com/shadowdex/lmon/internal/geoip"
)

type fakeGeo map[netip.Addr]geoip.Location

func (f fakeGeo) Lookup(ip netip.Addr) (geoip.Location, bool) { l, ok := f[ip]; return l, ok }

var (
	toulouse = geoip.Location{ISO: "FR", Country: "France", City: "Toulouse", Lat: 43.6047, Lon: 1.4444}
	paris    = geoip.Location{ISO: "FR", Country: "France", City: "Paris", Lat: 48.8566, Lon: 2.3522}
	london   = geoip.Location{ISO: "GB", Country: "United Kingdom", City: "London", Lat: 51.5074, Lon: -0.1278}
	sanFran  = geoip.Location{ISO: "US", Country: "United States", City: "San Francisco", Lat: 37.7901, Lon: -122.401}
	toronto  = geoip.Location{ISO: "CA", Country: "Canada", City: "Toronto", Lat: 43.6532, Lon: -79.3832}
	tokyo    = geoip.Location{ISO: "JP", Country: "Japan", City: "Tokyo", Lat: 35.6762, Lon: 139.6503}
)

// hop builds a hop answered by one router.
func hop(ttl int, addr string, rtt float64) Hop {
	return Hop{TTL: ttl, Responders: []Responder{{IP: netip.MustParseAddr(addr), MinRTT: rtt, Replies: 3}}}
}

func tr4(s string) netip.Addr { return netip.MustParseAddr(s) }

func silent(ttl int) Hop { return Hop{TTL: ttl, Timeouts: 3} }

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.1f want %.1f (±%.1f)", name, got, want, tol)
	}
}

func TestHaversineAndBound(t *testing.T) {
	near(t, "Toulouse-Paris km", haversineKm(toulouse.Lat, toulouse.Lon, paris.Lat, paris.Lon), 589, 10)
	near(t, "Paris-San Francisco km", haversineKm(paris.Lat, paris.Lon, sanFran.Lat, sanFran.Lon), 8960, 60)
	near(t, "bound for 8.8 ms", maxDistanceKm(8.8), 880, 0.1)
	if haversineKm(1, 2, 1, 2) != 0 {
		t.Error("same point is 0 km")
	}
}

func TestIsLocalNet(t *testing.T) {
	for _, s := range []string{"192.168.1.1", "10.0.0.1", "172.16.0.1", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.1.1", "fe80::1", "fd00::1", "::1"} {
		if !isLocalNet(netip.MustParseAddr(s)) {
			t.Errorf("%s should be local", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "100.63.255.255", "100.128.0.1", "2001:db8::1", "203.0.113.1"} {
		if isLocalNet(netip.MustParseAddr(s)) {
			t.Errorf("%s is not local", s)
		}
	}
}

// Modelled on the real trace from France to an Anthropic-style endpoint:
// the destination is registered in San Francisco but answers in under 9 ms.
func anycastScenario() (Trace, fakeGeo, map[netip.Addr]string) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "192.168.0.1", 4),
		hop(2, "203.0.113.2", 6.5), // ISP, Toulouse
		hop(3, "203.0.113.3", 8.6), // Paris
		silent(4),
		hop(5, "203.0.113.5", 10.2), // Colt: GeoIP says London, the hostname says par1
		hop(6, "203.0.113.6", 11.6), // Cloudflare-ish, Paris
		hop(7, "203.0.113.250", 8.8),
	}}
	geo := fakeGeo{
		netip.MustParseAddr("203.0.113.2"):   toulouse,
		netip.MustParseAddr("203.0.113.3"):   paris,
		netip.MustParseAddr("203.0.113.5"):   london,
		netip.MustParseAddr("203.0.113.6"):   paris,
		netip.MustParseAddr("203.0.113.250"): sanFran,
	}
	names := map[netip.Addr]string{
		netip.MustParseAddr("203.0.113.5"): "ae2.3211.edge7.par1.neo.colt.net",
	}
	return tr, geo, names
}

func TestAnycastEndpointIsRecognisedAsNearby(t *testing.T) {
	tr, geo, names := anycastScenario()
	p := Assess("api.example.test", tr, geo, names, nil)

	if p.Vantage == nil || p.Vantage.Source != "first public hop" || p.Vantage.Label != "Toulouse, FR" {
		t.Fatalf("vantage: %+v", p.Vantage)
	}
	if p.Hops[0].Class != "private" || p.Hops[3].Class != "silent" || p.Hops[1].Class != "public" {
		t.Fatalf("classes: %v", p.Hops)
	}
	// GeoIP put the Colt router in London; its hostname says Paris, and the hostname wins
	colt := p.Hops[4]
	if colt.ISO != "FR" || colt.City != "Paris" || colt.Source != "rdns" || colt.GeoISO != "GB" || colt.Confidence != "medium" {
		t.Fatalf("colt hop: %+v", colt)
	}
	if !strings.Contains(strings.Join(colt.Notes, " "), "hostname says Paris") {
		t.Fatalf("the disagreement must be explained: %v", colt.Notes)
	}
	// the ISP hops agree with GeoIP only (no hostname): medium
	if p.Hops[1].Source != "geoip" || p.Hops[1].Confidence != "medium" {
		t.Fatalf("isp hop: %+v", p.Hops[1])
	}
	// the destination: registered in San Francisco, impossible at 8.8 ms from Toulouse
	d := p.Hops[6]
	if !d.Dest || d.Confidence != "low" || !strings.Contains(strings.Join(d.Notes, " "), "rules out") {
		t.Fatalf("dest hop: %+v", d)
	}
	if p.Edge.Kind != "nearby" || p.Edge.Registered.ISO != "US" {
		t.Fatalf("edge: %+v", p.Edge)
	}
	near(t, "edge bound", p.Edge.MaxKm, 880, 1)
	if p.Edge.ISO != "FR" || p.Edge.City != "Paris" { // the last trustworthy hop before the destination
		t.Fatalf("edge location: %+v", p.Edge)
	}
	if strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("countries: %v (the impossible US and the wrong GB must not appear)", p.Countries)
	}
	if !strings.Contains(p.Edge.Note, "anycast") {
		t.Fatalf("note: %s", p.Edge.Note)
	}
}

func TestFarUnicastDestinationIsConsistentNotProven(t *testing.T) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "192.168.0.1", 2), hop(2, "203.0.113.2", 5), hop(3, "203.0.113.3", 9), hop(4, "203.0.113.250", 142),
	}}
	geo := fakeGeo{
		netip.MustParseAddr("203.0.113.2"): toulouse, netip.MustParseAddr("203.0.113.3"): paris,
		netip.MustParseAddr("203.0.113.250"): sanFran,
	}
	p := Assess("far.example.test", tr, geo, nil, nil)
	if p.Edge.Kind != "consistent" || p.Edge.ISO != "US" || p.Edge.City != "San Francisco" {
		t.Fatalf("edge: %+v", p.Edge)
	}
	if !strings.Contains(p.Edge.Note, "consistent") {
		t.Fatalf("the note must say consistent, not claim proof: %s", p.Edge.Note)
	}
	// US is only where the destination is registered: reported by the edge, not counted as observed
	if strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("countries: %v", p.Countries)
	}
}

func TestUnreachedDestinationIsUnknown(t *testing.T) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Reached: false, Hops: []Hop{
		hop(1, "192.168.0.1", 2), hop(2, "203.0.113.2", 5), silent(3), silent(4),
	}}
	p := Assess("blocked.example.test", tr, fakeGeo{netip.MustParseAddr("203.0.113.2"): toulouse}, nil, nil)
	if p.Edge.Kind != "unknown" || !strings.Contains(p.Edge.Note, "never answered") {
		t.Fatalf("edge: %+v", p.Edge)
	}
	if strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("what was observed still counts: %v", p.Countries)
	}
}

func TestImplausibleMiddleHopIsExcluded(t *testing.T) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "192.168.0.1", 1), hop(2, "203.0.113.2", 2), hop(3, "203.0.113.3", 8), hop(4, "203.0.113.250", 9),
	}}
	geo := fakeGeo{
		netip.MustParseAddr("203.0.113.2"):   paris,
		netip.MustParseAddr("203.0.113.3"):   tokyo, // GeoIP claims Tokyo at 8 ms from Paris
		netip.MustParseAddr("203.0.113.250"): paris,
	}
	p := Assess("x.example.test", tr, geo, nil, nil)
	if p.Hops[2].Confidence != "low" || strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("hop 3: %+v countries: %v", p.Hops[2], p.Countries)
	}
}

func TestGivenVantageIsUsedAndNotOverridden(t *testing.T) {
	tr, geo, names := anycastScenario()
	// the user really is in London: the same 8.8 ms now rules out San Francisco from there too,
	// but Toulouse (~900 km away) is no longer a plausible place for the ISP hops at 6.5 ms
	v := &Point{Lat: london.Lat, Lon: london.Lon, Label: "London, GB", Source: "given"}
	p := Assess("api.example.test", tr, geo, names, v)
	if p.Vantage.Source != "given" || p.Vantage.Label != "London, GB" {
		t.Fatalf("vantage replaced: %+v", p.Vantage)
	}
	if p.Hops[1].Confidence != "low" {
		t.Fatalf("a 6.5 ms hop cannot be in Toulouse when you are in London: %+v", p.Hops[1])
	}
}

func TestNoLocationDataMeansNoClaims(t *testing.T) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "192.168.0.1", 1), silent(2), hop(3, "203.0.113.250", 20),
	}}
	p := Assess("x.example.test", tr, nil, nil, nil)
	if p.Vantage != nil || len(p.Countries) != 0 {
		t.Fatalf("nothing to go on: vantage=%+v countries=%v", p.Vantage, p.Countries)
	}
	if p.Edge.Kind != "unknown" {
		t.Fatalf("edge: %+v", p.Edge)
	}
	if h := p.Hops[2]; h.Confidence != "low" || !h.Dest {
		t.Fatalf("hop 3: %+v", h)
	}
}

func TestLoadBalancedHopKeepsOtherRoutersAndPrefersTheDestination(t *testing.T) {
	dest := netip.MustParseAddr("203.0.113.250")
	h := Hop{TTL: 5, Responders: []Responder{
		{IP: netip.MustParseAddr("203.0.113.8"), MinRTT: 5, Replies: 2},
		{IP: dest, MinRTT: 6, Replies: 1},
	}}
	p := Assess("x.example.test", Trace{Dest: dest, Reached: true, Hops: []Hop{h}}, nil, nil, nil)
	got := p.Hops[0]
	if got.IP != dest || !got.Dest || len(got.Also) != 1 || got.Also[0] != netip.MustParseAddr("203.0.113.8") {
		t.Fatalf("%+v", got)
	}
}

func TestCountryLabelHint(t *testing.T) {
	tr := Trace{Dest: netip.MustParseAddr("203.0.113.250"), Hops: []Hop{hop(1, "192.168.0.1", 1), hop(2, "203.0.113.9", 10)}}
	names := map[netip.Addr]string{netip.MustParseAddr("203.0.113.9"): "core1.edge.de.example.net"}
	// GeoIP agrees on the country -> high, and its coordinates are used
	geo := fakeGeo{netip.MustParseAddr("203.0.113.9"): {ISO: "DE", City: "Berlin", Lat: 52.52, Lon: 13.4}}
	h := Assess("x", tr, geo, names, nil).Hops[1]
	if h.Source != "rdns+geoip" || h.Confidence != "high" || h.City != "Berlin" {
		t.Fatalf("agree: %+v", h)
	}
	// GeoIP says elsewhere -> the hostname's country wins, without coordinates
	geo = fakeGeo{netip.MustParseAddr("203.0.113.9"): {ISO: "US", City: "Ashburn", Lat: 39, Lon: -77}}
	h = Assess("x", tr, geo, names, nil).Hops[1]
	if h.ISO != "DE" || h.Source != "rdns" || h.GeoISO != "US" {
		t.Fatalf("disagree: %+v", h)
	}
	if h.HasCoords() {
		t.Fatalf("a country-only hint has no coordinates to draw: %+v", h)
	}
}

// The endpoint answered from its own address: that last router IS the end of the path.
func TestTerminalResponderIsTheDestinationHop(t *testing.T) {
	dest := tr4("203.0.113.250")
	tr := Trace{Dest: dest, Reached: true, Terminal: tr4("203.0.113.99"), Hops: []Hop{
		hop(1, "203.0.113.2", 5), hop(2, "203.0.113.3", 7), hop(3, "203.0.113.99", 8.2),
	}}
	geo := fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.3"): paris, tr4("203.0.113.99"): paris, dest: sanFran}
	p := Assess("x.example.test", tr, geo, nil, nil)
	last := p.Hops[2]
	if !last.Dest || !strings.Contains(strings.Join(last.Notes, " "), "different address") {
		t.Fatalf("%+v", last)
	}
	if p.Edge.Kind != "nearby" {
		t.Fatalf("an 8 ms answer for a San Francisco address must be nearby: %+v", p.Edge)
	}
	if p.Edge.ISO != "FR" {
		t.Fatalf("edge location: %+v", p.Edge)
	}
}

func TestTTL1WithAGlobalAddressIsStillYourOwnGateway(t *testing.T) {
	// with IPv6 your home router has a global address that GeoIP places at the ISP's registration
	tr := Trace{Dest: tr4("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "203.0.113.2", 3), hop(2, "203.0.113.3", 6), hop(3, "203.0.113.250", 8),
	}}
	geo := fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.3"): toulouse, tr4("203.0.113.250"): toulouse}
	p := Assess("x.example.test", tr, geo, nil, nil)
	if p.Hops[0].Class != "private" || p.Hops[0].ISO != "" {
		t.Fatalf("hop 1 must be local and unlocated: %+v", p.Hops[0])
	}
	if p.Vantage == nil || p.Vantage.Label != "Toulouse, FR" {
		t.Fatalf("the viewpoint must come from hop 2, not your own router: %+v", p.Vantage)
	}
}

// Real behaviour: OpenAI over IPv6 is registered in London but the answering
// Cloudflare router is in Paris, 7 ms away.
func TestAnswerFromAnotherRouterBeatsTheRegisteredLocation(t *testing.T) {
	dest := tr4("203.0.113.250")
	answering := tr4("203.0.113.99")
	tr := Trace{Dest: dest, Reached: true, Terminal: answering, Hops: []Hop{
		hop(1, "192.168.0.1", 3), hop(2, "203.0.113.3", 5), hop(3, "203.0.113.99", 7),
	}}
	geo := fakeGeo{tr4("203.0.113.3"): paris, answering: paris, dest: london}
	p := Assess("api.example.test", tr, geo, nil, nil)
	if p.Edge.Kind != "nearby" || p.Edge.ISO != "FR" || p.Edge.City != "Paris" {
		t.Fatalf("edge: %+v", p.Edge)
	}
	if !strings.Contains(p.Edge.Note, "answered from FR Paris") || !strings.Contains(p.Edge.Note, "not a machine in GB") {
		t.Fatalf("note: %s", p.Edge.Note)
	}
	if strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("London must not appear: %v", p.Countries)
	}
	if got := p.Route(); got != "you → FR Paris → edge" {
		t.Fatalf("route: %q", got)
	}
}

func TestAnswerFromAnotherRouterInTheSameCountryIsConsistent(t *testing.T) {
	dest, answering := tr4("203.0.113.250"), tr4("203.0.113.99")
	tr := Trace{Dest: dest, Reached: true, Terminal: answering, Hops: []Hop{
		hop(1, "192.168.0.1", 3), hop(2, "203.0.113.3", 5), hop(3, "203.0.113.99", 7),
	}}
	geo := fakeGeo{tr4("203.0.113.3"): paris, answering: paris, dest: toulouse}
	p := Assess("api.example.test", tr, geo, nil, nil)
	if p.Edge.Kind != "consistent" || p.Edge.ISO != "FR" {
		t.Fatalf("edge: %+v", p.Edge)
	}
}

// Real case: from Toulouse, an 8 ms hop that GeoIP puts in London (~900 km). The
// physics allows it (limit 800 km + slack), but only along a perfectly straight cable.
func TestHopThatNeedsAnImpossiblyStraightCableIsNotTrusted(t *testing.T) {
	v := &Point{Lat: toulouse.Lat, Lon: toulouse.Lon, Label: "Toulouse, FR", Source: "given"}
	mk := func(loc geoip.Location, rtt float64) Located {
		tr := Trace{Dest: tr4("203.0.113.250"), Hops: []Hop{hop(1, "192.168.0.1", 1), hop(2, "203.0.113.9", rtt)}}
		return Assess("x", tr, fakeGeo{tr4("203.0.113.9"): loc}, nil, v).Hops[1]
	}
	l := mk(london, 8)
	if l.Confidence != "low" || !strings.Contains(strings.Join(l.Notes, " "), "straight cable") {
		t.Fatalf("London at 8 ms from Toulouse must not be trusted: %+v", l)
	}
	// Paris (590 km) at the same 8 ms is fine, and London at a latency that fits is fine
	if l := mk(paris, 8); l.Confidence == "low" {
		t.Fatalf("Paris at 8 ms is reasonable: %+v", l)
	}
	if l := mk(london, 14); l.Confidence == "low" {
		t.Fatalf("London at 14 ms is reasonable: %+v", l)
	}
	// and when the physics forbids it outright, the note says so
	if l := mk(london, 3); !strings.Contains(strings.Join(l.Notes, " "), "rules out") {
		t.Fatalf("%+v", l)
	}
}

// Real symptom: a hop replied in 11 ms (a slow control-plane answer) but the
// destination further along answered in 5.5 ms, so the hop cannot be in London.
func TestEarlierHopCannotBeFartherThanALaterFasterAnswerAllows(t *testing.T) {
	v := &Point{Lat: toulouse.Lat, Lon: toulouse.Lon, Label: "Toulouse, FR", Source: "given"}
	build := func(lastRTT float64) Path {
		tr := Trace{Dest: tr4("203.0.113.250"), Reached: true, Hops: []Hop{
			hop(1, "192.168.0.1", 1),
			hop(2, "203.0.113.9", 11), // GeoIP: London, 900 km away; 11 ms alone would be plausible
			hop(3, "203.0.113.250", lastRTT),
		}}
		return Assess("x", tr, fakeGeo{tr4("203.0.113.9"): london, tr4("203.0.113.250"): london}, nil, v)
	}
	fast := build(5.5)
	if h := fast.Hops[1]; h.Confidence != "low" || !strings.Contains(strings.Join(h.Notes, " "), "a later hop on the same path answered in 6 ms") {
		t.Fatalf("the London hop must be ruled out by the later 5.5 ms answer: %+v", h)
	}
	if h := fast.Hops[1]; h.RTT != 11 {
		t.Errorf("the measured RTT is still shown as measured, got %v", h.RTT)
	}
	if strings.Join(fast.Countries, ",") != "" {
		t.Errorf("no trustworthy country: %v", fast.Countries)
	}
	// control: when the later hops are slower, the 11 ms hop keeps its own bound and is accepted
	slow := build(30)
	if h := slow.Hops[1]; h.Confidence == "low" {
		t.Fatalf("an 11 ms hop followed by a slower one stays plausible: %+v", h)
	}
}

func TestSilentHopsDoNotBreakTheLaterAnswerBound(t *testing.T) {
	v := &Point{Lat: toulouse.Lat, Lon: toulouse.Lon, Source: "given"}
	tr := Trace{Dest: tr4("203.0.113.250"), Reached: true, Hops: []Hop{
		hop(1, "192.168.0.1", 1), hop(2, "203.0.113.9", 9), silent(3), silent(4), hop(5, "203.0.113.250", 20),
	}}
	p := Assess("x", tr, fakeGeo{tr4("203.0.113.9"): paris, tr4("203.0.113.250"): paris}, nil, v)
	if p.Hops[1].Confidence == "low" {
		t.Fatalf("silent hops have no RTT and must not zero the bound: %+v", p.Hops[1])
	}
}

// Real symptom: one inflated traceroute reply (62 ms) made a Cloudflare anycast
// address look like it really was in Toronto. The TCP handshake says 7 ms.
func TestTCPHandshakeTimeBeatsAnInflatedRouterReply(t *testing.T) {
	dest := tr4("203.0.113.250")
	mk := func(connectMs float64) Path {
		tr := Trace{Dest: dest, Reached: true, ConnectMs: connectMs, Hops: []Hop{
			hop(1, "192.168.0.1", 1), hop(2, "203.0.113.3", 5), hop(3, "203.0.113.250", 62),
		}}
		geo := fakeGeo{tr4("203.0.113.3"): toulouse, dest: toronto}
		return Assess("x", tr, geo, nil, &Point{Lat: toulouse.Lat, Lon: toulouse.Lon, Label: "Toulouse, FR", Source: "given"})
	}
	if p := mk(0); p.Edge.Kind != "consistent" {
		t.Fatalf("without the handshake the 62 ms reply fits Toronto: %+v", p.Edge)
	}
	p := mk(7.1)
	if p.Edge.Kind != "nearby" {
		t.Fatalf("a 7 ms handshake rules Toronto out: %+v", p.Edge)
	}
	d := p.Hops[2]
	if d.RTT != 7.1 || !strings.Contains(strings.Join(d.Notes, " "), "TCP handshake") {
		t.Fatalf("the destination hop should carry the handshake time: %+v", d)
	}
	// a handshake slower than the reply must not make things look farther than they are
	tr := Trace{Dest: dest, Reached: true, ConnectMs: 90, Hops: []Hop{hop(1, "192.168.0.1", 1), hop(2, "203.0.113.250", 20)}}
	if got := Assess("x", tr, nil, nil, nil).Hops[1].RTT; got != 20 {
		t.Errorf("the smaller RTT wins: %v", got)
	}
}

func TestRegisteredCountryOfTheProbedAddressIsNotAnObservedCountry(t *testing.T) {
	dest := tr4("203.0.113.250")
	tr := Trace{Dest: dest, Reached: true, Hops: []Hop{hop(1, "192.168.0.1", 1), hop(2, "203.0.113.250", 140)}}
	p := Assess("x", tr, fakeGeo{dest: sanFran}, nil, &Point{Lat: paris.Lat, Lon: paris.Lon, Source: "given"})
	if len(p.Countries) != 0 {
		t.Fatalf("nothing but the registry says US: %v", p.Countries)
	}
	if p.Edge.Kind != "consistent" || p.Edge.ISO != "US" {
		t.Fatalf("the endpoint line still reports it: %+v", p.Edge)
	}
}

// Modelled on a real trace: a Zayo router whose hostname says cdg15 (Paris) and
// which GeoIP also puts in Paris answered in 5.4 ms, while an ISP router that
// GeoIP puts in Toulouse answered in 3.8 ms. They cannot both be where they are
// said to be, and the hostname-confirmed one is the one to believe.
func zayoScenario() (Trace, fakeGeo, map[netip.Addr]string) {
	dest := tr4("203.0.113.250")
	tr := Trace{Dest: dest, Reached: true, ConnectMs: 5.7, Hops: []Hop{
		hop(1, "10.45.59.254", 5.5),
		hop(2, "203.0.113.3", 3.8), // the ISP router comes first: "earliest hop wins" would pick Toulouse
		hop(3, "203.0.113.2", 5.4),
		hop(4, "203.0.113.250", 6),
	}}
	geo := fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.3"): toulouse, dest: sanFran}
	names := map[netip.Addr]string{tr4("203.0.113.2"): "100.ga-1-1-16.er1.cdg15.fr.zip.zayo.com"}
	return tr, geo, names
}

func TestEstimatedViewpointFollowsTheHostnameConfirmedRouter(t *testing.T) {
	tr, geo, names := zayoScenario()
	p := Assess("x.test", tr, geo, names, nil)
	if p.Vantage == nil || p.Vantage.Label != "Paris, FR" {
		t.Fatalf("the viewpoint must agree with the hostname-confirmed Paris router, got %+v", p.Vantage)
	}
	if z := p.Hops[2]; z.Source != "rdns+geoip" || z.Confidence != "high" {
		t.Fatalf("the Zayo router stays trusted: %+v", z)
	}
	if isp := p.Hops[1]; isp.Confidence != "low" {
		t.Fatalf("an ISP 'Toulouse' that cannot be 590 km away at 3.8 ms is the doubtful one: %+v", isp)
	}
	if len(p.Warnings) != 0 {
		t.Fatalf("nothing contradicts an estimated viewpoint that follows the routers: %v", p.Warnings)
	}
	if strings.Join(p.Countries, ",") != "FR" {
		t.Fatalf("countries: %v", p.Countries)
	}
}

func TestGivenViewpointThatContradictsAConfirmedRouterIsFlagged(t *testing.T) {
	tr, geo, names := zayoScenario()
	v := &Point{Lat: toulouse.Lat, Lon: toulouse.Lon, Label: "Toulouse, FR", Source: "given"}
	p := Assess("x.test", tr, geo, names, v)
	if p.Vantage.Label != "Toulouse, FR" {
		t.Fatalf("a viewpoint the user gave is never replaced: %+v", p.Vantage)
	}
	if len(p.Warnings) != 2 {
		t.Fatalf("want the conflict and the advice, got %v", p.Warnings)
	}
	w := strings.Join(p.Warnings, "\n")
	for _, want := range []string{"hop 3", "hostname and GeoIP", "FR Paris", "Toulouse, FR", "--from"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning is missing %q:\n%s", want, w)
		}
	}
	// and it reaches the user's terminal
	if out := RenderText([]Path{p}, false); !strings.Contains(out, "  ! hop 3") {
		t.Errorf("the warning must be printed:\n%s", out)
	}
}

func TestNoWarningWhenTheViewpointFits(t *testing.T) {
	tr, geo, names := zayoScenario()
	v := &Point{Lat: paris.Lat, Lon: paris.Lon, Label: "Paris, FR", Source: "given"}
	if w := Assess("x.test", tr, geo, names, v).Warnings; len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
}

func TestViewpointTieGoesToTheEarliestHop(t *testing.T) {
	tr := Trace{Dest: tr4("203.0.113.250"), Hops: []Hop{hop(1, "192.168.0.1", 1), hop(2, "203.0.113.2", 8), hop(3, "203.0.113.3", 9)}}
	geo := fakeGeo{tr4("203.0.113.2"): toulouse, tr4("203.0.113.3"): paris}
	if p := Assess("x", tr, geo, nil, nil); p.Vantage.Label != "Toulouse, FR" {
		t.Fatalf("no hostname-confirmed hops to go by: the earliest wins, got %+v", p.Vantage)
	}
}
