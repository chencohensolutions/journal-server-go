// Package version is the server's version: version/VERSION, the day of the build (2026.11.14, or 2026.11.14.2 for a
// second one that day; the app uses the same scheme), embedded in journal-server (which also runs the import and
// jxl) and in the installer's name. The pre-commit hook in .githooks sets it to today on every commit that changes Go
// code (scripts/bump-version.sh). Versions before Nov 14, 2026 were 1.x.y. See docs/install.md "Versions".
package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed VERSION
var file string

// Number is the version in VERSION, e.g. "2026.11.14".
var Number = strings.TrimSpace(file)

// Build is "release" in the programs .github/workflows/release.yml publishes (set with -ldflags -X), and empty in
// a build from source. Only a release build replaces itself with a newer release (package update).
var Build string

// Released reports whether this is a published release build.
func Released() bool { return Build == "release" }

// String is the version with the build's commit and time from the Go build info: "2026.11.14 (3f2a9c1 2026-11-14)",
// with "dirty" when built with uncommitted changes, or just "2026.11.14" when built without version control.
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
