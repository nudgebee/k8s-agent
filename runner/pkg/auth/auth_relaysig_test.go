package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/relaysig"
)

// These tests pin the back-compat matrix for the relay-signature path: a valid
// relay signature authorizes any action; an absent one falls through to the
// existing modes; and a present-but-unverifiable one (no key) is IGNORED so
// reads don't 401 on an upgraded-but-unkeyed agent.

func relaysigKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return priv, base64.StdEncoding.EncodeToString(pub)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func signedRelayReq(priv ed25519.PrivateKey, bodyRaw []byte, actionName, nonce string) *Request {
	sig := ed25519.Sign(priv, bodyRaw)
	return &Request{
		ActionName: actionName,
		BodyRaw:    bodyRaw,
		RelaySig: relaysig.Envelope{
			Signature: base64.StdEncoding.EncodeToString(sig),
			SignedAt:  time.Now().UTC().Format(time.RFC3339),
			Nonce:     nonce,
		},
	}
}

// (a) valid relay sig authorizes a NON-lightAction.
func TestValidate_RelaySig_AuthorizesMutation(t *testing.T) {
	priv, pub := relaysigKey(t)
	ver, err := relaysig.NewVerifier(pub, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}, RelayVerifier: ver}

	body := []byte(`{"account_id":"a","action_name":"delete_workload","action_params":{"kind":"deployment"}}`)
	if err := v.Validate(signedRelayReq(priv, body, "delete_workload", "m1")); err != nil {
		t.Fatalf("valid relay sig should authorize delete_workload: %v", err)
	}
}

// (b) absent relay sig → mutation still rejected, read still allowed.
func TestValidate_NoRelaySig_FallsThrough(t *testing.T) {
	_, pub := relaysigKey(t)
	ver, _ := relaysig.NewVerifier(pub, discardLogger())
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}, RelayVerifier: ver}

	if err := v.Validate(&Request{ActionName: "get_resource"}); err != nil {
		t.Errorf("read without relay sig should pass via lightActions: %v", err)
	}
	if err := v.Validate(&Request{ActionName: "delete_workload"}); err == nil {
		t.Error("mutation without relay sig should be rejected")
	}
}

// (c) relay sig present but verifier disabled (no key) → IGNORED, fall through.
// Read still 200, mutation still 401 — crucially NOT a hard 401 on the read.
func TestValidate_RelaySigPresent_VerifierDisabled_Ignored(t *testing.T) {
	priv, _ := relaysigKey(t)
	disabled, _ := relaysig.NewVerifier("", discardLogger()) // no key
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}, RelayVerifier: disabled}

	readBody := []byte(`{"action_name":"get_resource"}`)
	if err := v.Validate(signedRelayReq(priv, readBody, "get_resource", "c1")); err != nil {
		t.Errorf("read with relay sig but no verifier key should still pass via lightActions: %v", err)
	}
	mutBody := []byte(`{"action_name":"delete_workload"}`)
	if err := v.Validate(signedRelayReq(priv, mutBody, "delete_workload", "c2")); err == nil {
		t.Error("mutation with unverifiable relay sig should fall through and be rejected")
	}
}

// Same as (c) but RelayVerifier is nil entirely.
func TestValidate_RelaySigPresent_NoVerifier_Ignored(t *testing.T) {
	priv, _ := relaysigKey(t)
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}}
	readBody := []byte(`{"action_name":"get_resource"}`)
	if err := v.Validate(signedRelayReq(priv, readBody, "get_resource", "n1")); err != nil {
		t.Errorf("read with relay sig and nil verifier should pass via lightActions: %v", err)
	}
}

// (d) relay sig present + verifier enabled but signature invalid → the MUTATION
// falls through to the lightAction check and is rejected there (delete_workload
// isn't a light action). Still a 401, just via fall-through rather than a hard
// signature failure.
func TestValidate_RelaySig_InvalidMutationRejected(t *testing.T) {
	priv, pub := relaysigKey(t)
	ver, _ := relaysig.NewVerifier(pub, discardLogger())
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}, RelayVerifier: ver}

	body := []byte(`{"action_name":"delete_workload"}`)
	req := signedRelayReq(priv, body, "delete_workload", "d1")
	// Corrupt the body after signing → signature no longer matches.
	req.BodyRaw = []byte(`{"action_name":"delete_workload","action_params":{"evil":true}}`)
	if err := v.Validate(req); err == nil {
		t.Fatal("invalid relay sig on a mutation should still be rejected")
	}
}

// (e) THE load-bearing safety property: an invalid/failed relay signature on a
// READ must FALL THROUGH to lightActions and succeed — never a hard 401. This
// is what makes signing every k8s request (reads included) safe under clock
// skew / verify bugs once an agent is keyed.
func TestValidate_RelaySig_InvalidRead_FallsThroughToLightAction(t *testing.T) {
	priv, pub := relaysigKey(t)
	ver, _ := relaysig.NewVerifier(pub, discardLogger())
	v := &Validator{LightActions: map[string]struct{}{"get_resource": {}}, RelayVerifier: ver}

	body := []byte(`{"action_name":"get_resource"}`)
	req := signedRelayReq(priv, body, "get_resource", "e1")
	// Corrupt the body after signing → signature fails verification.
	req.BodyRaw = []byte(`{"action_name":"get_resource","action_params":{"x":1}}`)
	if err := v.Validate(req); err != nil {
		t.Fatalf("read with a failed relay sig must fall through to lightActions, got: %v", err)
	}
}
