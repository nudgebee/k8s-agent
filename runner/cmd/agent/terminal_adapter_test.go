package main

import (
	"reflect"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/podshell"
)

// TestToPodshellRequestCopiesEveryField — the conversion is a hand-written
// field-by-field copy between two structs that duplicate one wire shape, so a
// field left off the list is dropped with no compiler or linter complaint. That
// is precisely how the pod-shell container choice was lost after #581.
//
// Populate every string field with a distinct value via reflection, convert,
// and assert each one arrived. Adding a field to either struct without wiring
// it through fails here rather than in production.
func TestToPodshellRequestCopiesEveryField(t *testing.T) {
	src := &dispatch.TerminalRequest{}
	v := reflect.ValueOf(src).Elem()
	typ := v.Type()

	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() != reflect.String {
			t.Fatalf("field %s is %s; this test only knows how to populate strings", typ.Field(i).Name, v.Field(i).Kind())
		}
		v.Field(i).SetString("value-of-" + typ.Field(i).Name)
	}

	got := reflect.ValueOf(toPodshellRequest(src)).Elem()

	for i := 0; i < v.NumField(); i++ {
		name := typ.Field(i).Name
		dst := got.FieldByName(name)
		if !dst.IsValid() {
			t.Errorf("podshell.Request has no field %s; the two wire structs have drifted", name)
			continue
		}
		if want := "value-of-" + name; dst.String() != want {
			t.Errorf("%s = %q; want %q — this field is dropped in transit", name, dst.String(), want)
		}
	}

	// Guard the other direction too: a field added to podshell.Request alone
	// would leave nothing to copy into it.
	if a, b := got.NumField(), v.NumField(); a != b {
		t.Errorf("podshell.Request has %d fields, dispatch.TerminalRequest has %d; keep the wire shapes identical", a, b)
	}

	// Cheap sanity check on the field that actually broke.
	if p := toPodshellRequest(&dispatch.TerminalRequest{Action: "start", Container: "nginx"}); p.Container != "nginx" {
		t.Errorf("Container = %q; want %q", p.Container, "nginx")
	}
}

var _ = podshell.Request{}
