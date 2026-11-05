// Package version is the server's version: version/VERSION (major.minor.patch), embedded in every program built
// from this module (journal-server, journal-import, journal-jxl) and in the installer's name. The pre-commit hook in
// .githooks raises the patch number on every commit that changes Go code; scripts/bump-version.sh raises the minor
// or major number. See docs/install.md "Versions".
package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed VERSION
var file string

// Number is the version in VERSION, e.g. "1.4.2".
var Number = strings.TrimSpace(file)

// String is the version with the build's commit and time from the Go build info: "1.4.2 (3f2a9c1 2026-10-23)",
// with "dirty" when built with uncommitted changes, or just "1.4.2" when built without version control.
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Number
	}
	var revision, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return Number
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if len(at) >= 10 {
		revision += " " + at[:10]
	}
	if dirty {
		revision += " dirty"
	}
	return Number + " (" + revision + ")"
}
