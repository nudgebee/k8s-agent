package config

import (
	"strings"
	"testing"
)

// FuzzParseHeaders asserts ParseHeaders never panics and produces a
// well-formed header map for any input.
func FuzzParseHeaders(f *testing.F) {
	for _, seed := range []string{
		"",
		"X-Scope-OrgID: tenant-a",
		"X-Scope-OrgID: tenant-a, X-Auth: secret",
		"   X-Foo  :   bar   ",
		":no-key",
		"no-colon",
		"k:",
		",,,",
		"k:v,",
		"k1:v1,,k2:v2",
		"k:multi:colons:here",
		"\x00:\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		h := ParseHeaders(s)
		for k, vs := range h {
			if k == "" {
				t.Fatalf("empty key in result: %q", s)
			}
			if k != strings.TrimSpace(k) {
				t.Fatalf("untrimmed key %q from %q", k, s)
			}
			for _, v := range vs {
				if v != strings.TrimSpace(v) {
					t.Fatalf("untrimmed value %q for key %q from %q", v, k, s)
				}
			}
		}
	})
}

// FuzzParseTargets asserts ParseTargets never panics and produces a
// well-formed target map for any input.
func FuzzParseTargets(f *testing.F) {
	for _, seed := range []string{
		"",
		"foo=http://foo",
		"foo=http://foo;bar=http://bar",
		"  foo  =  http://foo  ",
		"=no-key",
		"no-equals",
		"foo=",
		";;;",
		"foo=bar;",
		"foo=a=b=c",
		"k1=v1;;k2=v2",
		"\x00=\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		m := ParseTargets(s)
		for k, v := range m {
			if k == "" {
				t.Fatalf("empty key in result: %q", s)
			}
			if k != strings.TrimSpace(k) {
				t.Fatalf("untrimmed key %q from %q", k, s)
			}
			if v != strings.TrimSpace(v) {
				t.Fatalf("untrimmed value %q for key %q from %q", v, k, s)
			}
		}
	})
}
