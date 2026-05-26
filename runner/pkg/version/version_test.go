package version

import "testing"

// CurrentVersion prefers RUNNER_VERSION over the build-time ldflag.
// The Helm chart sets RUNNER_VERSION={{.Chart.AppVersion}} on the
// runner container (charts/nudgebee-agent/templates/_helpers.tpl:177);
// the UI's Agent Health card surfaces this as the user-facing version.
func TestCurrentVersion(t *testing.T) {
	t.Run("env_overrides_build_version", func(t *testing.T) {
		t.Setenv("RUNNER_VERSION", "0.0.124")
		Version = "2026-05-13T08-48-09_1c6d54c"
		if got := CurrentVersion(); got != "0.0.124" {
			t.Errorf("CurrentVersion() = %q; want 0.0.124", got)
		}
	})

	t.Run("falls_back_to_build_version_when_env_unset", func(t *testing.T) {
		t.Setenv("RUNNER_VERSION", "")
		Version = "build-sha-fallback"
		if got := CurrentVersion(); got != "build-sha-fallback" {
			t.Errorf("CurrentVersion() = %q; want build-sha-fallback", got)
		}
	})

	t.Run("empty_env_treated_as_unset", func(t *testing.T) {
		// An explicit empty value should fall back rather than emit "".
		t.Setenv("RUNNER_VERSION", "")
		Version = "v0.0.99"
		if got := CurrentVersion(); got != "v0.0.99" {
			t.Errorf("CurrentVersion() = %q; want v0.0.99", got)
		}
	})
}
