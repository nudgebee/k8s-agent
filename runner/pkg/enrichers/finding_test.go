package enrichers

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// decodeBase64 wraps stdlib for readability in the FileBlock test.
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// TestFindingResponse_WireShape checks every layer of the response —
// outer success/findings, inner evidence with stringified-JSON data,
// and that the structured_data array round-trips through JSON.
//
// This is the contract that api-server's relay.ExecuteAndExtractResponse walks
// (services/relay/service.go:203-303). If any field shifts, that walker breaks.
func TestFindingResponse_WireShape(t *testing.T) {
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	resp, err := FindingResponse("acc-1", id,
		PrometheusBlock(map[string]any{"result_type": "vector"}, "up"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp["success"] != true {
		t.Errorf("success = %v; want true", resp["success"])
	}
	findings := resp["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("findings len = %d; want 1", len(findings))
	}
	finding := findings[0].(map[string]any)
	if finding["id"] != id.String() {
		t.Errorf("finding.id = %v; want %s", finding["id"], id)
	}
	evidence := finding["evidence"].([]any)
	if len(evidence) != 1 {
		t.Fatalf("evidence len = %d; want 1", len(evidence))
	}
	ev := evidence[0].(map[string]any)
	if ev["account_id"] != "acc-1" {
		t.Errorf("evidence.account_id = %v; want acc-1", ev["account_id"])
	}
	if ev["file_type"] != "structured_data" {
		t.Errorf("evidence.file_type = %v; want structured_data", ev["file_type"])
	}
	dataStr, ok := ev["data"].(string)
	if !ok {
		t.Fatalf("evidence.data not a string: %T", ev["data"])
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(dataStr), &blocks); err != nil {
		t.Fatalf("evidence.data not a JSON array: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "prometheus" {
		t.Errorf("blocks = %+v; want one prometheus block", blocks)
	}
}

// TestFindingResponse_GeneratesUUIDWhenNil verifies the convenience path
// where callers pass uuid.Nil and we mint one. A fresh UUID per Finding
// matches the legacy behaviour.
func TestFindingResponse_GeneratesUUIDWhenNil(t *testing.T) {
	resp, err := FindingResponse("acc-x", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	finding := resp["findings"].([]any)[0].(map[string]any)
	if finding["id"] == "" || finding["id"] == uuid.Nil.String() {
		t.Errorf("expected fresh UUID; got %v", finding["id"])
	}
}

func TestJSONBlock_StringifiesData(t *testing.T) {
	block, err := JSONBlock(map[string]any{"A": map[string]any{"result_type": "vector"}})
	if err != nil {
		t.Fatal(err)
	}
	if block["type"] != "json" {
		t.Errorf("type = %v; want json", block["type"])
	}
	s, ok := block["data"].(string)
	if !ok {
		t.Fatalf("data not a string: %T", block["data"])
	}
	if !strings.Contains(s, `"result_type":"vector"`) {
		t.Errorf("inner JSON missing result_type: %s", s)
	}
}

// TestFileBlock_DetectsExtension covers the small filename-extension parser.
// The "type" field is what api-server / UI uses to dispatch rendering.
func TestFileBlock_DetectsExtension(t *testing.T) {
	cases := map[string]string{
		"frontend.log":     "log",
		"trace.json":       "json",
		"no-extension":     "",
		"file.with.dots.x": "x",
	}
	for filename, want := range cases {
		got := FileBlock(filename, []byte("x"))["type"]
		if got != want {
			t.Errorf("FileBlock(%q).type = %v; want %q", filename, got, want)
		}
	}
}

// TestFileBlock_WireShape locks the Python-quirk wire format that UI
// consumers (KubernetesPodYaml.tsx, KubernetesPodLogs.tsx, KubernetesWorkloads.jsx)
// depend on: data is base64-encoded and wrapped with a literal `b'...'` so the
// UI's `atob(d.data.slice(2, -1))` is correct. See the backend.
func TestFileBlock_WireShape(t *testing.T) {
	payload := []byte("hello world")
	block := FileBlock("x.yaml", payload)
	if block["filename"] != "x.yaml" || block["type"] != "yaml" {
		t.Fatalf("filename/type wrong: %+v", block)
	}
	wrapped, _ := block["data"].(string)
	if !strings.HasPrefix(wrapped, "b'") || !strings.HasSuffix(wrapped, "'") {
		t.Fatalf("data must be wrapped as b'<base64>': got %q", wrapped)
	}
	// Mimic the UI's strip + decode.
	b64 := wrapped[2 : len(wrapped)-1]
	decoded, err := decodeBase64(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != "hello world" {
		t.Errorf("decoded = %q; want hello world", decoded)
	}
}

func TestMarkdownBlock_Shape(t *testing.T) {
	b := MarkdownBlock("# header\nbody")
	if b["type"] != "markdown" {
		t.Errorf("type = %v; want markdown", b["type"])
	}
	if b["data"] != "# header\nbody" {
		t.Errorf("data = %v; want passthrough", b["data"])
	}
}

func TestErrorResponse_Shape(t *testing.T) {
	r := ErrorResponse("nope", 404)
	if r["success"] != false || r["msg"] != "nope" || r["error_code"] != 404 {
		t.Errorf("ErrorResponse = %+v", r)
	}
}
