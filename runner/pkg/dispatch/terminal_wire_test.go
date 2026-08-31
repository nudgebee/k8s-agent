package dispatch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/podshell"
)

// fieldSignatures describes a struct's fields as "Name type json-tag" strings.
// Name and type are included alongside the tag because the tag alone is not
// enough: two fields can share a json tag under different Go names (the
// adapter's field-by-field copy then fails to compile, or worse, silently
// copies the wrong one), and a type change on one side would break the copy
// rather than the wire.
func fieldSignatures(t reflect.Type) []string {
	sigs := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		sigs = append(sigs, fmt.Sprintf("%s %s `json:%q`", f.Name, f.Type, f.Tag.Get("json")))
	}
	sort.Strings(sigs)
	return sigs
}

// TestTerminalRequestMatchesPodshellRequest — TerminalRequest duplicates
// podshell.Request's wire shape so pkg/dispatch need not import pkg/podshell.
// That duplication is a silent-drop hazard: handleTerminal unmarshals the
// inbound message into *this* struct, so a field that exists on
// podshell.Request but not here is discarded before the handler ever sees it,
// with no error anywhere.
//
// That is exactly how `container` was lost after #581 added it to
// podshell.Request alone — the pod-shell container picker appeared to work
// (the response path is untyped `any`, so `containers` came back fine) while
// every session silently attached to the default container.
//
// Keep the two shapes identical. If they must legitimately diverge, narrow this
// assertion deliberately rather than deleting it.
func TestTerminalRequestMatchesPodshellRequest(t *testing.T) {
	got := fieldSignatures(reflect.TypeOf(TerminalRequest{}))
	want := fieldSignatures(reflect.TypeOf(podshell.Request{}))

	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire shapes have drifted:\n  dispatch.TerminalRequest:\n    %s\n  podshell.Request:\n    %s\nA field present on one but not the other is dropped in transit; a name, type or tag mismatch breaks the copy.",
			strings.Join(got, "\n    "), strings.Join(want, "\n    "))
	}
}

// TestTerminalRequestDecodesContainer — the field that actually broke: prove an
// inbound start payload carrying `container` survives the unmarshal
// handleTerminal performs.
func TestTerminalRequestDecodesContainer(t *testing.T) {
	msg := []byte(`{"action":"start","name":"app-dev-7c55ff74db-2ntj7","namespace":"nudgebee","container":"nginx","request_id":"r1"}`)

	var req TerminalRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Container != "nginx" {
		t.Errorf("Container = %q; want %q — the caller's container choice was dropped in transit", req.Container, "nginx")
	}
}
