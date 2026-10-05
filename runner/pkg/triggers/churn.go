package triggers

import (
	"container/list"
	"sync"
	"time"
)

// Defaults for ConfigMap change suppression. A person edits a ConfigMap and
// then goes and looks at something; a controller storing state in one rewrites
// it forever. Three changes inside ten minutes is well outside anything a
// human does to the same object and well inside what a controller does — on
// the dev cluster the three worst offenders each produced ~2 changes per
// minute, continuously.
const (
	DefaultChurnThreshold = 3
	DefaultChurnWindow    = 10 * time.Minute
	DefaultChurnCooldown  = 6 * time.Hour
)

// ChurnSuppressor stops reporting a resource that keeps rewriting itself.
//
// This is deliberately frequency-based rather than a list of names. The
// alternative — deciding from `metadata.managedFields[].manager` whether a
// human or a controller wrote it — reads better on paper but fails in the
// dangerous direction: two of the three noisiest ConfigMaps on our own dev
// cluster record no manager at all, and an allow-list of known-good writers
// silently drops changes from any deploy tool nobody thought to enumerate.
// Losing a real change is far worse than keeping a noisy one, so the rule
// only ever suppresses what has already proven itself chatty.
//
// Bounded the same way as RateLimiter: an LRU over keys, so a cluster full of
// churning objects cannot grow this without limit.
type ChurnSuppressor struct {
	mu         sync.Mutex
	maxEntries int
	threshold  int
	window     time.Duration
	cooldown   time.Duration
	now        func() time.Time // injectable for tests

	order *list.List // *churnEntry, front=newest
	index map[string]*list.Element
}

type churnEntry struct {
	key           string
	fires         int
	windowStart   time.Time
	cooldownUntil time.Time
}

// NewChurnSuppressor returns a suppressor with the given capacity. Pass 0 for
// the default (10000), and zero values for threshold/window/cooldown to take
// the defaults above.
func NewChurnSuppressor(maxEntries, threshold int, window, cooldown time.Duration) *ChurnSuppressor {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if threshold <= 0 {
		threshold = DefaultChurnThreshold
	}
	if window <= 0 {
		window = DefaultChurnWindow
	}
	if cooldown <= 0 {
		cooldown = DefaultChurnCooldown
	}
	return &ChurnSuppressor{
		maxEntries: maxEntries,
		threshold:  threshold,
		window:     window,
		cooldown:   cooldown,
		now:        time.Now,
		order:      list.New(),
		index:      make(map[string]*list.Element, maxEntries),
	}
}

// Allow records a fire for the key and reports whether it should be emitted.
//
// `classified` is true exactly once per cooldown period — on the fire that
// crosses the threshold — so the caller can log the decision. A resource that
// silently stops producing events is indistinguishable from one that stopped
// changing, which is the kind of thing that costs an hour during an incident.
func (c *ChurnSuppressor) Allow(key string) (allowed, classified bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()

	el, ok := c.index[key]
	if !ok {
		if c.order.Len() >= c.maxEntries {
			if oldest := c.order.Back(); oldest != nil {
				delete(c.index, oldest.Value.(*churnEntry).key)
				c.order.Remove(oldest)
			}
		}
		c.index[key] = c.order.PushFront(&churnEntry{key: key, fires: 1, windowStart: now})
		return true, false
	}

	e := el.Value.(*churnEntry)
	c.order.MoveToFront(el)

	if now.Before(e.cooldownUntil) {
		return false, false
	}
	// Cooldown just elapsed: give the resource a clean slate. If it is still
	// churning it re-earns suppression within one window; if the controller
	// that was rewriting it has gone, its next real change is reported.
	if !e.cooldownUntil.IsZero() {
		e.cooldownUntil = time.Time{}
		e.fires = 1
		e.windowStart = now
		return true, false
	}

	if now.Sub(e.windowStart) > c.window {
		e.fires = 1
		e.windowStart = now
		return true, false
	}

	e.fires++
	if e.fires > c.threshold {
		e.cooldownUntil = now.Add(c.cooldown)
		return false, true
	}
	return true, false
}

// Len returns the current entry count. Test-only.
func (c *ChurnSuppressor) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
