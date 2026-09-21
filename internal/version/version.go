// Package version identifies the build a command was made from. The
// variables are set at link time by the Makefile, with -ldflags "-X ...";
// a binary built any other way reports "unknown".
package version

import "fmt"

var (
	Version = "unknown"
	Commit  = "unknown"
	Date    = "unknown"
)

// String describes the build of the named command, for --version.
func VersionString(command string) string {
	return fmt.Sprintf("%s version %s (commit %s on %s)", command, Version, Commit, Date)
}
