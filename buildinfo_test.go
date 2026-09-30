package main

import (
	"runtime/debug"
	"testing"
)

func bi(mainVer string, rev string) *debug.BuildInfo {
	b := &debug.BuildInfo{Main: debug.Module{Version: mainVer}}
	if rev != "" {
		b.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}
	}
	return b
}

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name                string
		ver, commit         string
		info                *debug.BuildInfo
		wantVer, wantCommit string
	}{
		{"release build keeps injected values", "0.1.0", "abc1234", bi("v9.9.9", "deadbeefcafe"), "0.1.0", "abc1234"},
		{"go install @v0.2.0 uses the module version", "dev", "none", bi("v0.2.0", ""), "0.2.0", "none"},
		{"pseudo-version from @latest", "dev", "none", bi("v0.0.0-20260930215414-3d58f3871e1f", ""), "0.0.0-20260930215414-3d58f3871e1f", "none"},
		{"local checkout build stays dev but gets the commit", "dev", "none", bi("(devel)", "3d58f3871e1f0000"), "dev", "3d58f38"},
		{"no build info", "dev", "none", nil, "dev", "none"},
		{"empty module version", "dev", "none", bi("", ""), "dev", "none"},
	}
	for _, c := range cases {
		v, cm := resolveVersion(c.ver, c.commit, c.info)
		if v != c.wantVer || cm != c.wantCommit {
			t.Errorf("%s: got (%q, %q) want (%q, %q)", c.name, v, cm, c.wantVer, c.wantCommit)
		}
	}
}
