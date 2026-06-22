// Package relaysig verifies Ed25519 signatures that the relay-server stamps on
// requests forwarded to native ("k8s") agents.
//
// Unlike the agent's per-account HMAC/RSA auth (pkg/auth), this proves a request
// transited the trusted relay: the relay signs the raw bytes of the request's
// `body` field with its global Ed25519 key, and the agent verifies that exact
// byte string with the relay's public key. A valid relay signature authorizes
// any action (the relay already enforced the api-server's per-action permission
// check + X-SECRET-KEY gate); see pkg/auth.Validator.
//
// The signature is carried in NEW envelope fields (`relay_signature`,
// `relay_signed_at`, `relay_nonce`, `relay_key_id`) — deliberately NOT the
// `signature` field, which the HMAC path owns. Old agents ignore the new fields,
// so this is purely additive.
//
// Binding the raw `body` bytes (rather than an extracted/canonicalised subset)
// means the signature covers account_id + action_name + action_params +
// timestamp together, and the agent verifies the identical bytes it executes —
// so there is no payload-substitution gap and no canonicalisation ambiguity. The
// transport (RabbitMQ → WS) passes the payload through byte-for-byte.
package relaysig

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// maxTimestampSkew bounds the gap between relay_signed_at and now.
	maxTimestampSkew = 5 * time.Minute
	// maxNonces caps the replay-prevention set before time-based eviction.
	maxNonces = 10000
)

// Envelope is the relay signature metadata, parsed by the caller from the
// `relay_*` top-level fields of the request.
type Envelope struct {
	Signature string // base64 Ed25519 signature over the raw `body` bytes
	SignedAt  string // RFC3339 timestamp
	Nonce     string // unique id for replay prevention
	KeyID     string // optional key id (rotation/logging)
}

// Present reports whether a relay signature is attached.
func (e Envelope) Present() bool { return e.Signature != "" }

// Verifier validates relay Ed25519 signatures over request bodies. It holds one
// or more public keys so the relay's signing key can be rotated without a
// synchronized agent restart (verification succeeds if ANY configured key
// matches).
type Verifier struct {
	publicKeys []ed25519.PublicKey
	logger     *slog.Logger

	seenNonces map[string]time.Time
	nonceMu    sync.Mutex
}

// NewVerifier builds a verifier from one or more public keys (comma- or
// whitespace-separated). Each key may be OpenSSH `ssh-ed25519 ...`, PEM (PKIX),
// or raw base64 (32 bytes). An empty string yields a disabled verifier
// (Enabled()==false) — the caller then ignores any relay signature and falls
// back to existing auth, so reads keep working on un-keyed agents.
func NewVerifier(publicKeysStr string, logger *slog.Logger) (*Verifier, error) {
	v := &Verifier{logger: logger, seenNonces: make(map[string]time.Time)}

	keys, err := parsePublicKeys(publicKeysStr)
	if err != nil {
		return nil, fmt.Errorf("invalid relay signing public key: %w", err)
	}
	v.publicKeys = keys
	if len(keys) == 0 {
		logger.Warn("relay signature verification disabled: no RELAY_SIGNING_PUBLIC_KEY configured")
	} else {
		logger.Info("relay signature verification enabled", "keys", len(keys))
	}
	return v, nil
}

// Enabled reports whether at least one public key is loaded.
func (v *Verifier) Enabled() bool { return len(v.publicKeys) > 0 }

// VerifyBody checks the relay signature over bodyRaw — the exact bytes of the
// request's `body` field as received. Returns nil only when the signature is
// valid under some configured key, the timestamp is within skew, and the nonce
// has not been seen. Callers MUST pass the raw `body` bytes (e.g. a
// json.RawMessage), never a re-marshaled map, so the bytes match what the relay
// signed.
func (v *Verifier) VerifyBody(bodyRaw []byte, env Envelope) error {
	if !v.Enabled() {
		return fmt.Errorf("relay signature verification not configured")
	}
	if env.Signature == "" {
		return fmt.Errorf("relay signature: missing relay_signature")
	}
	if env.SignedAt == "" {
		return fmt.Errorf("relay signature: missing relay_signed_at")
	}
	if env.Nonce == "" {
		return fmt.Errorf("relay signature: missing relay_nonce")
	}
	if len(bodyRaw) == 0 {
		return fmt.Errorf("relay signature: empty body")
	}

	signedAt, err := time.Parse(time.RFC3339, env.SignedAt)
	if err != nil {
		return fmt.Errorf("relay signature: invalid relay_signed_at: %w", err)
	}
	if absDuration(time.Since(signedAt)) > maxTimestampSkew {
		return fmt.Errorf("relay signature: relay_signed_at %s outside ±%s window", env.SignedAt, maxTimestampSkew)
	}

	if v.isReplayedNonce(env.Nonce) {
		return fmt.Errorf("relay signature: nonce %s already seen (replay)", env.Nonce)
	}

	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("relay signature: invalid signature encoding: %w", err)
	}

	verified := false
	for _, pk := range v.publicKeys {
		if ed25519.Verify(pk, bodyRaw, sig) {
			verified = true
			break
		}
	}
	if !verified {
		return fmt.Errorf("relay signature: invalid signature")
	}

	// Record the nonce only after a fully successful verification so a forged
	// or stale message can't consume a nonce a legitimate retry might reuse.
	v.recordNonce(env.Nonce)
	return nil
}

// parsePublicKeys splits on commas/whitespace and parses each entry. Returns an
// empty slice (no error) for an empty input.
func parsePublicKeys(s string) ([]ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// PEM keys contain newlines internally, so only split on commas when the
	// input isn't a PEM block; otherwise treat the whole string as one key.
	var fields []string
	if strings.Contains(s, "-----BEGIN") {
		fields = []string{s}
	} else {
		fields = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' })
	}
	var keys []ed25519.PublicKey
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		k, err := parsePublicKey(f)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// parsePublicKey accepts OpenSSH authorized_keys, PEM (PKIX), or raw base64.
func parsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, "ssh-ed25519 ") {
		pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
		if err != nil {
			return nil, fmt.Errorf("OpenSSH public key parse failed: %w", err)
		}
		cryptoPub, ok := pubKey.(ssh.CryptoPublicKey)
		if !ok {
			return nil, fmt.Errorf("OpenSSH key does not implement CryptoPublicKey")
		}
		edKey, ok := cryptoPub.CryptoPublicKey().(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("OpenSSH key is not Ed25519")
		}
		return edKey, nil
	}

	if block, _ := pem.Decode([]byte(s)); block != nil {
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PEM parse failed: %w", err)
		}
		edKey, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("PEM key is not Ed25519")
		}
		return edKey, nil
	}

	keyBytes, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed: %w", err)
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected %d bytes, got %d", ed25519.PublicKeySize, len(keyBytes))
	}
	return ed25519.PublicKey(keyBytes), nil
}

func (v *Verifier) isReplayedNonce(nonce string) bool {
	v.nonceMu.Lock()
	defer v.nonceMu.Unlock()
	_, seen := v.seenNonces[nonce]
	return seen
}

func (v *Verifier) recordNonce(nonce string) {
	v.nonceMu.Lock()
	defer v.nonceMu.Unlock()
	v.seenNonces[nonce] = time.Now()
	if len(v.seenNonces) > maxNonces {
		cutoff := time.Now().Add(-maxTimestampSkew * 2)
		for n, t := range v.seenNonces {
			if t.Before(cutoff) {
				delete(v.seenNonces, n)
			}
		}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
