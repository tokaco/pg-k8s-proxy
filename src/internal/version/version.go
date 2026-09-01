// Package version exposes build metadata stamped in at link time.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Values injected with -ldflags at build time.
var (
	// Version is the released version, e.g. v0.2.0.
	Version = "dev"
	// Commit is the git revision the binary was built from.
	Commit = ""
	// BuildDate is the RFC 3339 build timestamp.
	BuildDate = ""
)

// String renders a one-line summary of the build.
func String() string {
	commit := Commit
	if commit == "" {
		commit = vcsRevision()
	}
	return fmt.Sprintf("pg-k8s-proxy %s (commit %s, built %s, %s %s/%s)",
		Version, orUnknown(commit), orUnknown(BuildDate),
		runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// vcsRevision recovers the commit from the build info when the linker flags
// were not supplied, so a `go build` still reports something useful.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
