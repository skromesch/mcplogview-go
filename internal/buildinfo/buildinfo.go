// Package buildinfo exposes version metadata, set via -ldflags by GoReleaser.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	version, commit := Version, Commit
	if bi, ok := debug.ReadBuildInfo(); ok {
		// `go install ...@vX.Y.Z` embeds the module version but not ldflags.
		if version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
		if commit == "unknown" {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					commit = s.Value[:7]
				}
			}
		}
	}
	return fmt.Sprintf("version=%s commit=%s date=%s target=%s/%s", version, commit, Date, runtime.GOOS, runtime.GOARCH)
}

// Version returns just the version string.
func ShortVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}
