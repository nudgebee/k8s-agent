package triggers

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsFirst_SuppressesWithinWindow(t *testing.T) {
	rl := NewRateLimiter(0)
	if !rl.Allow("k", time.Minute) {
		t.Fatal("first Allow must return true")
	}
	if rl.Allow("k", time.Minute) {
		t.Error("second Allow within window must return false")
	}
}

func TestRateLimiter_AllowsAfterExpiry(t *testing.T) {
	rl := NewRateLimiter(0)
	clock := time.Now()
	rl.now = func() time.Time { return clock }

	if !rl.Allow("k", time.Minute) {
		t.Fatal("first Allow must return true")
	}
	clock = clock.Add(2 * time.Minute) // skip past TTL
	if !rl.Allow("k", time.Minute) {
		t.Error("Allow after expiry must return true")
	}
}

func TestRateLimiter_ZeroWindowDisables(t *testing.T) {
	// Matchers with RateLimit=0 should fire every time — used for
	// transition-gated triggers (job_failure, node_not_ready) where the
	// fingerprint already varies per transition.
	rl := NewRateLimiter(0)
	for i := 0; i < 5; i++ {
		if !rl.Allow("k", 0) {
			t.Errorf("Allow(window=0) must always return true; iteration %d", i)
		}
	}
	if rl.Len() != 0 {
		t.Errorf("Len = %d; window=0 must not store entries", rl.Len())
	}
}

func TestRateLimiter_DifferentKeysIndependent(t *testing.T) {
	rl := NewRateLimiter(0)
	if !rl.Allow("a", time.Minute) {
		t.Fatal("a should fire")
	}
	if !rl.Allow("b", time.Minute) {
		t.Fatal("b should fire (independent key)")
	}
	if rl.Allow("a", time.Minute) || rl.Allow("b", time.Minute) {
		t.Error("repeats of either key must be suppressed")
	}
}

func TestRateLimiter_EvictsAtCap(t *testing.T) {
	rl := NewRateLimiter(3)
	for i := 0; i < 10; i++ {
		rl.Allow(string(rune('a'+i)), time.Hour)
	}
	if rl.Len() != 3 {
		t.Errorf("Len = %d; want 3 (LRU cap)", rl.Len())
	}
	// Oldest evicted ('a') — Allow should now return true (no record exists).
	if !rl.Allow("a", time.Hour) {
		t.Error("evicted 'a' should be allowed again")
	}
}
