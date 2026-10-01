package netpath

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func refreshOpts(t *testing.T, failHost string) Options {
	return Options{
		Runner:  fakeRunner(t, "macos_v4.txt"),
		NoRDNS:  true,
		Connect: noConnect,
		Geo:     fakeGeo{netip.MustParseAddr("203.0.113.1"): paris, netip.MustParseAddr("203.0.113.11"): sanFran},
		Dial: func(_ context.Context, host string, _ int) (netip.Addr, error) {
			if host == failHost {
				return netip.Addr{}, errors.New("no such host")
			}
			return netip.MustParseAddr("203.0.113.11"), nil
		},
	}
}

func TestRefreshSavesSummariesAndOneFailureDoesNotStopTheRest(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "paths.json")}
	clock := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	paths, errs := Refresh(context.Background(), []string{"a.test", "broken.test", "b.test"}, refreshOpts(t, "broken.test"), st, 5*time.Second, func() time.Time { return clock })

	if len(paths) != 2 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.test") {
		t.Fatalf("paths=%d errs=%v", len(paths), errs)
	}
	got, err := st.Load()
	if err != nil || len(got) != 2 || got["a.test"].Host != "a.test" || got["b.test"].Host != "b.test" {
		t.Fatalf("%v %v", got, err)
	}
	if _, bad := got["broken.test"]; bad {
		t.Error("a failed host must not be saved")
	}
	if !got["a.test"].SavedAt.Equal(clock) {
		t.Errorf("saved_at = %v", got["a.test"].SavedAt)
	}
}

func TestRefreshKeepsOldSummariesOfHostsThatFailNow(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "paths.json")}
	old := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	st.Save(Summary{Host: "broken.test", EdgeKind: "nearby", EdgeISO: "FR", SavedAt: old})
	Refresh(context.Background(), []string{"broken.test"}, refreshOpts(t, "broken.test"), st, time.Second, nil)
	got, _ := st.Load()
	if !got["broken.test"].SavedAt.Equal(old) || got["broken.test"].EdgeISO != "FR" {
		t.Fatalf("a failed refresh must leave the last good summary alone: %+v", got["broken.test"])
	}
}

func TestRefreshStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := Store{Path: filepath.Join(t.TempDir(), "paths.json")}
	paths, errs := Refresh(ctx, []string{"a.test", "b.test"}, refreshOpts(t, ""), st, time.Second, nil)
	if len(paths) != 0 || len(errs) == 0 {
		t.Fatalf("paths=%d errs=%v", len(paths), errs)
	}
}
