package netpath

import (
	"bytes"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

func render(t *testing.T, paths []Path, attribution string) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteHTML(&b, paths, attribution); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func scenarioPaths() []Path {
	tr, geo, names := anycastScenario()
	a := Assess("api.one.test", tr, geo, names, nil)
	far := Trace{Dest: tr4("203.0.113.250"), Reached: true, Hops: []Hop{hop(1, "192.168.0.1", 2), hop(2, "203.0.113.2", 5), hop(3, "203.0.113.250", 142)}}
	b := Assess("api.two.test", far, fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.250"): sanFran}, nil, nil)
	return []Path{a, b}
}

func TestProjectionAnchors(t *testing.T) {
	x, y := project(0, 0)
	if math.Abs(x-500) > 1e-9 || math.Abs(y-84*mapK) > 1e-9 {
		t.Fatalf("(0,0) -> %.2f,%.2f", x, y)
	}
	if x, _ := project(0, -180); x != 0 {
		t.Errorf("lon -180 -> x=%v", x)
	}
	if x, _ := project(0, 180); math.Abs(x-1000) > 1e-9 {
		t.Errorf("lon 180 -> x=%v", x)
	}
	if _, y := project(84, 0); y != 0 {
		t.Errorf("lat 84 -> y=%v", y)
	}
	if _, y := project(-58, 0); math.Abs(y-mapH) > 1e-9 {
		t.Errorf("lat -58 -> y=%v want %v", y, mapH)
	}
	// Paris is east of and north of the equator crossing: sanity check the orientation
	px, py := project(48.8566, 2.3522)
	sx, sy := project(37.79, -122.4)
	if !(px > sx) || !(py < sy) {
		t.Errorf("Paris must be east (%.0f>%.0f) and north (%.0f<%.0f) of San Francisco", px, sx, py, sy)
	}
}

func TestWorldOutlineIsEmbeddedAndWithinTheGrid(t *testing.T) {
	if len(worldPath) < 50_000 || !strings.HasPrefix(worldPath, "M") || !strings.HasSuffix(worldPath, "Z") {
		t.Fatalf("world outline looks wrong: %d bytes", len(worldPath))
	}
	for _, m := range regexp.MustCompile(`-?\d+\.?\d*`).FindAllString(worldPath, -1) {
		var v float64
		if _, err := fmtSscan(m, &v); err != nil || v < -0.01 || v > 1000.01 {
			t.Fatalf("coordinate %q is outside the map grid", m)
		}
	}
}

func TestArcBulgesAwayFromTheChordAndSkipsZeroLength(t *testing.T) {
	if arc(10, 10, 10, 10) != "" || arc(10, 10, 10.2, 10.1) != "" {
		t.Error("a zero-length arc must be skipped")
	}
	ctrl := func(d string) (float64, float64) {
		m := regexp.MustCompile(`Q([\d.]+) ([\d.]+)`).FindStringSubmatch(d)
		if m == nil {
			t.Fatalf("no control point in %q", d)
		}
		var cx, cy float64
		fmtSscan(m[1], &cx)
		fmtSscan(m[2], &cy)
		return cx, cy
	}
	// horizontal chords bulge towards the top of the map, in either direction
	for _, c := range [][4]float64{{100, 100, 300, 100}, {300, 100, 100, 100}} {
		if _, cy := ctrl(arc(c[0], c[1], c[2], c[3])); cy >= 100 {
			t.Errorf("%v: the control point %.1f must be above the chord (y=100)", c, cy)
		}
	}
	// a vertical chord has no "up" to bulge towards, but must still leave the straight line
	if cx, cy := ctrl(arc(100, 100, 100, 300)); math.Abs(cx-100) < 10 || math.Abs(cy-200) > 1 {
		t.Errorf("vertical chord: control point (%.1f, %.1f) should sit beside the middle (100, 200)", cx, cy)
	}
}

func TestZoomBoxFramesPointsAndKeepsAspect(t *testing.T) {
	box, z := zoomBox([][2]float64{pt(48.85, 2.35), pt(43.6, 1.44)})
	var x0, y0, w, h float64
	fmtSscan4(box, &x0, &y0, &w, &h)
	if math.Abs(w/h-mapW/mapH) > 0.01 {
		t.Errorf("aspect %.3f, want %.3f (%s)", w/h, mapW/mapH, box)
	}
	px, py := project(48.85, 2.35)
	if px < x0 || px > x0+w || py < y0 || py > y0+h {
		t.Errorf("Paris lies outside %s", box)
	}
	if z < 5 {
		t.Errorf("two French cities should zoom in a lot, factor %.1f", z)
	}
	if b, z := zoomBox(nil); z != 1 || !strings.HasPrefix(b, "0 0 1000") {
		t.Errorf("no points -> whole world, got %s %.1f", b, z)
	}
	// far-apart points fall back to the whole map
	if _, z := zoomBox([][2]float64{pt(48.85, 2.35), pt(-33.9, 151.2), pt(37.8, -122.4)}); z != 1 {
		t.Errorf("a global spread must show the whole world, factor %.2f", z)
	}
}

func TestHTMLPageContainsTheStoryAndIsSelfContained(t *testing.T) {
	out := render(t, scenarioPaths(), "IP geolocation by DB-IP.com (CC BY 4.0)")
	for _, want := range []string{
		"<!doctype html>", `id="map"`, `data-view="world"`, `data-view="zoom"`,
		`id="route-0"`, `id="route-1"`, "api.one.test", "api.two.test",
		"you → FR Toulouse → FR Paris → edge",
		"anycast or edge server close to you", // the endpoint note
		"not observable from here",            // the honest limit
		"registered · US San Francisco",       // the dashed-ring label
		"edge · FR Paris", "IP geolocation by DB-IP.com (CC BY 4.0)", "Natural Earth",
		"(destination)", "rules out", // hop-table notes
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// self-contained: nothing is fetched from anywhere
	if re := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`); re.MatchString(out) {
		t.Error("the page must not reference external resources")
	}
	if strings.Contains(out, "<script src") || strings.Contains(out, "@import") || strings.Contains(out, "url(http") {
		t.Error("the page must not load scripts or styles")
	}
	if len(out) > 400_000 {
		t.Errorf("page is %d bytes", len(out))
	}
}

func TestHTMLEscapesUntrustedHostnamesAndNotes(t *testing.T) {
	tr, geo, _ := anycastScenario()
	evil := `</title><script>alert(1)</script>`
	names := map[string]string{}
	_ = names
	p := Assess(evil+".example.test", tr, geo, map[netip.Addr]string{tr4("203.0.113.5"): evil + ".par1.neo.colt.net"}, nil)
	out := render(t, []Path{p}, "")
	if strings.Contains(out, "<script>alert(1)") {
		t.Fatalf("hostname from reverse DNS was not escaped")
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)") {
		t.Errorf("expected the escaped text to be present")
	}
	// the page's own script is the only one
	if n := strings.Count(out, "<script"); n != 1 {
		t.Errorf("%d script elements, want exactly the page's own", n)
	}
}

func TestHTMLDrawsOnlyWhatIsTrustedAndShowsTheRest(t *testing.T) {
	out := render(t, scenarioPaths()[:1], "")
	// anycast scenario: the destination's registered San Francisco is a dashed ring and a dashed arc, not a route stop
	if !strings.Contains(out, `class="arc ghost"`) || !strings.Contains(out, `class="registered"`) {
		t.Error("the registered location should be a dashed ring on a dashed arc")
	}
	if strings.Contains(out, "hop 6: 203.0.113.250") {
		t.Error("the impossible destination location must not be drawn as an observed hop")
	}
}

func TestHTMLWithNothingToDrawStillRenders(t *testing.T) {
	tr := Trace{Dest: tr4("203.0.113.250"), Hops: []Hop{hop(1, "192.168.0.1", 1), silent(2)}}
	out := render(t, []Path{Assess("x.example.test", tr, nil, nil, nil)}, "")
	if !strings.Contains(out, "x.example.test") || !strings.Contains(out, "no reply") {
		t.Error("the table should still be there")
	}
	if out := render(t, nil, ""); !strings.Contains(out, "<svg") {
		t.Error("no paths at all must still produce a valid page")
	}
}

func fmtSscan(s string, v *float64) (int, error) { return fmt.Sscan(s, v) }

func fmtSscan4(s string, a, b, c, d *float64) { fmt.Sscan(s, a, b, c, d) }

func pt(lat, lon float64) [2]float64 { x, y := project(lat, lon); return [2]float64{x, y} }

func labelCount(out, text string) int {
	return len(regexp.MustCompile(`<text class="lbl[^"]*"[^>]*>`+regexp.QuoteMeta(text)+`</text>`).FindAllString(out, -1))
}

func TestIdenticalLabelsAtOnePlaceAreDrawnOnce(t *testing.T) {
	tr, geo, names := anycastScenario()
	var paths []Path
	for _, h := range []string{"a.test", "b.test", "c.test"} { // three endpoints entering at the same edge
		paths = append(paths, Assess(h, tr, geo, names, nil))
	}
	out := render(t, paths, "")
	if n := labelCount(out, "edge · FR Paris"); n != 1 {
		t.Errorf("the shared edge label is drawn %d times, want once", n)
	}
	if n := labelCount(out, "registered · US San Francisco"); n != 1 {
		t.Errorf("the shared registered label is drawn %d times, want once", n)
	}
	// but every route's marker still has its own hover text
	if n := strings.Count(out, "<title>registered in San Francisco, US, but it answers within about"); n != 3 {
		t.Errorf("each of the 3 routes keeps its edge tooltip, found %d", n)
	}
}

func TestDifferentLabelsAtTheSamePlaceAreStacked(t *testing.T) {
	paths := []hPath{{Markers: []hMarker{
		{X: 500, Y: 100, Label: "edge · FR Paris"},
		{X: 500.5, Y: 100.5, Label: "registered · GB London"},
	}}, {Markers: []hMarker{{X: 500, Y: 100, Label: "edge · FR Paris"}}}}
	declutter(paths)
	a, b := paths[0].Markers[0], paths[0].Markers[1]
	if a.Label == "" || b.Label == "" || a.DY == b.DY {
		t.Fatalf("two different labels on one spot must not overlap: %+v %+v", a, b)
	}
	if paths[1].Markers[0].Label != "" {
		t.Error("the repeated label on the second route should be dropped")
	}
}
