package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate_LightAction(t *testing.T) {
	v := &Validator{LightActions: map[string]struct{}{"query_data": {}}}

	if err := v.Validate(&Request{ActionName: "query_data"}); err != nil {
		t.Errorf("Validate(allowlisted): %v", err)
	}
	if err := v.Validate(&Request{ActionName: "delete_pod"}); err == nil {
		t.Errorf("Validate(not allowlisted): expected error")
	}
}

func TestValidate_LightAction_DisabledWhenAllowlistNil(t *testing.T) {
	v := &Validator{}
	if err := v.Validate(&Request{ActionName: "anything"}); err == nil {
		t.Error("expected error when light-action allowlist not configured")
	}
}

// The removed HMAC and partial-key fields are ignored: a request that carries
// only them is judged on the light-action allowlist alone.
func TestValidate_LegacySignatureFieldsGrantNothing(t *testing.T) {
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}}
	r := &Request{
		ActionName: "delete_workload",
		Body: map[string]any{
			"action_name": "delete_workload",
			"signature":   "v0=deadbeef",
		},
	}
	if err := v.Validate(r); err == nil {
		t.Fatal("a mutation with only a legacy signature field must be refused")
	}
}

// A rule narrows SignedOnly to some requests: the rest stay light actions.
func TestValidate_SignedRuleNarrowsToWrites(t *testing.T) {
	isWrite := func(p map[string]any) bool { return p["mode"] == "write" }
	v := &Validator{
		LightActions: map[string]struct{}{"run": {}},
		SignedOnly:   map[string]SignedRule{"run": isWrite},
	}
	read := &Request{ActionName: "run", Body: map[string]any{"action_params": map[string]any{"mode": "read"}}}
	if err := v.Validate(read); err != nil {
		t.Fatalf("read through a rule-gated action should stay a light action: %v", err)
	}
	write := &Request{ActionName: "run", Body: map[string]any{"action_params": map[string]any{"mode": "write"}}}
	if err := v.Validate(write); err == nil {
		t.Fatal("write through a rule-gated action needs a signature")
	}
	// No action_params: the rule sees nil and decides.
	if err := v.Validate(&Request{ActionName: "run"}); err != nil {
		t.Fatalf("rule returned false for nil params, request should pass: %v", err)
	}
}

// The refusal says why the relay signature did not count, so a keyed agent
// whose writes fail points at the cause (no key, no signature, bad signature).
func TestValidate_SignedOnlyRefusalNamesTheReason(t *testing.T) {
	v := &Validator{
		LightActions: map[string]struct{}{"schedule_k8s_job": {}},
		SignedOnly:   map[string]SignedRule{"schedule_k8s_job": nil},
	}
	err := v.Validate(&Request{ActionName: "schedule_k8s_job"})
	if !errors.Is(err, errRelayKeyNotConfigured) {
		t.Fatalf("err = %v; want it to wrap errRelayKeyNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "requires a valid relay signature") {
		t.Fatalf("err = %v; want it to say a relay signature is required", err)
	}
}
