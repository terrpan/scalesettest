// Package buildinfo provides build-time information (version, commit, build time).
// These variables are injected at build time via -ldflags.
package buildinfo

var (
	// Version is the application version (e.g. "v0.1.0" or "dev").
	// Set via: -ldflags "-X github.com/terrpan/scaleset/internal/buildinfo.Version=<value>"
	Version = "dev"

	// Commit is the git commit hash (e.g. "abc1234def5678").
	// Set via: -ldflags "-X github.com/terrpan/scaleset/internal/buildinfo.Commit=<value>"
	Commit = "unknown"

	// BuildTime is the build timestamp (e.g. "2026-02-19T12:34:56Z").
	// Set via: -ldflags "-X github.com/terrpan/scaleset/internal/buildinfo.BuildTime=<value>"
	BuildTime = "unknown"
)
