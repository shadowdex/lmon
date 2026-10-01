package netpath

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestSummaryOfAnAnycastPath(t *testing.T) {
	tr, geo, names := anycastScenario()
	p := Assess("api.example.test", tr, geo, names, nil)
	s := p.Summary(t0)
	if s.Host != "api.example.test" || s.Family != "IPv4" || !s.Reached || s.EdgeKind != "nearby" ||
		strings.Join(s.Countries, ",") != "FR" || s.EdgeISO != "FR" || s.EdgeCity != "Paris" ||
		s.RegisteredISO != "US" || s.Viewpoint != "Toulouse, FR" || !s.SavedAt.Equal(t0) {
		t.Fatalf("%+v", s)
	}
	b, _ := json.Marshal(s)
	// no router addresses or names (the destination, 203.0.113.250, is stored on purpose)
	for _, leak := range []string{`"203.0.113.2"`, `"203.0.113.3"`, `"203.0.113.5"`, `"203.0.113.6"`, "192.168.0.1", "colt.net"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("the summary must not hold hop details, found %q in %s", leak, b)
		}
	}
	if s.Age(t0.Add(3*time.Hour)) != 3*time.Hour {
		t.Error("age")
	}
}

func TestEntersLabels(t *testing.T) {
	cases := []struct {
		s    Summary
		want string
	}{
		{Summary{EdgeKind: "nearby", EdgeISO: "FR", EdgeCity: "Paris"}, "FR Paris"},
		{Summary{EdgeKind: "nearby"}, "near you"},
		{Summary{EdgeKind: "consistent", EdgeISO: "US", EdgeCity: "Seattle"}, "≈ US Seattle"},
		{Summary{EdgeKind: "unknown"}, "unknown"},
		{Summary{}, "unknown"},
	}
	for _, c := range cases {
		if got := c.s.Enters(); got != c.want {
			t.Errorf("%+v -> %q, want %q", c.s, got, c.want)
		}
	}
}

func TestStoreMissingFileIsEmptyAndSaveMergesByHost(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "sub", "paths.json")}
	got, err := st.Load()
	if err != nil || len(got) != 0 {
		t.Fatalf("missing store: %v %v", got, err)
	}
	if err := st.Save(Summary{Host: "a.test", EdgeKind: "nearby", SavedAt: t0}, Summary{Host: "b.test", SavedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(Summary{Host: "a.test", EdgeKind: "unknown", SavedAt: t0.Add(time.Hour)}); err != nil { // newer a.test replaces, b.test stays
		t.Fatal(err)
	}
	got, _ = st.Load()
	if len(got) != 2 || got["a.test"].EdgeKind != "unknown" || got["b.test"].Host != "b.test" {
		t.Fatalf("%+v", got)
	}
	info, _ := os.Stat(st.Path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the file should be private, got %v", info.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(st.Path), ".paths-*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestStoreRecoversFromACorruptFile(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "paths.json")}
	os.WriteFile(st.Path, []byte("{not json"), 0o600)
	if _, err := st.Load(); err == nil {
		t.Fatal("a corrupt file should be reported by Load")
	}
	if err := st.Save(Summary{Host: "a.test"}); err != nil {
		t.Fatalf("Save must replace it: %v", err)
	}
	if got, err := st.Load(); err != nil || got["a.test"].Host != "a.test" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestStoreConcurrentSaves(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "paths.json")}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			st.Save(Summary{Host: string(rune('a'+i)) + ".test"})
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got, err := st.Load(); err != nil || len(got) != 8 {
		t.Fatalf("concurrent saves lost data or corrupted the file: %d %v", len(got), err)
	}
}
