// Package version carries build identity, stamped in at link time.
//
// The Makefile sets these with -ldflags; a plain `go build` leaves the
// defaults, which is itself useful information in a log line.
package version

import "runtime/debug"

var (
	// Version is the release version, e.g. "0.1.0".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is an RFC3339 timestamp of the build.
	BuildDate = "unknown"
)

// String renders the build identity for logs and the -version flag.
func String() string {
	return Version + " (" + Commit + ", built " + BuildDate + ")"
}

// GoVersion reports the toolchain the binary was compiled with.
func GoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}
