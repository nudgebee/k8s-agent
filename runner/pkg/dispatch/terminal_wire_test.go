package dispatch

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/podshell"
)

// jsonFieldNames returns the json tag name of every field on a struct type.
func jsonFieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if comma := len(tag); comma > 0 {
			for j, r := range tag {
				if r == ',' {
					tag = tag[:j]
					break
				}
			}
		}
		names = append(names, tag)
	}
	sort.Strings(names)
	return names
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
	got := jsonFieldNames(reflect.TypeOf(TerminalRequest{}))
	want := jsonFieldNames(reflect.TypeOf(podshell.Request{}))

	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire shapes have drifted:\n  dispatch.TerminalRequest = %v\n  podshell.Request         = %v\nA field present on one but not the other is dropped in transit.", got, want)
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
