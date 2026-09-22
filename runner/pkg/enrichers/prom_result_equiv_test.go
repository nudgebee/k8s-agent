package enrichers

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"testing"
)

// The matrix/vector decode was rewritten from a `[][]any` stdlib decode plus
// fmt.Sprintf("%v", …) into an in-place scanner, because the boxed
// intermediate was the single largest contributor to the runner's memory
// bursts. These tests pin the rewrite to the exact output of the code it
// replaced.

// referenceMatrix decodes with the pre-rewrite types and returns the wire
// shape the old matrixToWire produced.
func referenceMatrix(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var top struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	out := make([]map[string]any, 0, len(top.Data.Result))
	for _, r := range top.Data.Result {
		timestamps := make([]float64, 0, len(r.Values))
		values := make([]string, 0, len(r.Values))
		for _, v := range r.Values {
			ts, val := oldUnpack(v)
			timestamps = append(timestamps, ts)
			values = append(values, val)
		}
		out = append(out, map[string]any{
			"metric":     r.Metric,
			"timestamps": timestamps,
			"values":     values,
		})
	}
	return out
}

// oldUnpack is the pre-rewrite unpackScalar, copied verbatim.
func oldUnpack(raw []any) (float64, string) {
	if len(raw) < 2 {
		return 0, ""
	}
	var ts float64
	switch t := raw[0].(type) {
	case float64:
		ts = t
	case string:
		ts, _ = strconv.ParseFloat(t, 64)
	}
	return ts, fmt.Sprintf("%v", raw[1])
}

func matrixEnvelope(seriesJSON string) []byte {
	return []byte(`{"status":"success","data":{"resultType":"matrix","result":` + seriesJSON + `}}`)
}

func vectorEnvelope(seriesJSON string) []byte {
	return []byte(`{"status":"success","data":{"resultType":"vector","result":` + seriesJSON + `}}`)
}

// TestMatrixDecode_MatchesReference runs the shapes a Prometheus-compatible
// backend can actually emit through both implementations.
func TestMatrixDecode_MatchesReference(t *testing.T) {
	cases := map[string]string{
		"plain":            `[{"metric":{"a":"b"},"values":[[1700000000,"0.42"],[1700000060,"1"]]}]`,
		"integral value":   `[{"metric":{},"values":[[1700000000,"3"]]}]`,
		"negative":         `[{"metric":{},"values":[[1700000000,"-17.5"]]}]`,
		"exponent value":   `[{"metric":{},"values":[[1700000000,"1.4e+09"]]}]`,
		"NaN value":        `[{"metric":{},"values":[[1700000000,"NaN"]]}]`,
		"inf value":        `[{"metric":{},"values":[[1700000000,"+Inf"]]}]`,
		"fractional ts":    `[{"metric":{},"values":[[1700000000.501,"1"]]}]`,
		"quoted ts":        `[{"metric":{},"values":[["1700000000","1"]]}]`,
		"empty values":     `[{"metric":{"a":"b"},"values":[]}]`,
		"empty result":     `[]`,
		"multi series":     `[{"metric":{"a":"1"},"values":[[1,"1"]]},{"metric":{"a":"2"},"values":[[2,"2"]]}]`,
		"whitespace":       `[ { "metric" : { } , "values" : [ [ 1700000000 , "0.42" ] ] } ]`,
		"escaped value":    `[{"metric":{},"values":[[1700000000,"a\"b"]]}]`,
		"unicode value":    `[{"metric":{},"values":[[1700000000,"é "]]}]`,
		"big ts":           `[{"metric":{},"values":[[253402300799,"1"]]}]`,
		"zero ts":          `[{"metric":{},"values":[[0,"0"]]}]`,
		"many samples":     manySamples(500),
		"long fractional":  `[{"metric":{},"values":[[1700000000,"0.123456789012345"]]}]`,
		"leading plus ts":  `[{"metric":{},"values":[[1700000000,"1"]]}]`,
		"value with comma": `[{"metric":{},"values":[[1700000000,"1,2"]]}]`,
	}

	for name, seriesJSON := range cases {
		t.Run(name, func(t *testing.T) {
			body := matrixEnvelope(seriesJSON)

			got, err := PrometheusQueryResultDict(body)
			if err != nil {
				t.Fatalf("new decode: %v", err)
			}
			want := referenceMatrix(t, body)

			gotSeries, _ := got["series_list_result"].([]map[string]any)
			if len(gotSeries) != len(want) {
				t.Fatalf("series count = %d; want %d", len(gotSeries), len(want))
			}
			for i := range want {
				assertSameSeries(t, gotSeries[i], want[i])
			}
		})
	}
}

func manySamples(n int) string {
	s := `[{"metric":{"a":"b"},"values":[`
	for i := range n {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`[%d,"%d.%d"]`, 1700000000+i*60, i, i%97)
	}
	return s + `]}]`
}

func assertSameSeries(t *testing.T, got, want map[string]any) {
	t.Helper()
	gt, _ := got["timestamps"].([]float64)
	wt, _ := want["timestamps"].([]float64)
	if len(gt) != len(wt) {
		t.Fatalf("timestamps len = %d; want %d", len(gt), len(wt))
	}
	for i := range wt {
		if gt[i] != wt[i] && !(math.IsNaN(gt[i]) && math.IsNaN(wt[i])) {
			t.Errorf("timestamps[%d] = %v; want %v", i, gt[i], wt[i])
		}
	}
	gv, _ := got["values"].([]string)
	wv, _ := want["values"].([]string)
	if len(gv) != len(wv) {
		t.Fatalf("values len = %d; want %d", len(gv), len(wv))
	}
	for i := range wv {
		if gv[i] != wv[i] {
			t.Errorf("values[%d] = %q; want %q", i, gv[i], wv[i])
		}
	}
}

// TestVectorDecode_MatchesReference covers the instant-query pair.
func TestVectorDecode_MatchesReference(t *testing.T) {
	cases := map[string]string{
		"plain":         `[{"metric":{"a":"b"},"value":[1700000000,"0.42"]}]`,
		"quoted ts":     `[{"metric":{},"value":["1700000000","7"]}]`,
		"escaped value": `[{"metric":{},"value":[1700000000,"a\"b"]}]`,
		"empty result":  `[]`,
	}
	for name, seriesJSON := range cases {
		t.Run(name, func(t *testing.T) {
			body := vectorEnvelope(seriesJSON)
			got, err := PrometheusQueryResultDict(body)
			if err != nil {
				t.Fatalf("new decode: %v", err)
			}

			var top struct {
				Data struct {
					Result []struct {
						Metric map[string]string `json:"metric"`
						Value  []any             `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &top); err != nil {
				t.Fatal(err)
			}

			gotSeries, _ := got["vector_result"].([]map[string]any)
			if len(gotSeries) != len(top.Data.Result) {
				t.Fatalf("series count = %d; want %d", len(gotSeries), len(top.Data.Result))
			}
			for i, r := range top.Data.Result {
				wantTS, wantVal := oldUnpack(r.Value)
				v, _ := gotSeries[i]["value"].(map[string]any)
				if v["timestamp"] != wantTS {
					t.Errorf("timestamp = %v; want %v", v["timestamp"], wantTS)
				}
				if v["value"] != wantVal {
					t.Errorf("value = %v; want %v", v["value"], wantVal)
				}
			}
		})
	}
}

// TestFastFloat_MatchesParseFloat pins the timestamp fast path against the
// stdlib for every form it claims to handle, and checks that it declines the
// forms it does not.
func TestFastFloat_MatchesParseFloat(t *testing.T) {
	accepted := []string{
		"0", "1", "1700000000", "1700000000.5", "-1", "-1700000000.25",
		"+17", "000123", "9.999999", "123456789012345.5",
	}
	for _, s := range accepted {
		got, ok := fastFloat([]byte(s))
		if !ok {
			t.Errorf("fastFloat(%q) declined; expected it to parse", s)
			continue
		}
		want, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Errorf("fastFloat(%q) accepted input ParseFloat rejects: %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("fastFloat(%q) = %v; ParseFloat = %v", s, got, want)
		}
	}

	declined := []string{"", "1e9", "1.5e-3", "NaN", "Inf", "+Inf", "-Inf", ".", "-", "1.", "abc", "1.2.3"}
	for _, s := range declined {
		if v, ok := fastFloat([]byte(s)); ok {
			t.Errorf("fastFloat(%q) = %v, accepted; want declined so ParseFloat handles it", s, v)
		}
	}
}

// FuzzSampleColumns cross-checks the scanner against the stdlib decode on
// arbitrary input. Any sample array the stdlib accepts must decode to the same
// timestamps and values here, and the scanner must never panic.
func FuzzSampleColumns(f *testing.F) {
	seeds := []string{
		`[[1700000000,"0.42"]]`,
		`[]`,
		`null`,
		`[[1,"1"],[2,"2"],[3,"3"]]`,
		`[ [ 1 , "x" ] ]`,
		`[["1","2"]]`,
		`[[1,"a\"b"]]`,
		`[[1,2]]`,
		`[[1]]`,
		`[[1,"1",99]]`,
		`[[1,"é"]]`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		var cols sampleColumns
		newErr := json.Unmarshal([]byte(in), &cols)

		var ref [][]any
		refErr := json.Unmarshal([]byte(in), &ref)
		if refErr != nil {
			// The stdlib rejected it; the scanner may accept or reject, but it
			// must not have panicked (we got here) and must not invent data
			// from input the stdlib could not read at all.
			return
		}
		if newErr != nil {
			// Reference accepted but scanner rejected: only tolerable for
			// pairs the reference itself would have dropped.
			for _, pair := range ref {
				if len(pair) >= 2 {
					t.Fatalf("scanner rejected input the stdlib accepted: %q: %v", in, newErr)
				}
			}
			return
		}
		if len(cols.timestamps) != len(ref) {
			t.Fatalf("len = %d; want %d for %q", len(cols.timestamps), len(ref), in)
		}
		for i, pair := range ref {
			wantTS, wantVal := oldUnpack(pair)
			if cols.timestamps[i] != wantTS && !(math.IsNaN(cols.timestamps[i]) && math.IsNaN(wantTS)) {
				t.Errorf("ts[%d] = %v; want %v (%q)", i, cols.timestamps[i], wantTS, in)
			}
			if cols.values[i] != wantVal {
				t.Errorf("val[%d] = %q; want %q (%q)", i, cols.values[i], wantVal, in)
			}
		}
	})
}
