package enrichers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// PrometheusQueryResultDict converts a raw Prometheus query response (the
// `data` field returned by `/api/v1/query` / `query_range`) into the
// PrometheusQueryResult wire shape consumed by downstream services.
//
// The shape is:
//
//	{
//	  "result_type": "vector"|"matrix"|"scalar"|"string"|"error",
//	  "vector_result":      [{metric: {..}, value: {timestamp, value}}] | null,
//	  "series_list_result": [{metric: {..}, timestamps: [..], values: [..]}] | null,
//	  "scalar_result":      {timestamp, value} | null,
//	  "string_result":      "..." | null
//	}
//
// Only one of the four *_result fields is populated; the others are explicit
// nulls (json: omitempty would drop them, but consumers expect nulls).
// prometheusResponseEnvelope mirrors the top-level shape Prometheus emits at
// /api/v1/query and /api/v1/query_range. Shared between
// PrometheusQueryResultDict and runOneInstantRaw so the two stay aligned.
type prometheusResponseEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// sampleColumns holds a Prometheus sample array — `[[<ts>,"<val>"], …]` —
// already split into the parallel typed slices the wire shape wants.
//
// Decoding straight into typed columns is the whole point. The obvious
// `[][]any` decode boxes every sample into a 2-element []any (slice header +
// two interfaces + a heap-boxed float + a string header), costing ~110 bytes
// for a ~28-byte sample, and then matrixToWire had to build the parallel
// arrays *alongside* the still-live []any tree. On a real
// prometheus_queries_enricher burst that was the difference between ~2x and
// ~7.5x live heap per wire byte.
type sampleColumns struct {
	timestamps []float64
	values     []string
}

var errBadSample = errors.New("prometheus sample: want [<timestamp>, <value>]")

// errComplexSample means a sample member was an object or array. Prometheus
// cannot emit that, but rather than reimplement fmt's rendering of an
// arbitrary decoded value, the scanner bails and the whole array is re-read
// through encoding/json + unpackScalar — the exact code this replaced.
var errComplexSample = errors.New("prometheus sample: non-scalar member")

// decodeSamplesStdlib is the pre-rewrite decode, kept as the fallback for
// inputs the scanner declines.
func decodeSamplesStdlib(b []byte) ([]float64, []string, error) {
	var pairs [][]any
	if err := json.Unmarshal(b, &pairs); err != nil {
		return nil, nil, err
	}
	timestamps := make([]float64, 0, len(pairs))
	values := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ts, val := unpackScalar(p)
		timestamps = append(timestamps, ts)
		values = append(values, val)
	}
	return timestamps, values, nil
}

// UnmarshalJSON scans the sample array directly out of the response bytes.
//
// A json.Decoder would be the idiomatic choice, but Token() returns `any` and
// so heap-boxes a float per sample plus the decoder's own buffering — on a
// 2.9M-sample response that tripled GC churn versus scanning in place. The
// grammar here is fixed and tiny (`[[<num|str>,<num|str>], …]`), every exotic
// case falls back to encoding/json, and promResultFuzz cross-checks this
// against the stdlib decode.
func (c *sampleColumns) UnmarshalJSON(b []byte) error {
	s := sampleScanner{b: b}
	s.ws()
	if s.atNull() {
		// `values: null` is legal and means "no samples".
		return nil
	}
	if !s.take('[') {
		return errBadSample
	}
	// Pre-size exactly. Prometheus emits compact JSON, so counting the "],["
	// separators gives the sample count in one SIMD-backed pass — far cheaper
	// than the alternative: a bytes-per-sample estimate that undershoots by a
	// few percent makes append double both slices, allocating twice what is
	// needed and copying the whole thing. Whitespace-padded input counts low
	// and simply falls back to append's growth.
	if est := bytes.Count(b, []byte("],[")) + 1; est > 0 {
		c.timestamps = make([]float64, 0, est)
		c.values = make([]string, 0, est)
	}
	for {
		s.ws()
		if s.take(']') {
			return nil
		}
		ts, val, err := s.pair()
		if errors.Is(err, errComplexSample) {
			c.timestamps, c.values, err = decodeSamplesStdlib(b)
			return err
		}
		if err != nil {
			return err
		}
		c.timestamps = append(c.timestamps, ts)
		c.values = append(c.values, val)
		s.ws()
		if s.take(',') {
			continue
		}
		if s.take(']') {
			return nil
		}
		return errBadSample
	}
}

// promSample is the single `[<ts>,"<val>"]` pair an instant vector carries.
type promSample struct {
	ts  float64
	val string
}

func (s *promSample) UnmarshalJSON(b []byte) error {
	sc := sampleScanner{b: b}
	sc.ws()
	if sc.atNull() {
		return nil
	}
	ts, val, err := sc.pair()
	if errors.Is(err, errComplexSample) {
		var pair []any
		if err := json.Unmarshal(b, &pair); err != nil {
			return err
		}
		s.ts, s.val = unpackScalar(pair)
		return nil
	}
	if err != nil {
		// A malformed value is not fatal — the legacy fmt.Sprintf path
		// returned the zero sample rather than failing the whole query.
		if errors.Is(err, errBadSample) {
			return nil
		}
		return err
	}
	s.ts, s.val = ts, val
	return nil
}

// sampleScanner is a cursor over raw JSON bytes.
type sampleScanner struct {
	b []byte
	i int
}

func (s *sampleScanner) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

func (s *sampleScanner) take(c byte) bool {
	if s.i < len(s.b) && s.b[s.i] == c {
		s.i++
		return true
	}
	return false
}

func (s *sampleScanner) atNull() bool {
	return bytes.HasPrefix(s.b[s.i:], []byte("null"))
}

// pair reads one `[<ts>, <val>]`. Both members are accepted as either a JSON
// number or a JSON string: Prometheus sends `[<number>, "<string>"]`, but some
// compatible backends quote the timestamp, and the previous
// fmt.Sprintf("%v", …) path tolerated a bare number for the value.
func (s *sampleScanner) pair() (float64, string, error) {
	s.ws()
	if !s.take('[') {
		return 0, "", errBadSample
	}
	var toks [2]rawToken
	n := 0
	for {
		s.ws()
		if s.take(']') {
			break
		}
		if n > 0 && !s.take(',') {
			return 0, "", errBadSample
		}
		s.ws()
		t, err := s.token()
		if err != nil {
			return 0, "", err
		}
		if n < 2 {
			toks[n] = t
		}
		// Members past the second are not part of the contract; drained above.
		n++
	}
	if n < 2 {
		// unpackScalar's `len(raw) < 2` guard: a short pair is kept as a
		// zero sample rather than dropped or treated as an error.
		return 0, "", nil
	}
	return toks[0].float(), toks[1].string(), nil
}

// rawToken is a not-yet-converted scalar: a subslice of the response bytes
// plus whether it was quoted. Holding a subslice means numbers cost nothing
// until (and unless) they are converted.
type rawToken struct {
	raw    []byte
	quoted bool
	esc    bool // quoted and contains a backslash — needs the stdlib unquoter
}

// slow reports whether this token has to go through encoding/json rather than
// being taken verbatim: either it carries escape sequences, or it is not valid
// UTF-8 (the stdlib substitutes U+FFFD for invalid bytes, and we must match
// that byte for byte). Prometheus values are ASCII numerals, so neither fires
// in practice — but a compatible backend returning a label-derived string
// could, and FuzzSampleColumns holds us to the stdlib's exact behaviour.
func (t rawToken) slow() bool {
	return t.esc || (t.quoted && !utf8.Valid(t.raw))
}

func (t rawToken) float() float64 {
	if t.slow() {
		var s string
		if json.Unmarshal(t.full(), &s) == nil {
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		return 0
	}
	if f, ok := fastFloat(t.raw); ok {
		return f
	}
	f, _ := strconv.ParseFloat(string(t.raw), 64)
	return f
}

func (t rawToken) string() string {
	if t.slow() {
		var s string
		if json.Unmarshal(t.full(), &s) == nil {
			return s
		}
		return ""
	}
	if t.quoted {
		return string(t.raw)
	}
	// Unquoted members reach here only from a non-conforming backend —
	// Prometheus always quotes sample values. The old path decoded them into
	// `any` and ran fmt.Sprintf("%v", …), which reformats floats via %g
	// ("1000000" becomes "1e+06") and renders null as "<nil>". Reproduce that
	// exactly rather than passing the wire spelling through.
	switch string(t.raw) {
	case "true", "false":
		return string(t.raw)
	case "null":
		return "<nil>"
	}
	f, err := strconv.ParseFloat(string(t.raw), 64)
	if err != nil {
		return string(t.raw)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// full re-wraps a quoted token in its quotes for the stdlib unquoter.
func (t rawToken) full() []byte {
	out := make([]byte, 0, len(t.raw)+2)
	out = append(out, '"')
	out = append(out, t.raw...)
	return append(out, '"')
}

// token reads one scalar (string, number, or literal) and returns it
// unconverted.
func (s *sampleScanner) token() (rawToken, error) {
	if s.i >= len(s.b) {
		return rawToken{}, errBadSample
	}
	if s.b[s.i] == '[' || s.b[s.i] == '{' {
		return rawToken{}, errComplexSample
	}
	if s.b[s.i] == '"' {
		s.i++
		start := s.i
		esc := false
		for s.i < len(s.b) {
			c := s.b[s.i]
			if c == '\\' {
				esc = true
				// Skip the escaped byte, but never past the end: a trailing
				// backslash would otherwise leave s.i == len(s.b)+1 and break
				// the scanner's `s.i <= len(s.b)` invariant. Every read today
				// is length-guarded so this cannot currently be observed, but
				// atNull slices s.b[s.i:] and would panic if the invariant
				// were ever relied on after this point.
				if s.i+1 < len(s.b) {
					s.i += 2
				} else {
					s.i = len(s.b)
				}
				continue
			}
			if c == '"' {
				raw := s.b[start:s.i]
				s.i++
				return rawToken{raw: raw, quoted: true, esc: esc}, nil
			}
			s.i++
		}
		return rawToken{}, errBadSample
	}
	start := s.i
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ',', ']', '}', ' ', '\t', '\n', '\r':
			if s.i == start {
				return rawToken{}, errBadSample
			}
			return rawToken{raw: s.b[start:s.i]}, nil
		default:
			s.i++
		}
	}
	if s.i == start {
		return rawToken{}, errBadSample
	}
	return rawToken{raw: s.b[start:s.i]}, nil
}

// fastFloat parses the plain decimal forms Prometheus timestamps actually take
// (1700000000, 1700000000.5) without the string allocation ParseFloat needs.
// Anything else — exponents, Inf, NaN, huge magnitudes — returns false and
// takes the ParseFloat path.
func fastFloat(b []byte) (float64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	i := 0
	neg := false
	if b[0] == '-' || b[0] == '+' {
		neg = b[0] == '-'
		i++
		if i == len(b) {
			return 0, false
		}
	}
	var intPart int64
	digits := 0
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		intPart = intPart*10 + int64(b[i]-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	out := float64(intPart)
	if i < len(b) {
		if b[i] != '.' {
			return 0, false
		}
		i++
		var frac int64
		scale := 1.0
		fracDigits := 0
		for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
			frac = frac*10 + int64(b[i]-'0')
			scale *= 10
			fracDigits++
		}
		if i != len(b) || fracDigits == 0 {
			return 0, false
		}
		out += float64(frac) / scale
	}
	if neg {
		out = -out
	}
	return out, true
}

func PrometheusQueryResultDict(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty Prometheus response")
	}
	var top prometheusResponseEnvelope
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if top.Status != "success" {
		return map[string]any{
			"result_type":        "error",
			"vector_result":      nil,
			"series_list_result": nil,
			"scalar_result":      nil,
			"string_result":      top.Error,
		}, nil
	}

	out := map[string]any{
		"result_type":        top.Data.ResultType,
		"vector_result":      nil,
		"series_list_result": nil,
		"scalar_result":      nil,
		"string_result":      nil,
	}

	switch top.Data.ResultType {
	case "vector":
		var raws []vectorSeries
		if err := json.Unmarshal(top.Data.Result, &raws); err != nil {
			return nil, fmt.Errorf("decode vector result: %w", err)
		}
		out["vector_result"] = vectorToWire(raws)
	case "matrix":
		var raws []matrixSeries
		if err := json.Unmarshal(top.Data.Result, &raws); err != nil {
			return nil, fmt.Errorf("decode matrix result: %w", err)
		}
		out["series_list_result"] = matrixToWire(raws)
	case "scalar":
		var raw []any
		if err := json.Unmarshal(top.Data.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode scalar result: %w", err)
		}
		out["scalar_result"] = scalarToWire(raw)
	case "string":
		var raw any
		_ = json.Unmarshal(top.Data.Result, &raw)
		out["string_result"] = fmt.Sprintf("%v", raw)
	default:
		return nil, fmt.Errorf("unknown result_type %q", top.Data.ResultType)
	}
	return out, nil
}

// vectorSeries / matrixSeries are the two per-series shapes Prometheus
// returns under data.result.
type vectorSeries struct {
	Metric map[string]string `json:"metric"`
	Value  promSample        `json:"value"`
}

type matrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values sampleColumns     `json:"values"`
}

// vectorToWire turns Prometheus instant samples into the
// `[{metric, value: {timestamp, value}}]` wire shape. value is always a
// string, timestamp always a float.
func vectorToWire(raws []vectorSeries) []map[string]any {
	out := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		out = append(out, map[string]any{
			"metric": r.Metric,
			"value":  map[string]any{"timestamp": r.Value.ts, "value": r.Value.val},
		})
	}
	return out
}

// matrixToWire turns Prometheus range samples into the
// `[{metric, timestamps: [..floats..], values: [..strings..]}]` wire shape.
// The columns were already split during decode, so this just hands over the
// slices — no second copy of the sample data.
func matrixToWire(raws []matrixSeries) []map[string]any {
	out := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		timestamps := r.Values.timestamps
		values := r.Values.values
		if timestamps == nil {
			timestamps = []float64{}
		}
		if values == nil {
			values = []string{}
		}
		out = append(out, map[string]any{
			"metric":     r.Metric,
			"timestamps": timestamps,
			"values":     values,
		})
	}
	return out
}

func scalarToWire(raw []any) map[string]any {
	ts, val := unpackScalar(raw)
	return map[string]any{"timestamp": ts, "value": val}
}

// unpackScalar coerces the 2-element [ts, value] Prometheus sample to
// (float64, string).
func unpackScalar(raw []any) (float64, string) {
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
