package triggers

import (
	"testing"
	"time"
)

func newTestChurn(threshold int, window, cooldown time.Duration, clock *time.Time) *ChurnSuppressor {
	c := NewChurnSuppressor(0, threshold, window, cooldown)
	c.now = func() time.Time { return *clock }
	return c
}

// The shape this exists for: a controller storing state in a ConfigMap. On the
// dev cluster three of them produced ~2 changes a minute, continuously — 53
// rows from one object in 30 minutes.
func TestChurnSuppressor_SuppressesAfterThreshold(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(3, 10*time.Minute, 6*time.Hour, &now)

	for i := 1; i <= 3; i++ {
		if allowed, _ := c.Allow("cm:autoscaler-status"); !allowed {
			t.Fatalf("fire %d must be allowed; the threshold is 3", i)
		}
		now = now.Add(30 * time.Second)
	}
	allowed, classified := c.Allow("cm:autoscaler-status")
	if allowed {
		t.Error("the fire past the threshold must be suppressed")
	}
	if !classified {
		t.Error("crossing the threshold must be announced once, or the events just vanish")
	}
}

// Announcing on every suppressed fire would be as noisy as the events.
func TestChurnSuppressor_ClassifiesOncePerCooldown(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(2, 10*time.Minute, 6*time.Hour, &now)

	classifications := 0
	for i := 0; i < 20; i++ {
		if _, classified := c.Allow("cm:noisy"); classified {
			classifications++
		}
		now = now.Add(20 * time.Second)
	}
	if classifications != 1 {
		t.Errorf("classified %d times; want exactly 1 per cooldown", classifications)
	}
}

// A person editing the same ConfigMap a few times over an afternoon must not
// be mistaken for a controller.
func TestChurnSuppressor_SlowRepeatsAreNotChurn(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(3, 10*time.Minute, 6*time.Hour, &now)

	for i := 0; i < 10; i++ {
		if allowed, _ := c.Allow("cm:app-config"); !allowed {
			t.Fatalf("edit %d suppressed; edits 15 minutes apart are not churn", i)
		}
		now = now.Add(15 * time.Minute)
	}
}

// After the cooldown the resource gets a clean slate: if the controller that
// was rewriting it is gone, its next real change must be reported.
func TestChurnSuppressor_RecoversAfterCooldown(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(2, 10*time.Minute, time.Hour, &now)

	c.Allow("cm:noisy")
	c.Allow("cm:noisy")
	if allowed, _ := c.Allow("cm:noisy"); allowed {
		t.Fatal("expected suppression before the cooldown elapses")
	}

	now = now.Add(time.Hour + time.Minute)
	if allowed, _ := c.Allow("cm:noisy"); !allowed {
		t.Error("the first change after the cooldown must be reported")
	}
	// Still churning → re-earns suppression within one window.
	now = now.Add(time.Second)
	c.Allow("cm:noisy")
	now = now.Add(time.Second)
	if allowed, _ := c.Allow("cm:noisy"); allowed {
		t.Error("a resource that is still churning must be suppressed again")
	}
}

func TestChurnSuppressor_KeysAreIndependent(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(2, 10*time.Minute, time.Hour, &now)

	for i := 0; i < 5; i++ {
		c.Allow("cm:noisy")
	}
	if allowed, _ := c.Allow("cm:quiet"); !allowed {
		t.Error("one churning resource must not suppress a different one")
	}
}

// Bounded like the rate limiter: a cluster full of churning objects cannot
// grow this without limit.
func TestChurnSuppressor_EvictsBeyondCapacity(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c := NewChurnSuppressor(10, 3, 10*time.Minute, time.Hour)
	c.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		c.Allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if c.Len() > 10 {
		t.Errorf("entries = %d; want the capacity of 10 respected", c.Len())
	}
}

// The classification log has to state the settings that produced it, not the
// package defaults — a suppressor built with different values would otherwise
// announce numbers that never applied.
func TestChurnSuppressor_ReportsItsOwnSettings(t *testing.T) {
	c := NewChurnSuppressor(0, 7, 3*time.Minute, 90*time.Minute)
	if c.threshold != 7 || c.window != 3*time.Minute || c.cooldown != 90*time.Minute {
		t.Errorf("settings not retained: threshold=%d window=%s cooldown=%s", c.threshold, c.window, c.cooldown)
	}
	d := NewChurnSuppressor(0, 0, 0, 0)
	if d.threshold != DefaultChurnThreshold || d.window != DefaultChurnWindow || d.cooldown != DefaultChurnCooldown {
		t.Error("zero values must fall back to the documented defaults")
	}
}
