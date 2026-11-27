package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	"github.com/nudgebee/nudgebee-agent/pkg/podrunner"
	"github.com/nudgebee/nudgebee-agent/pkg/relaysig"
)

func TestIsWriteHTTPMethod(t *testing.T) {
	for method, want := range map[string]bool{
		"": false, "GET": false, "HEAD": false,
		"get": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true,
	} {
		if got := isWriteHTTPMethod(map[string]any{"method": method}); got != want {
			t.Errorf("method %q: got %v, want %v", method, got, want)
		}
	}
	if isWriteHTTPMethod(nil) {
		t.Error("no params defaults to GET, not a write")
	}
}

func TestIsWriteKubectlCommand(t *testing.T) {
	for command, want := range map[string]bool{
		"get pods -n default":           false,
		"kubectl describe deploy/web":   false,
		"logs pod/web --tail=10":        false,
		"rollout history deploy/web":    false,
		"delete pod web":                true,
		"scale deploy/web --replicas=0": true,
		"rollout restart deploy/web":    true,
		"get pods && delete pod web":    true,
		"apply -f manifest.yaml":        true,
		"":                              true, // unparseable → not read-only
		"get pods | grep web":           true,
		"get secret db -o yaml":         true, // refused by the executor in every mode
	} {
		if got := isWriteKubectlCommand(map[string]any{"command": command}); got != want {
			t.Errorf("command %q: got %v, want %v", command, got, want)
		}
	}
}

// recommend_only lives under action_params.settings, as the handler reads it.
func TestIsRightsizingApply(t *testing.T) {
	recommend := map[string]any{"settings": map[string]any{"recommend_only": true}, "applications": []any{}}
	if isRightsizingApply(recommend) {
		t.Error("settings.recommend_only=true patches nothing")
	}
	for _, p := range []map[string]any{
		nil,
		{"settings": map[string]any{"recommend_only": false}},
		{"settings": map[string]any{}},
		{"settings": map[string]any{"recommend_only": "true"}}, // handler reads a bool only
		{"recommend_only": true},                               // top level is not where the handler looks
	} {
		if !isRightsizingApply(p) {
			t.Errorf("params %v may patch, so it is a write", p)
		}
	}
}

// The rules see action_params from a real request body through the validator:
// on a keyed agent an unsigned recommend_only run and a kubectl read pass, and
// the patching run and a kubectl write are refused.
func TestWriteRulesThroughValidator(t *testing.T) {
	ver, err := relaysig.NewVerifier(base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	v := &auth.Validator{
		LightActions:  map[string]struct{}{"continuous_rightsizing": {}, "kubectl_command_executor": {}},
		RelayVerifier: ver,
		SignedOnly: map[string]auth.SignedRule{
			"continuous_rightsizing":   isRightsizingApply,
			"kubectl_command_executor": isWriteKubectlCommand,
		},
	}
	req := func(body string) *auth.Request {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		name, _ := m["action_name"].(string)
		return &auth.Request{Body: m, ActionName: name, BodyRaw: []byte(body)}
	}
	for body, wantOK := range map[string]bool{
		`{"action_name":"continuous_rightsizing","action_params":{"settings":{"recommend_only":true},"applications":[]}}`:  true,
		`{"action_name":"continuous_rightsizing","action_params":{"settings":{"recommend_only":false},"applications":[]}}`: false,
		`{"action_name":"kubectl_command_executor","action_params":{"command":"get pods -A"}}`:                             true,
		`{"action_name":"kubectl_command_executor","action_params":{"command":"delete pod web"}}`:                          false,
	} {
		if err := v.Validate(req(body)); (err == nil) != wantOK {
			t.Errorf("%s: err = %v, want ok=%v", body, err, wantOK)
		}
	}
}

// A script request for <release>-script-write needs a signature even on an agent where
// pod_script_run_enricher is otherwise a light action; read and no-token
// requests stay light.
func TestScriptWriteAccessNeedsSignature(t *testing.T) {
	v := &auth.Validator{
		LightActions: map[string]struct{}{"pod_script_run_enricher": {}},
		SignedOnly:   map[string]auth.SignedRule{"pod_script_run_enricher": podrunner.IsWriteRequest},
	}
	req := func(access string) *auth.Request {
		params := map[string]any{"image": "busybox", "command": "true"}
		if access != "" {
			params["cluster_access"] = access
		}
		return &auth.Request{ActionName: "pod_script_run_enricher", Body: map[string]any{"action_params": params}}
	}
	for _, access := range []string{"", "read", "none"} {
		if err := v.Validate(req(access)); err != nil {
			t.Errorf("cluster_access=%q unsigned: %v; want accepted", access, err)
		}
	}
	for _, access := range []string{"write", "Write"} {
		if err := v.Validate(req(access)); err == nil {
			t.Errorf("cluster_access=%q unsigned: accepted; want refused", access)
		}
	}
}

// identity=write on a read command still needs a signature.
func TestKubectlWriteIdentityHintNeedsSignature(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   bool
	}{
		{map[string]any{"command": "kubectl get pods"}, false},
		{map[string]any{"command": "kubectl get pods", "identity": "read"}, false},
		{map[string]any{"command": "kubectl get pods", "identity": "write"}, true},
		{map[string]any{"command": "kubectl delete pod x", "identity": "read"}, true},
	} {
		if got := isWriteKubectlCommand(tc.params); got != tc.want {
			t.Errorf("isWriteKubectlCommand(%v) = %v, want %v", tc.params, got, tc.want)
		}
	}
}
