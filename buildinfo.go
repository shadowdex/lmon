package main

import (
	"runtime/debug"
	"strings"
)

// resolveVersion picks what `lmon version` prints. Release builds get version
// and commit injected by GoReleaser. A `go install ...@vX.Y.Z` build has
// neither, but Go records the module version (and, for builds from a checkout,
// the git revision) in the binary, so read it from there.
func resolveVersion(ver, commit string, bi *debug.BuildInfo) (string, string) {
	if ver != "dev" || bi == nil {
		return ver, commit
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		ver = strings.TrimPrefix(v, "v") // GoReleaser's {{.Version}} has no leading v either
	}
	if commit == "none" {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				commit = s.Value[:7]
			}
		}
	}
	return ver, commit
}

func versionInfo() (string, string) {
	bi, _ := debug.ReadBuildInfo()
	return resolveVersion(version, commit, bi)
}
