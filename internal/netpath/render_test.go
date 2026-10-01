package netpath

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRouteChain(t *testing.T) {
	tr, geo, names := anycastScenario()
	p := Assess("api.example.test", tr, geo, names, nil)
	// Toulouse twice would be merged; Colt is corrected to Paris so Paris merges too
	if got, want := p.Route(), "you → FR Toulouse → FR Paris → edge"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestRouteMarksUntrustedHopsAndUnreachedDestination(t *testing.T) {
	tr := Trace{Dest: tr4("203.0.113.250"), Reached: false, Hops: []Hop{
		hop(1, "192.168.0.1", 1), hop(2, "203.0.113.2", 2), hop(3, "203.0.113.3", 8), silent(4),
	}}
	geo := fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.3"): tokyo}
	p := Assess("x.example.test", tr, geo, nil, nil)
	if got := p.Route(); got != "you → FR Paris → ? → (destination did not answer)" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderTextTellsTheAnycastStory(t *testing.T) {
	tr, geo, names := anycastScenario()
	out := RenderText([]Path{Assess("api.example.test", tr, geo, names, nil)}, false)
	for _, want := range []string{
		"api.example.test  →  203.0.113.250  (IPv4, reached, 7 hops)",
		"viewpoint: Toulouse, FR (estimated from your first public router; use --from to set it)",
		"route:     you → FR Toulouse → FR Paris → edge",
		"countries seen on the path: FR",
		"registered in San Francisco, US",
		"anycast or edge server close to you",
		"beyond the edge: not observable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "HOP") {
		t.Error("the hop table is only for -v")
	}
}

func TestVerboseAddsHopTableWithNotes(t *testing.T) {
	tr, geo, names := anycastScenario()
	out := RenderText([]Path{Assess("api.example.test", tr, geo, names, nil)}, true)
	for _, want := range []string{"HOP", "(local network)", "(no reply)", "ae2.3211.edge7.par1.neo.colt.net", "GeoIP says London, GB, the hostname says Paris", "(destination)", "rules out"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestComparisonTableForSeveralEndpoints(t *testing.T) {
	tr, geo, names := anycastScenario()
	a := Assess("api.one.test", tr, geo, names, nil)
	far := Trace{Dest: tr4("203.0.113.250"), Reached: true, Hops: []Hop{hop(1, "203.0.113.2", 5), hop(2, "203.0.113.250", 142)}}
	b := Assess("api.two.test", far, fakeGeo{tr4("203.0.113.2"): paris, tr4("203.0.113.250"): sanFran}, nil, nil)
	out := RenderText([]Path{a, b}, false)
	for _, want := range []string{"ENDPOINT", "api.one.test", "near you: FR Paris (within ~880 km)", "US San Francisco", "api.two.test", "consistent with US San Francisco"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if one := RenderText([]Path{a}, false); strings.Contains(one, "ENDPOINT") {
		t.Error("a single path needs no comparison table")
	}
}

func TestPathSerialisesToJSON(t *testing.T) {
	tr, geo, names := anycastScenario()
	b, err := json.Marshal(Assess("api.example.test", tr, geo, names, nil))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"host", "dest", "reached", "hops", "countries", "edge", "vantage"} {
		if _, ok := back[k]; !ok {
			t.Errorf("JSON is missing %q: %s", k, b)
		}
	}
	edge := back["edge"].(map[string]any)
	if edge["kind"] != "nearby" {
		t.Errorf("edge: %v", edge)
	}
}

func TestUnknownEdgePlaceIsNotPrintedAsAQuestionMark(t *testing.T) {
	p := Path{Host: "x.test", Dest: tr4("203.0.113.250"), Reached: true,
		Edge: Edge{Kind: "nearby", MaxKm: 511, Note: "registered in US, but it answers close by"}}
	if got := comparison([]Path{p}); !strings.Contains(got, "near you (within ~511 km)") || strings.Contains(got, ": ?") {
		t.Errorf("comparison:\n%s", got)
	}
	if got := p.Edge.describe(); strings.Contains(got, "?") {
		t.Errorf("describe: %s", got)
	}
	// and the place is not repeated when the note already names it
	e := Edge{Kind: "nearby", ISO: "FR", City: "Paris", Note: "answered from FR Paris (7 ms)"}
	if got := e.describe(); strings.Count(got, "FR Paris") != 1 {
		t.Errorf("repeated place: %s", got)
	}
}
