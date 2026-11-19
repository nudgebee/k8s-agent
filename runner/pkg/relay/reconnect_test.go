package relay

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestReconnectWaitBounds(t *testing.T) {
	const base, limit = 3 * time.Second, 30 * time.Second
	highest := func(n int64) int64 { return n - 1 }
	for attempt, want := range []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second, 30 * time.Second, 30 * time.Second} {
		if got := reconnectWait(base, limit, attempt, highest); got != want {
			t.Errorf("attempt %d: longest wait = %v, want %v", attempt, got, want)
		}
		if got := reconnectWait(base, limit, attempt, func(int64) int64 { return 0 }); got != 0 {
			t.Errorf("attempt %d: shortest wait = %v, want 0", attempt, got)
		}
	}
	// Doubling must not overflow however many attempts have failed.
	if got := reconnectWait(base, limit, 1000, highest); got != limit {
		t.Errorf("attempt 1000: longest wait = %v, want %v", got, limit)
	}
	// A huge limit must not overflow the doubling or randN's bound.
	if got := reconnectWait(time.Hour, math.MaxInt64, 1000, highest); got != math.MaxInt64-1 {
		t.Errorf("max limit: longest wait = %v, want %v", got, time.Duration(math.MaxInt64-1))
	}
	if got := reconnectWait(0, limit, 0, highest); got != 0 {
		t.Errorf("zero base: wait = %v, want 0", got)
	}
}

// TestReconnectWaitSpreadsAgents pins the point of the jitter: agents that
// lose the relay at the same moment do not all redial it at the same moment.
func TestReconnectWaitSpreadsAgents(t *testing.T) {
	const agents = 200
	buckets := map[time.Duration]int{}
	for range agents {
		buckets[reconnectWait(3*time.Second, 30*time.Second, 0, rand.Int64N).Truncate(500*time.Millisecond)]++
	}
	for b, n := range buckets {
		if n > agents/2 {
			t.Fatalf("%d of %d agents redial within the same 500ms (%v); want them spread over the first 3s", n, agents, b)
		}
	}
	if len(buckets) < 5 {
		t.Fatalf("redials fell in %d half-second buckets, want spread over ~6", len(buckets))
	}
}
