package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/nudgebee/nudgebee-agent/pkg/canonjson"
)

func TestValidate_HMAC_Roundtrip(t *testing.T) {
	v := &Validator{SigningKey: "test-signing-key"}
	body := map[string]any{
		"action_name":   "prometheus_queries_enricher",
		"timestamp":     int64(1700000000),
		"action_params": map[string]any{"q": "up"},
	}
	sig, err := signHMAC(v.SigningKey, body)
	if err != nil {
		t.Fatalf("signHMAC: %v", err)
	}
	if err := v.Validate(&Request{Body: body, Signature: sig}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_HMAC_RejectsTamperedBody(t *testing.T) {
	v := &Validator{SigningKey: "test-signing-key"}
	body := map[string]any{"action_name": "x"}
	sig, _ := signHMAC(v.SigningKey, body)

	tampered := map[string]any{"action_name": "y"}
	if err := v.Validate(&Request{Body: tampered, Signature: sig}); err == nil {
		t.Fatal("Validate: expected error for tampered body")
	}
}

func TestValidate_LightAction(t *testing.T) {
	v := &Validator{LightActions: map[string]struct{}{"query_data": {}}}

	if err := v.Validate(&Request{ActionName: "query_data"}); err != nil {
		t.Errorf("Validate(allowlisted): %v", err)
	}
	if err := v.Validate(&Request{ActionName: "delete_pod"}); err == nil {
		t.Errorf("Validate(not allowlisted): expected error")
	}
}

func TestValidate_PartialKeys_Roundtrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	signingUUID := uuid.MustParse("12345678-1234-1234-1234-1234567890ab")
	v := &Validator{
		SigningKey: signingUUID.String(),
		PrivateKey: priv,
	}

	body := map[string]any{
		"action_name":   "delete_pod",
		"timestamp":     int64(1700000000),
		"action_params": map[string]any{"pod": "foo"},
	}

	// Pick keyA, derive keyB so that keyA XOR keyB == signingUUID.
	keyA := uuid.MustParse("aaaaaaaa-1111-2222-3333-bbbbbbbbcccc")
	keyB := xorUUIDs(keyA, signingUUID)

	hash := bodyHash(t, body)
	authA := encryptPartial(t, &priv.PublicKey, hash, keyA)
	authB := encryptPartial(t, &priv.PublicKey, hash, keyB)

	if err := v.Validate(&Request{Body: body, PartialAuthA: authA, PartialAuthB: authB}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_PartialKeys_RejectsBadXor(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	signingUUID := uuid.MustParse("12345678-1234-1234-1234-1234567890ab")
	v := &Validator{SigningKey: signingUUID.String(), PrivateKey: priv}

	body := map[string]any{"action_name": "delete_pod"}
	keyA := uuid.MustParse("aaaaaaaa-1111-2222-3333-bbbbbbbbcccc")
	keyB := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff") // wrong — XOR != signingUUID

	hash := bodyHash(t, body)
	authA := encryptPartial(t, &priv.PublicKey, hash, keyA)
	authB := encryptPartial(t, &priv.PublicKey, hash, keyB)

	if err := v.Validate(&Request{Body: body, PartialAuthA: authA, PartialAuthB: authB}); err == nil {
		t.Fatal("Validate: expected error for bad XOR")
	}
}

// Cover the early-return guards that the happy-path tests skip past.

func TestValidate_HMAC_NoSigningKey(t *testing.T) {
	v := &Validator{} // SigningKey empty
	if err := v.Validate(&Request{Body: map[string]any{"a": 1}, Signature: "v0=deadbeef"}); err == nil {
		t.Error("expected error when signing key not configured")
	}
}

func TestValidate_PartialKeys_NoPrivateKey(t *testing.T) {
	v := &Validator{SigningKey: "x"} // PrivateKey nil
	if err := v.Validate(&Request{Body: map[string]any{}, PartialAuthA: "a", PartialAuthB: "b"}); err == nil {
		t.Error("expected error when private key not configured")
	}
}

func TestValidate_PartialKeys_NoSigningKey(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Validator{PrivateKey: priv} // SigningKey empty
	if err := v.Validate(&Request{Body: map[string]any{}, PartialAuthA: "a", PartialAuthB: "b"}); err == nil {
		t.Error("expected error when signing key not configured")
	}
}

func TestValidate_PartialKeys_OneSideMissing(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Validator{SigningKey: uuid.NewString(), PrivateKey: priv}
	// Only auth A present; auth B empty.
	if err := v.Validate(&Request{Body: map[string]any{}, PartialAuthA: "x", PartialAuthB: ""}); err == nil {
		t.Error("expected error when one partial-auth side is missing (validatePartialKeys path)")
	}
}

func TestValidate_PartialKeys_BadBase64(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Validator{SigningKey: uuid.NewString(), PrivateKey: priv}
	err := v.Validate(&Request{
		Body:         map[string]any{},
		PartialAuthA: "!!!not-base64!!!",
		PartialAuthB: "!!!not-base64!!!",
	})
	if err == nil {
		t.Error("expected error for non-base64 ciphertext")
	}
}

func TestValidate_PartialKeys_GarbledCiphertext(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Validator{SigningKey: uuid.NewString(), PrivateKey: priv}
	// Valid base64 but not a valid OAEP ciphertext.
	err := v.Validate(&Request{
		Body:         map[string]any{},
		PartialAuthA: "AAAA",
		PartialAuthB: "AAAA",
	})
	if err == nil {
		t.Error("expected error for garbled ciphertext")
	}
}

func TestValidate_PartialKeys_BadSigningKeyUUID(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	v := &Validator{SigningKey: "not-a-uuid", PrivateKey: priv}

	// Construct VALID partials (so we hit the signing-key parse error
	// path rather than getting rejected earlier).
	body := map[string]any{"action_name": "x"}
	hash := bodyHash(t, body)
	keyA := uuid.MustParse("aaaaaaaa-1111-2222-3333-bbbbbbbbcccc")
	keyB := uuid.MustParse("aaaaaaaa-1111-2222-3333-bbbbbbbbcccc") // any UUID; we want to fail before XOR check
	authA := encryptPartial(t, &priv.PublicKey, hash, keyA)
	authB := encryptPartial(t, &priv.PublicKey, hash, keyB)

	if err := v.Validate(&Request{Body: body, PartialAuthA: authA, PartialAuthB: authB}); err == nil {
		t.Error("expected error for non-UUID signing key")
	}
}

func TestValidate_LightAction_DisabledWhenAllowlistNil(t *testing.T) {
	v := &Validator{}
	if err := v.Validate(&Request{ActionName: "anything"}); err == nil {
		t.Error("expected error when light-action allowlist not configured")
	}
}

func TestValidate_PartialKeys_RejectsBadHash(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	signingUUID := uuid.MustParse("12345678-1234-1234-1234-1234567890ab")
	v := &Validator{SigningKey: signingUUID.String(), PrivateKey: priv}

	body := map[string]any{"action_name": "delete_pod"}
	keyA := uuid.MustParse("aaaaaaaa-1111-2222-3333-bbbbbbbbcccc")
	keyB := xorUUIDs(keyA, signingUUID)

	wrongHash := "v0=deadbeef"
	authA := encryptPartial(t, &priv.PublicKey, wrongHash, keyA)
	authB := encryptPartial(t, &priv.PublicKey, wrongHash, keyB)

	if err := v.Validate(&Request{Body: body, PartialAuthA: authA, PartialAuthB: authB}); err == nil {
		t.Fatal("Validate: expected error for bad hash")
	}
}

// signHMAC mirrors sign_action_request: build
// "v0:" + canonjson.EncodeForSignature(body), HMAC-SHA256, prefix "v0=".
func signHMAC(signingKey string, body any) (string, error) {
	encoded, err := canonjson.EncodeForSignature(body)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte("v0:"))
	mac.Write(encoded)
	return "v0=" + hex.EncodeToString(mac.Sum(nil)), nil
}

func bodyHash(t *testing.T, body any) string {
	t.Helper()
	encoded, err := canonjson.Encode(body)
	if err != nil {
		t.Fatalf("canonjson.Encode: %v", err)
	}
	sum := sha256.Sum256(encoded)
	return "v0=" + hex.EncodeToString(sum[:])
}

func encryptPartial(t *testing.T, pub *rsa.PublicKey, hash string, key uuid.UUID) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hash": hash,
		"key":  key.String(),
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, payload, nil)
	if err != nil {
		t.Fatalf("rsa.EncryptOAEP: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

func xorUUIDs(a, b uuid.UUID) uuid.UUID {
	aInt := new(big.Int).SetBytes(a[:])
	bInt := new(big.Int).SetBytes(b[:])
	xored := new(big.Int).Xor(aInt, bInt)

	var out uuid.UUID
	xb := xored.Bytes()
	// Left-pad to 16 bytes.
	copy(out[16-len(xb):], xb)
	return out
}
