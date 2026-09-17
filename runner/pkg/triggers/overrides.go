package triggers

import (
	"sort"
	"time"
)

// ApplyRateLimits replaces the suppression window of every matcher named in
// overrides, in place, and reports what happened: `applied` lists the matcher
// names that were changed, `unknown` the requested names that match no matcher.
// Both are sorted so a caller logging them produces stable output.
//
// The caller is expected to log `unknown` loudly. A misspelled matcher name is
// the likely failure of this knob and it is otherwise silent — the operator
// sees the agent start cleanly and concludes the override took effect, when
// nothing changed. MatcherNames() gives the valid set for that message.
//
// Overrides are applied to the specs as given, so this must run before the
// slice is handed to NewEngine. Windows are taken verbatim; validation
// (positive durations only) belongs to whoever parses the operator's input —
// see config.ParseTriggerRateLimits.
func ApplyRateLimits(specs []MatcherSpec, overrides map[string]time.Duration) (applied, unknown []string) {
	if len(overrides) == 0 {
		return nil, nil
	}
	known := make(map[string]bool, len(specs))
	for i := range specs {
		known[specs[i].Name] = true
		if window, ok := overrides[specs[i].Name]; ok {
			specs[i].RateLimit = window
			applied = append(applied, specs[i].Name)
		}
	}
	for name := range overrides {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(applied)
	sort.Strings(unknown)
	return applied, unknown
}

// MatcherNames returns every matcher's name, sorted. Used to tell an operator
// which names TRIGGER_RATE_LIMITS accepts when one of theirs does not match.
func MatcherNames(specs []MatcherSpec) []string {
	out := make([]string, 0, len(specs))
	for i := range specs {
		out = append(out, specs[i].Name)
	}
	sort.Strings(out)
	return out
}
