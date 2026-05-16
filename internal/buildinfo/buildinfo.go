// Package buildinfo exposes build-time metadata (version, commit, build date)
// that goreleaser injects via -ldflags. The defaults below are what you'll see
// when binaries are produced by `go build` directly rather than by goreleaser.
package buildinfo

// Version is the semver-ish release tag (e.g. "v1.2.3"). Set via -ldflags at
// release time; defaults to "dev" for local builds.
var Version = "dev"

// Commit is the short git SHA the binary was built from. Set via -ldflags at
// release time; empty for local builds.
var Commit = ""

// Date is the build timestamp (RFC 3339). Set via -ldflags at release time;
// empty for local builds.
var Date = ""

// String renders a human-readable one-liner suitable for cobra's `--version`
// output. Goreleaser-built binaries print e.g. "v1.2.3 (abc1234, 2026-05-16)";
// local builds print just "dev".
func String() string {
	if Commit == "" && Date == "" {
		return Version
	}
	extra := ""
	switch {
	case Commit != "" && Date != "":
		extra = " (" + Commit + ", " + Date + ")"
	case Commit != "":
		extra = " (" + Commit + ")"
	case Date != "":
		extra = " (" + Date + ")"
	}
	return Version + extra
}
