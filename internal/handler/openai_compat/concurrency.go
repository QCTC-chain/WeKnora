package openai_compat

import (
	"sync"
)

// perKeyConcurrency caps simultaneous gateway turns per API key (docs §9):
// a keyed counting semaphore. Over-limit callers answer 429
// concurrency_limit immediately — no queueing, so a burst of clients sees
// predictable backpressure instead of pile-up latency.
//
// The counters live in process memory. A multi-replica deployment therefore
// enforces the cap per replica; true cluster-wide accounting is a Redis
// concern deferred until the gateway actually ships multi-replica.
type perKeyConcurrency struct {
	mu    sync.Mutex
	slots map[uint64]*keySlots
}

type keySlots struct {
	held int
}

func newPerKeyConcurrency() *perKeyConcurrency {
	return &perKeyConcurrency{slots: make(map[uint64]*keySlots)}
}

// tryAcquire takes one slot for key, bounded by max. max <= 0 disables the
// limit. The returned release func is always non-nil and safe to call once.
func (p *perKeyConcurrency) tryAcquire(key uint64, max int) (release func()) {
	if max <= 0 || key == 0 {
		return func() {}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	slots, ok := p.slots[key]
	if !ok {
		slots = &keySlots{}
		p.slots[key] = slots
	}
	if slots.held >= max {
		return nil
	}
	slots.held++
	released := false
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if released {
			return
		}
		released = true
		slots.held--
		if slots.held <= 0 {
			// Drop the entry so idle keys do not accumulate.
			if cur, ok := p.slots[key]; ok && cur == slots {
				delete(p.slots, key)
			}
		}
	}
}

// held reports the current slot count for a key (test introspection).
func (p *perKeyConcurrency) held(key uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if slots, ok := p.slots[key]; ok {
		return slots.held
	}
	return 0
}
