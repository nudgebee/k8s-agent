package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/cmdout"
)

// printingKubectl writes a stand-in kubectl that prints its args, then the
// given number of bytes of a Prometheus-series-like line, to stdout.
func printingKubectl(t *testing.T, n int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kubectl.sh")
	script := fmt.Sprintf("#!/bin/sh\necho \"args: $*\"\nyes '{\"__name__\":\"k6_http_req_duration_bucket\",\"namespace\":\"demo\",\"le\":\"0.5\"},' | head -c %d\n", n)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func decodeGzipStdout(t *testing.T, out map[string]any) string {
	t.Helper()
	enc, ok := out["stdout_gzip_b64"].(string)
	if !ok {
		t.Fatalf("stdout_gzip_b64 missing: %v", out)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestKubectl_GzipOutput_MatchesPlainAcrossChainedSegments(t *testing.T) {
	bin := printingKubectl(t, 4096)
	k := &KubectlExecutor{BinaryPath: bin}
	cmd := "kubectl get pods && kubectl get svc"

	plain, err := k.Run(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := k.RunEncoded(context.Background(), cmd, cmdout.Gzip)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := plain[cmdout.Param]; ok {
		t.Errorf("plain response carries %s: %v", cmdout.Param, plain[cmdout.Param])
	}
	want, _ := plain["stdout"].(string)
	if !strings.Contains(want, "args: get pods") || !strings.Contains(want, "args: get svc") {
		t.Fatalf("plain stdout misses a segment: %.80q", want)
	}
	if got := decodeGzipStdout(t, gz); got != want {
		t.Fatalf("gzip stdout differs from plain: %d vs %d bytes", len(got), len(want))
	}
	if gz[cmdout.Param] != cmdout.Gzip || gz["stdout_bytes"] != int64(len(want)) || gz["exit_code"] != 0 {
		t.Errorf("gzip fields = %s:%v stdout_bytes:%v exit_code:%v", cmdout.Param, gz[cmdout.Param], gz["stdout_bytes"], gz["exit_code"])
	}
	if _, ok := gz["stdout"]; ok {
		t.Error("gzip response must not also carry stdout")
	}
}

func TestKubectlHandler_PassesOutputEncoding(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: printingKubectl(t, 64)}
	h := Handlers(nil, k, "acct")["kubectl_command_executor"]
	resp, err := h(context.Background(), map[string]any{"command": "kubectl get pods", cmdout.Param: cmdout.Gzip})
	if err != nil {
		t.Fatal(err)
	}
	// Envelope: findings[0].evidence[0].data → JSON blocks → blocks[0].data → JSON payload.
	ev := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(ev["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload[cmdout.Param] != cmdout.Gzip {
		t.Fatalf("payload = %v; want %s=gzip", payload, cmdout.Param)
	}
}

// Large command output in gzip mode must cost the agent a fraction of its size
// in allocations. Before output_encoding, collecting and encoding output took
// about 10x its size in heap: 484 MB from a Prometheus series query OOM-killed
// the runner at its 2000Mi limit.
func TestKubectlHandler_GzipOutput_AllocatesFractionOfOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 64 MiB through a fake kubectl")
	}
	const size = 64 << 20
	k := &KubectlExecutor{BinaryPath: printingKubectl(t, size)}
	h := Handlers(nil, k, "acct")["kubectl_command_executor"]

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := h(context.Background(), map[string]any{"command": "kubectl get --raw /x", cmdout.Param: cmdout.Gzip})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(resp); err != nil { // as relay.send does
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/4 {
		t.Fatalf("allocated %d MiB for %d MiB of output; want under %d MiB", alloc>>20, size>>20, (size/4)>>20)
	}
}
