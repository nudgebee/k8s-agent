package cmdout

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

func gunzipB64(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

func TestPlain_KeepsTextUnderStdout(t *testing.T) {
	o := New("")
	_, _ = o.Write([]byte("hello "))
	_, _ = o.Write([]byte("world"))
	m := map[string]any{}
	if err := o.AddTo(m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"stdout": "hello world"}
	if len(m) != 1 || m["stdout"] != want["stdout"] {
		t.Fatalf("fields = %v; want %v", m, want)
	}
	if o.Encoding() != "" || o.Len() != 11 {
		t.Errorf("Encoding=%q Len=%d; want \"\" 11", o.Encoding(), o.Len())
	}
}

func TestGzip_RoundTripsAcrossWrites(t *testing.T) {
	o := New(Gzip)
	parts := []string{`{"status":"success","data":[`, strings.Repeat(`{"__name__":"up","le":"0.5"},`, 10000), `{}]}`}
	for _, p := range parts {
		if _, err := o.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]any{}
	if err := o.AddTo(m); err != nil {
		t.Fatal(err)
	}
	want := strings.Join(parts, "")
	if _, ok := m["stdout"]; ok {
		t.Error("gzip mode must not also set stdout")
	}
	if m[Param] != Gzip {
		t.Errorf("%s = %v; want %q", Param, m[Param], Gzip)
	}
	if m["stdout_bytes"] != int64(len(want)) {
		t.Errorf("stdout_bytes = %v; want %d", m["stdout_bytes"], len(want))
	}
	enc, _ := m["stdout_gzip_b64"].(string)
	if got := gunzipB64(t, enc); got != want {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if len(enc) >= len(want)/10 {
		t.Errorf("encoded %d bytes for %d of repetitive input; compression not applied?", len(enc), len(want))
	}
}

func TestGzip_EmptyOutputStillDecodes(t *testing.T) {
	enc, err := New(Gzip).GzipBase64()
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzipB64(t, enc); got != "" {
		t.Fatalf("got %q; want empty", got)
	}
}

func TestGzipBase64_IsRepeatable(t *testing.T) {
	o := New(Gzip)
	_, _ = o.Write([]byte("abc"))
	a, err := o.GzipBase64()
	if err != nil {
		t.Fatal(err)
	}
	b, err := o.GzipBase64()
	if err != nil || a != b {
		t.Fatalf("second call = %q, %v; want %q, nil", b, err, a)
	}
}

// An agent that predates output_encoding ignores the param and returns text;
// an encoding this agent does not know must behave the same way, so a caller
// that reads the response's output_encoding works with both.
func TestUnknownEncoding_FallsBackToPlain(t *testing.T) {
	o := New("zstd")
	_, _ = o.Write([]byte("x"))
	m := map[string]any{}
	if err := o.AddTo(m); err != nil {
		t.Fatal(err)
	}
	if m["stdout"] != "x" || m[Param] != nil {
		t.Fatalf("fields = %v; want plain stdout and no %s", m, Param)
	}
}
