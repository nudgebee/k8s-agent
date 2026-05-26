package triggers

import (
	"container/list"
	"sync"
	"time"
)

// RateLimiter is a bounded LRU + TTL cache keyed by string. Used to
// suppress repeat fires of the same trigger fingerprint within the
// matcher's RateLimit window — pod_oom_killed uses rate_limit=3600.
//
// Single-replica agent deployments mean we don't need cross-process
// coordination; in-memory is enough. Plan agent flagged that restart
// loses state — see Engine's grace-window for the mitigation.
//
// The cap (`MaxEntries`) protects against pathological label/name
// cardinality (an alert with random labels could otherwise grow
// unbounded). When full we evict the oldest by access time, not by
// expiry. ~10k entries ≈ ~1 MB of memory, comfortable headroom.
type RateLimiter struct {
	mu         sync.Mutex
	maxEntries int
	now        func() time.Time // injectable for tests

	order *list.List // *entry, front=newest
	index map[string]*list.Element
}

type entry struct {
	key       string
	expiresAt time.Time
}

// NewRateLimiter returns a limiter with the given capacity. Pass 0 to
// use the default (10000). Callers don't need to call Close — there's
// no goroutine.
func NewRateLimiter(maxEntries int) *RateLimiter {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &RateLimiter{
		maxEntries: maxEntries,
		now:        time.Now,
		order:      list.New(),
		index:      make(map[string]*list.Element, maxEntries),
	}
}

// Allow returns true when the key is fresh (no record within window or
// the previous record's TTL has expired) and records a new entry that
// expires `window` from now. Returns false when a non-expired record
// exists — the caller should drop the fire.
//
// Window 0 disables the limiter for this call (always returns true,
// no record kept) — convenient for matchers with no rate limit.
func (r *RateLimiter) Allow(key string, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	if el, ok := r.index[key]; ok {
		e := el.Value.(*entry)
		if now.Before(e.expiresAt) {
			// Existing record still in window — suppress the fire,
			// don't refresh the TTL (a continuously-firing condition
			// shouldn't extend the suppression indefinitely).
			return false
		}
		// Expired — refresh the record + bump to MRU.
		e.expiresAt = now.Add(window)
		r.order.MoveToFront(el)
		return true
	}

	// New entry. Evict if we're at the cap.
	if r.order.Len() >= r.maxEntries {
		oldest := r.order.Back()
		if oldest != nil {
			old := oldest.Value.(*entry)
			delete(r.index, old.key)
			r.order.Remove(oldest)
		}
	}
	el := r.order.PushFront(&entry{key: key, expiresAt: now.Add(window)})
	r.index[key] = el
	return true
}

// Len returns the current entry count. Test-only.
func (r *RateLimiter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order.Len()
}
