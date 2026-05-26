// Package version exposes linker-stamped build metadata (version, commit,
// build time) used by the agent's /healthz, /metrics, and WS greeting.
package version

import "os"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// CurrentVersion returns the user-facing agent version. Prefers the
// `RUNNER_VERSION` env var (set by the Helm chart to `.Chart.AppVersion`,
// e.g. `0.0.124`) over the build-time `Version` ldflag (e.g.
// `2026-05-13T08-48-09_1c6d54c…`).
//
// The chart version is what the UI's Agent Health panel and the
// "your agent version is out of date" comparison expect. The build
// SHA is still useful for debugging and is exposed via the startup
// banner + the `Commit` / `BuildTime` package vars.
func CurrentVersion() string {
	if v := os.Getenv("RUNNER_VERSION"); v != "" {
		return v
	}
	return Version
}
