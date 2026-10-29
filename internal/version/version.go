// Package version identifies a build. A release build sets Version, Commit and
// Date with -ldflags; a plain `go build` falls back to what Go recorded.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String is a one-line description such as "1.0.0 (commit abc1234, 2026-10-29, go1.27.2 linux/amd64)".
func String() string {
	commit, date := Commit, Date
	if info, ok := debug.ReadBuildInfo(); ok && (commit == "" || date == "") {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" && len(s.Value) >= 7 {
					commit = s.Value[:7]
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			}
		}
	}
	out := Version
	if commit != "" {
		out += " (commit " + commit
		if date != "" {
			out += ", " + date
		}
		out += ")"
	}
	return fmt.Sprintf("%s [%s %s/%s]", out, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
