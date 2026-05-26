package canonjson

import (
	"math"
	"testing"
)

// Each test case's `want` is what Python emits via:
//
//	json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
//
// after dropping nil entries (Pydantic exclude_none=True). The Python equivalent
// of the Go input is shown in a comment alongside each case so a reviewer can
// verify by running it. The cross-language fixture suite
// will replace these with auto-generated fixtures when it lands.
func TestEncode(t *testing.T) {
	// Build the want strings byte-by-byte for any case where the literal
	// would be interpreted as a Unicode escape by source-level tooling.
	// `bs` is a single backslash; this avoids `\u...` sequences inside
	// raw-string literals being mistaken for escapes.
	bs := "\\"
	wantCtrl01 := `"` + bs + `u0001"`
	wantCtrl1f := `"` + bs + `u001f"`
	wantLatinE := `"` + bs + `u00e9"`
	wantCJK := `"` + bs + `u65e5` + bs + `u672c` + bs + `u8a9e"`
	wantEmojiSurrogate := `"` + bs + `ud83d` + bs + `ude00"`

	cases := []struct {
		name string
		in   any
		want string
	}{
		// Primitives
		{"nil", nil, `null`},
		{"true", true, `true`},
		{"false", false, `false`},
		{"int_zero", 0, `0`},
		{"int_pos", 42, `42`},
		{"int_neg", -7, `-7`},
		{"int64_max", int64(9223372036854775807), `9223372036854775807`},
		{"uint64_max", uint64(18446744073709551615), `18446744073709551615`},

		// Float — Python preserves trailing .0 for whole-number floats.
		// json.dumps(1.0) => '1.0'   (NOT '1')
		// json.dumps(1)   => '1'
		{"float_one", 1.0, `1.0`},
		{"float_zero", 0.0, `0.0`},
		{"float_neg_one", -1.0, `-1.0`},
		{"float_tenth", 0.1, `0.1`},
		{"float_small", 1e-7, `1e-07`},
		{"float_large", 1e16, `1e+16`},

		// String basics
		{"str_empty", "", `""`},
		{"str_ascii", "hello", `"hello"`},
		// json.dumps('a"b\\c') => '"a\\"b\\\\c"'
		{"str_quote_backslash", `a"b\c`, `"a\"b\\c"`},
		// json.dumps('\b\f\n\r\t') => '"\\b\\f\\n\\r\\t"'
		{"str_short_escapes", "\b\f\n\r\t", `"\b\f\n\r\t"`},
		// Other control chars: json.dumps('\x01') => '"\\u0001"'
		{"str_ctrl_low", "\x01", wantCtrl01},
		{"str_ctrl_x1f", "\x1f", wantCtrl1f},

		// Non-ASCII — ensure_ascii=True escapes everything >= 0x80.
		// json.dumps('é') => '"\\u00e9"'
		{"str_latin", "é", wantLatinE},
		// json.dumps('日本語') => '"\\u65e5\\u672c\\u8a9e"'
		{"str_cjk", "日本語", wantCJK},
		// json.dumps('\U0001f600') => '"\\ud83d\\ude00"' (surrogate pair)
		{"str_emoji_surrogate", "\U0001f600", wantEmojiSurrogate},

		// Python does NOT HTML-escape <, >, &; Go's encoding/json default does.
		// We must NOT escape them.
		{"str_html_unescaped", "<a>&", `"<a>&"`},

		// Arrays
		{"arr_empty", []any{}, `[]`},
		{"arr_mixed", []any{1, "x", true, nil}, `[1,"x",true,null]`},
		// Nil entries inside arrays are kept as null (only map entries are dropped).
		{"arr_with_nil", []any{nil, 1, nil}, `[null,1,null]`},

		// Objects — keys sorted by Unicode code point, no whitespace.
		{"obj_empty", map[string]any{}, `{}`},
		// json.dumps({"b":1,"a":2}, sort_keys=True, separators=(",",":")) => '{"a":2,"b":1}'
		{"obj_sorted", map[string]any{"b": 1, "a": 2}, `{"a":2,"b":1}`},
		// exclude_none drops the "x" entry.
		{"obj_drops_nil", map[string]any{"a": 1, "x": nil, "b": 2}, `{"a":1,"b":2}`},
		// Sort is by Unicode code point: uppercase letters precede lowercase.
		{"obj_codepoint_sort", map[string]any{"a": 1, "Z": 2, "B": 3}, `{"B":3,"Z":2,"a":1}`},
		// Nested: still no whitespace; child keys also sorted.
		{
			"obj_nested",
			map[string]any{
				"outer": map[string]any{"y": 2, "x": 1},
				"list":  []any{1, 2, 3},
			},
			`{"list":[1,2,3],"outer":{"x":1,"y":2}}`,
		},

		// Realistic body shape.
		{
			"action_request_body",
			map[string]any{
				"action_name":   "prometheus_queries_enricher",
				"timestamp":     int64(1700000000),
				"action_params": map[string]any{"query": "up", "duration": int64(60)},
				"sinks":         nil, // dropped
				"origin":        "services-server",
			},
			`{"action_name":"prometheus_queries_enricher","action_params":{"duration":60,"query":"up"},"origin":"services-server","timestamp":1700000000}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Encode(c.in)
			if err != nil {
				t.Fatalf("Encode(%v) error: %v", c.in, err)
			}
			if string(got) != c.want {
				t.Errorf("Encode(%v)\n got:  %s\n want: %s", c.in, got, c.want)
			}
		})
	}
}

// EncodeForSignature uses Python's default separators (", " and ": ").
// Verified against:
//
//	json.dumps(obj, sort_keys=True)   # default separators
//
// (no separators argument), as in sign_action_request line 40.
func TestEncodeForSignature(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"obj_simple", map[string]any{"a": 1, "b": 2}, `{"a": 1, "b": 2}`},
		{"arr_simple", []any{1, 2, 3}, `[1, 2, 3]`},
		{"nested", map[string]any{"action_name": "x", "action_params": map[string]any{"q": "up"}},
			`{"action_name": "x", "action_params": {"q": "up"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EncodeForSignature(c.in)
			if err != nil {
				t.Fatalf("EncodeForSignature error: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("EncodeForSignature(%v)\n got:  %s\n want: %s", c.in, got, c.want)
			}
		})
	}
}

func TestEncodeRejectsNaNInf(t *testing.T) {
	for _, in := range []any{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Encode(in); err == nil {
			t.Errorf("Encode(%v) = no error; want error", in)
		}
	}
}

func TestEncodeRejectsUnsupportedType(t *testing.T) {
	type myStruct struct{ X int }
	if _, err := Encode(myStruct{X: 1}); err == nil {
		t.Error("Encode(struct) = no error; want error (structs must be converted to map first)")
	}
}
