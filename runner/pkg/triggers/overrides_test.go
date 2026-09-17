package triggers

import (
	"reflect"
	"testing"
	"time"
)

func specByName(t *testing.T, specs []MatcherSpec, name string) MatcherSpec {
	t.Helper()
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no matcher named %q", name)
	return MatcherSpec{}
}

func TestApplyRateLimits_OverridesNamedMatchersOnly(t *testing.T) {
	specs := Builtins()
	oomBefore := specByName(t, specs, "pod_oom_killed").RateLimit

	applied, unknown := ApplyRateLimits(specs, map[string]time.Duration{"pod_crash_loop": 5 * time.Minute})

	if got := specByName(t, specs, "pod_crash_loop").RateLimit; got != 5*time.Minute {
		t.Errorf("pod_crash_loop RateLimit = %v; want 5m", got)
	}
	if got := specByName(t, specs, "pod_oom_killed").RateLimit; got != oomBefore {
		t.Errorf("pod_oom_killed RateLimit = %v; want %v (untouched)", got, oomBefore)
	}
	if !reflect.DeepEqual(applied, []string{"pod_crash_loop"}) {
		t.Errorf("applied = %v; want [pod_crash_loop]", applied)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v; want none", unknown)
	}
}

func TestApplyRateLimits_ReportsUnknownNames(t *testing.T) {
	// The whole failure mode of this knob is a typo silently doing nothing, so
	// the caller has to be able to log it.
	specs := Builtins()
	applied, unknown := ApplyRateLimits(specs, map[string]time.Duration{
		"pod_crashloop":  time.Minute, // missing underscore
		"pod_crash_loop": time.Minute,
	})
	if !reflect.DeepEqual(applied, []string{"pod_crash_loop"}) {
		t.Errorf("applied = %v; want [pod_crash_loop]", applied)
	}
	if !reflect.DeepEqual(unknown, []string{"pod_crashloop"}) {
		t.Errorf("unknown = %v; want [pod_crashloop]", unknown)
	}
}

func TestApplyRateLimits_EmptyIsANoop(t *testing.T) {
	specs := Builtins()
	before := specByName(t, specs, "pod_crash_loop").RateLimit
	for _, overrides := range []map[string]time.Duration{nil, {}} {
		applied, unknown := ApplyRateLimits(specs, overrides)
		if applied != nil || unknown != nil {
			t.Errorf("applied=%v unknown=%v; want both nil", applied, unknown)
		}
	}
	if got := specByName(t, specs, "pod_crash_loop").RateLimit; got != before {
		t.Errorf("RateLimit = %v; want %v (unchanged)", got, before)
	}
}

func TestApplyRateLimits_ResultsAreSorted(t *testing.T) {
	// Map iteration order is random; a caller logging these should not emit a
	// different line every restart.
	specs := Builtins()
	applied, unknown := ApplyRateLimits(specs, map[string]time.Duration{
		"pod_oom_killed": time.Minute,
		"job_failure":    time.Minute,
		"zzz_nope":       time.Minute,
		"aaa_nope":       time.Minute,
	})
	if !reflect.DeepEqual(applied, []string{"job_failure", "pod_oom_killed"}) {
		t.Errorf("applied = %v; want sorted [job_failure pod_oom_killed]", applied)
	}
	if !reflect.DeepEqual(unknown, []string{"aaa_nope", "zzz_nope"}) {
		t.Errorf("unknown = %v; want sorted [aaa_nope zzz_nope]", unknown)
	}
}

func TestApplyRateLimits_TakesTheWindowVerbatim(t *testing.T) {
	// Validation belongs to the parser, not here: this must not quietly
	// substitute a value the operator did not ask for.
	specs := Builtins()
	ApplyRateLimits(specs, map[string]time.Duration{"pod_crash_loop": 3 * time.Second})
	if got := specByName(t, specs, "pod_crash_loop").RateLimit; got != 3*time.Second {
		t.Errorf("RateLimit = %v; want 3s", got)
	}
}

func TestMatcherNames_ListsEveryMatcherSorted(t *testing.T) {
	names := MatcherNames(Builtins())
	if len(names) != len(Builtins()) {
		t.Fatalf("MatcherNames returned %d names for %d matchers", len(names), len(Builtins()))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("MatcherNames not sorted: %q before %q", names[i-1], names[i])
		}
	}
	// The names the docs and chart tell operators to use must exist.
	want := map[string]bool{"pod_crash_loop": true, "pod_oom_killed": true, "image_pull_backoff": true}
	for _, n := range names {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("documented matcher names missing from Builtins: %v", want)
	}
}
