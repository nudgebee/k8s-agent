package relaysig

import (
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- Shared cross-repo signing vector ---------------------------------------
// These MUST stay byte-identical to the relay-server's signer_test.go vector so
// the two implementations can't silently drift. Seed → key; signing `vectorBody`
// with that key yields exactly vectorSigB64. (Mirrors the v0:/v0= HMAC contract.)
const (
	vectorSeedB64 = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="
	vectorPubB64  = "ebVWLo/mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ="
	vectorSigB64  = "EuCrun1jNPY3biXu7Faf7M3K1mAqMF18jXxm36ltMoHFjZfCaWafUzgorxWysf4mb/I6vgip2C+b++FMytCCDw=="
	vectorBody    = `{"account_id":"acct-1","action_name":"delete_workload","action_params":{"kind":"deployment","name":"web","namespace":"shop"},"timestamp":1700000000}`
)

func mustSeedKey(t *testing.T, seedB64 string) ed25519.PrivateKey {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func freshEnv(priv ed25519.PrivateKey, body []byte, nonce string) Envelope {
	sig := ed25519.Sign(priv, body)
	return Envelope{
		Signature: base64.StdEncoding.EncodeToString(sig),
		SignedAt:  time.Now().UTC().Format(time.RFC3339),
		Nonce:     nonce,
		KeyID:     "test",
	}
}

// TestVector_CrossImplStability guards against drift between this verifier and
// the relay signer: the fixed seed signing the fixed body must produce exactly
// the fixed signature, and that signature must verify under the fixed pub key.
func TestVector_CrossImplStability(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	gotSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(vectorBody)))
	if gotSig != vectorSigB64 {
		t.Fatalf("signature drift:\n got %s\nwant %s", gotSig, vectorSigB64)
	}
	v, err := NewVerifier(vectorPubB64, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{Signature: vectorSigB64, SignedAt: time.Now().UTC().Format(time.RFC3339), Nonce: "n-vec"}
	if err := v.VerifyBody([]byte(vectorBody), env); err != nil {
		t.Fatalf("vector did not verify: %v", err)
	}
}

func TestVerifyBody_HappyPath(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	v, err := NewVerifier(vectorPubB64, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(vectorBody)
	if err := v.VerifyBody(body, freshEnv(priv, body, "n1")); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
}

func TestVerifyBody_Tamper(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	v, _ := NewVerifier(vectorPubB64, testLogger())
	body := []byte(vectorBody)
	env := freshEnv(priv, body, "n2")
	// Verify against a body whose params were altered after signing.
	tampered := []byte(`{"account_id":"acct-1","action_name":"delete_workload","action_params":{"kind":"deployment","name":"PROD","namespace":"shop"},"timestamp":1700000000}`)
	if err := v.VerifyBody(tampered, env); err == nil {
		t.Fatal("tampered body verified; want failure")
	}
}

func TestVerifyBody_WrongKey(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	// Verifier configured with a DIFFERENT key.
	otherSeed := make([]byte, 32)
	otherSeed[0] = 99
	otherPub := ed25519.NewKeyFromSeed(otherSeed).Public().(ed25519.PublicKey)
	v, _ := NewVerifier(base64.StdEncoding.EncodeToString(otherPub), testLogger())
	body := []byte(vectorBody)
	if err := v.VerifyBody(body, freshEnv(priv, body, "n3")); err == nil {
		t.Fatal("wrong-key verify passed; want failure")
	}
}

func TestVerifyBody_Replay(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	v, _ := NewVerifier(vectorPubB64, testLogger())
	body := []byte(vectorBody)
	env := freshEnv(priv, body, "dup")
	if err := v.VerifyBody(body, env); err != nil {
		t.Fatalf("first verify failed: %v", err)
	}
	// Same nonce again → replay rejection.
	env2 := freshEnv(priv, body, "dup")
	if err := v.VerifyBody(body, env2); err == nil {
		t.Fatal("replayed nonce verified; want failure")
	}
}

func TestVerifyBody_Skew(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	v, _ := NewVerifier(vectorPubB64, testLogger())
	body := []byte(vectorBody)
	env := freshEnv(priv, body, "n4")
	env.SignedAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if err := v.VerifyBody(body, env); err == nil {
		t.Fatal("stale signed_at verified; want failure")
	}
}

func TestVerifyBody_MultiKey(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	// Two keys configured (rotation): first is unrelated, second is the signer.
	other := make([]byte, 32)
	other[0] = 7
	otherPub := ed25519.NewKeyFromSeed(other).Public().(ed25519.PublicKey)
	v, err := NewVerifier(base64.StdEncoding.EncodeToString(otherPub)+","+vectorPubB64, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(vectorBody)
	if err := v.VerifyBody(body, freshEnv(priv, body, "n5")); err != nil {
		t.Fatalf("multi-key verify failed: %v", err)
	}
}

// TestVerifyBody_ConcurrentReplay fires many goroutines verifying the SAME
// valid signed message with the SAME nonce. Exactly one must succeed; the rest
// must be rejected as replays. This guards the atomic check-and-record fix
// against the TOCTOU race a separate pre-check would reopen.
func TestVerifyBody_ConcurrentReplay(t *testing.T) {
	priv := mustSeedKey(t, vectorSeedB64)
	v, err := NewVerifier(vectorPubB64, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(vectorBody)
	env := freshEnv(priv, body, "concurrent-nonce")

	const n = 64
	var wg sync.WaitGroup
	var successes int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := v.VerifyBody(body, env); err == nil {
				atomic.AddInt64(&successes, 1)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent replay: %d goroutines succeeded, want exactly 1", successes)
	}
}

func TestNewVerifier_DisabledWhenEmpty(t *testing.T) {
	v, err := NewVerifier("", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Fatal("verifier with no key should be disabled")
	}
	if err := v.VerifyBody([]byte(vectorBody), Envelope{Signature: "x", SignedAt: "y", Nonce: "z"}); err == nil {
		t.Fatal("disabled verifier should refuse VerifyBody")
	}
}

func TestParsePublicKey_Formats(t *testing.T) {
	// raw base64 (covered by vectorPubB64) already exercised; ensure it parses.
	if _, err := parsePublicKey(vectorPubB64); err != nil {
		t.Fatalf("raw base64 pubkey parse: %v", err)
	}
	if _, err := parsePublicKey("not-a-key"); err == nil {
		t.Fatal("garbage key parsed; want error")
	}
}
