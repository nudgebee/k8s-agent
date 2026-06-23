// Package auth implements three authentication paths:
//
//  1. HMAC signature        — validate_action_request_signature
//  2. RSA-OAEP partial keys — validate_with_private_key
//  3. Light-action allowlist — validate_light_action
//
// The dispatcher picks the first present mode in the request and skips the others.
package auth

import (
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/nudgebee/nudgebee-agent/pkg/canonjson"
	"github.com/nudgebee/nudgebee-agent/pkg/relaysig"
)

// LoadPrivateKey reads a PEM-encoded RSA private key from disk. Supports
// both PKCS#1 ("BEGIN RSA PRIVATE KEY") and PKCS#8 ("BEGIN PRIVATE KEY")
// formats — the auth-config.yaml ConfigMap uses PKCS#8.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read RSA key from %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("auth: %s does not contain a PEM block", path)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("auth: %s contains a non-RSA private key", path)
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("auth: unexpected PEM block type %q in %s", block.Type, path)
	}
}

// Request is the in-memory shape of an incoming ExternalActionRequest after
// JSON unmarshal. Body is the same map the wire JSON `body` field decoded to;
// canonjson encodes it before signing/hashing.
type Request struct {
	Body         map[string]any
	ActionName   string
	Signature    string // HMAC mode
	PartialAuthA string // RSA partial keys mode (encrypted, base64)
	PartialAuthB string // RSA partial keys mode (encrypted, base64)

	// RelaySig + BodyRaw drive the relay-signature path. BodyRaw is the exact
	// bytes of the wire `body` field (a json.RawMessage) — NOT a re-marshaled
	// Body map — so it matches the bytes the relay signed.
	RelaySig relaysig.Envelope
	BodyRaw  []byte
}

// Validator holds the agent's auth state.
//
// LightActions is atomically swappable so refresh_playbook can update the
// allowlist without dropping the WS connection or interleaving with concurrent
// Validate calls. Use SetLightActions for runtime updates.
type Validator struct {
	// SigningKey is the shared HMAC secret AND the UUID that key_a XOR key_b
	// must reconstruct in the partial-keys path. Empty disables both modes.
	SigningKey string

	// PrivateKey is the agent-side RSA private key for the partial-keys path.
	// Nil disables partial-keys auth.
	PrivateKey *rsa.PrivateKey

	// LightActions can be set directly at startup; SetLightActions /
	// LightActionsSet hot-swap it under load.
	LightActions map[string]struct{}

	// lightActionsAtomic, when non-nil, takes precedence over LightActions.
	// Set via SetLightActions.
	lightActionsAtomic atomic.Pointer[map[string]struct{}]

	// RelayVerifier validates an Ed25519 relay signature over the raw body.
	// When set and Enabled(), a present+valid relay signature authorizes ANY
	// action (bypassing the HMAC/partial/lightAction modes). Nil or disabled =
	// the relay signature is ignored and the request falls through to the
	// existing modes, so reads keep working on un-keyed agents.
	RelayVerifier *relaysig.Verifier
}

// SetLightActions atomically replaces the light-action allowlist. Concurrent
// Validate calls see either the old or new set, never a torn read. Pass an
// empty map to disable all light actions; pass nil to fall back to the
// startup-set Validator.LightActions.
func (v *Validator) SetLightActions(actions map[string]struct{}) {
	if actions == nil {
		v.lightActionsAtomic.Store(nil)
		return
	}
	// Defensive copy so callers can't mutate the live map under us.
	dup := make(map[string]struct{}, len(actions))
	for k := range actions {
		dup[k] = struct{}{}
	}
	v.lightActionsAtomic.Store(&dup)
}

// LightActionsSet returns the active allowlist (atomic if set, otherwise
// the startup map). Callers must NOT mutate the result.
func (v *Validator) LightActionsSet() map[string]struct{} {
	if p := v.lightActionsAtomic.Load(); p != nil {
		return *p
	}
	return v.LightActions
}

// Validate returns nil if the request is authentic per any of the supported
// modes. Mirrors the backend's dispatch logic.
//
// Relay-signature handling is a non-fatal pre-check: a VALID relay signature
// (verifier configured + enabled) authorizes any action; anything else — no
// signature, no key, or a signature that fails verification (bad sig, clock
// skew, replay) — FALLS THROUGH to the existing modes rather than hard-failing.
//
// Falling through is deliberate and load-bearing: the relay signs every k8s
// request (reads included), so a hard 401 on a failed signature would break
// reads that work today via lightActions — e.g. on an upgraded-but-unkeyed
// agent, or under clock skew. With fall-through, reads always survive via
// lightActions and only mutations (which aren't light actions) effectively
// require a valid relay signature. Falling through never grants extra access:
// a forged/invalid mutation signature still hits the lightAction check and is
// rejected.
func (v *Validator) Validate(r *Request) error {
	if r.RelaySig.Present() && v.RelayVerifier != nil && v.RelayVerifier.Enabled() {
		if err := v.RelayVerifier.VerifyBody(r.BodyRaw, r.RelaySig); err == nil {
			return nil // valid relay signature authorizes any action
		}
		// Verification failed — fall through to the existing modes (reads keep
		// working via lightActions; mutations get rejected there).
	}
	switch {
	case r.Signature != "":
		return v.validateHMAC(r)
	case r.PartialAuthA != "" || r.PartialAuthB != "":
		return v.validatePartialKeys(r)
	default:
		return v.validateLightAction(r)
	}
}

func (v *Validator) validateLightAction(r *Request) error {
	set := v.LightActionsSet()
	if set == nil {
		return errors.New("auth: light-action mode disabled")
	}
	if _, ok := set[r.ActionName]; !ok {
		return fmt.Errorf("auth: action %q not in light-action allowlist", r.ActionName)
	}
	return nil
}

// validateHMAC mirrors sign_action_request:
//
//	format_req = "v0:" + body.json(exclude_none=True, sort_keys=True)
//	mac = hmac.new(signing_key, format_req, sha256).hexdigest()
//	signature = "v0=" + mac
//
// Note the WITH-SPACES canonical form (default Python json.dumps separators),
// not the no-space form used in the partial-keys hash path.
func (v *Validator) validateHMAC(r *Request) error {
	if v.SigningKey == "" {
		return errors.New("auth: signing key not configured")
	}
	body, err := canonjson.EncodeForSignature(r.Body)
	if err != nil {
		return fmt.Errorf("auth: encode body for sig: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(v.SigningKey))
	mac.Write([]byte("v0:"))
	mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(r.Signature)) {
		return errors.New("auth: signature mismatch")
	}
	return nil
}

// validatePartialKeys decrypts both partial payloads with the agent RSA key
// (OAEP-MGF1-SHA256), parses {key, hash}, verifies both hashes match
// v0=sha256(canonical_json(body)), and verifies
// key_a XOR key_b == signing_key UUID as a 128-bit int.
func (v *Validator) validatePartialKeys(r *Request) error {
	if v.PrivateKey == nil {
		return errors.New("auth: private key not configured")
	}
	if v.SigningKey == "" {
		return errors.New("auth: signing key not configured")
	}
	if r.PartialAuthA == "" || r.PartialAuthB == "" {
		return errors.New("auth: missing partial auth")
	}

	body, err := canonjson.Encode(r.Body) // no-space form for partial-keys hash path
	if err != nil {
		return fmt.Errorf("auth: encode body for partial-key hash: %w", err)
	}
	sum := sha256.Sum256(body)
	expectedHash := "v0=" + hex.EncodeToString(sum[:])

	keyA, err := v.extractPartialKey(r.PartialAuthA, expectedHash)
	if err != nil {
		return fmt.Errorf("auth: partial_auth_a: %w", err)
	}
	keyB, err := v.extractPartialKey(r.PartialAuthB, expectedHash)
	if err != nil {
		return fmt.Errorf("auth: partial_auth_b: %w", err)
	}

	signingUUID, err := uuid.Parse(v.SigningKey)
	if err != nil {
		return fmt.Errorf("auth: signing key not a UUID: %w", err)
	}

	// XOR of two 128-bit UUIDs as bytes; compare with signing_key UUID bytes.
	want := uuidToBigInt(signingUUID)
	got := new(big.Int).Xor(uuidToBigInt(keyA), uuidToBigInt(keyB))
	if want.Cmp(got) != 0 {
		return errors.New("auth: key_a XOR key_b does not match signing key")
	}
	return nil
}

// extractPartialKey decrypts one base64-encoded RSA-OAEP-MGF1-SHA256 ciphertext,
// parses {hash, key} JSON inside, verifies the hash matches the request body,
// and returns the embedded UUID.
func (v *Validator) extractPartialKey(encoded, expectedHash string) (uuid.UUID, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return uuid.Nil, fmt.Errorf("base64: %w", err)
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), nil, v.PrivateKey, ciphertext, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("rsa-oaep: %w", err)
	}
	var auth struct {
		Hash string `json:"hash"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(plain, &auth); err != nil {
		return uuid.Nil, fmt.Errorf("json: %w", err)
	}
	if !hmac.Equal([]byte(auth.Hash), []byte(expectedHash)) {
		return uuid.Nil, errors.New("hash mismatch")
	}
	parsed, err := uuid.Parse(auth.Key)
	if err != nil {
		return uuid.Nil, fmt.Errorf("key not a UUID: %w", err)
	}
	return parsed, nil
}

func uuidToBigInt(u uuid.UUID) *big.Int {
	var n big.Int
	return n.SetBytes(u[:])
}
