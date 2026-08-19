package modbus

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// RegisterCache caches individual holding/input register values, addressed
// by (unitId, function code, address), instead of a whole requested range at
// once. A read is served from cache as soon as every register it covers is
// already known and fresh - even if no single prior read covered exactly
// this range, only the union of several - not just on an exact repeat of an
// earlier request. A write invalidates only the registers it touched.
type RegisterCache struct {
	ttl time.Duration

	mu   sync.Mutex
	data map[registerKey]registerEntry

	// generation is bumped by Invalidate and captured by Fetch before a
	// load starts, so a load already past its own cache lookup when a write
	// elsewhere invalidates can't resurrect its now-stale result into the
	// cache afterwards - see putRange. One counter for the whole cache
	// rather than per-key: simpler, and a write is rare enough that briefly
	// discarding other in-flight loads' results too costs nothing but an
	// extra physical read next time they're needed.
	generation atomic.Int64

	flight singleflight.Group // keyed by the full requested range, see Fetch
}

type registerKey struct {
	unitId uint8
	code   byte // Modbus function code: 3 = holding, 4 = input
	addr   uint16
}

type registerEntry struct {
	value     [2]byte
	expiresAt time.Time
}

// NewRegisterCache returns a RegisterCache that holds entries for ttl.
func NewRegisterCache(ttl time.Duration) *RegisterCache {
	return &RegisterCache{ttl: ttl, data: make(map[registerKey]registerEntry)}
}

// Fetch returns qty registers starting at addr for (unitId, code) if every
// one of them is already cached and fresh. On a miss, load is invoked
// exactly once across all concurrent callers requesting the exact same
// range *against the same generation* (see the flight key below), and its
// result is decomposed into per-register entries so a later,
// differently-shaped request covering some of the same registers can be
// served without its own physical read. The bool return is true whenever
// the caller was spared its own physical load - not singleflight's own
// "shared" value, which is also true for the leader once a follower joins.
func (c *RegisterCache) Fetch(unitId uint8, code byte, addr, qty uint16, load func() ([]byte, error)) ([]byte, bool, error) {
	if payload, ok := c.getRange(unitId, code, addr, qty); ok {
		return payload, true, nil
	}

	gen := c.generation.Load()
	// gen is part of the key, not just checked in putRange: without it, a
	// request that starts right after Invalidate could still join a flight
	// that started before it and is racing an in-flight write, and would
	// then be handed that leader's pre-write payload directly - correct
	// payload, wrong answer, and putRange's generation check can't help
	// here since it only guards what gets cached, not what Fetch returns.
	reqKey := fmt.Sprintf("%d/%d/%d/%d/%d", gen, unitId, code, addr, qty)

	var loaded bool
	var start time.Time
	v, err, _ := c.flight.Do(reqKey, func() (any, error) {
		// re-check under the flight: a prior flight may have populated the
		// cache between our miss above and acquiring the call.
		if payload, ok := c.getRange(unitId, code, addr, qty); ok {
			return payload, nil
		}
		loaded = true
		start = time.Now()
		payload, err := load()
		if err != nil {
			return nil, err
		}
		c.putRange(unitId, code, addr, payload, gen, start.Add(c.ttl))
		return payload, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.([]byte), !loaded, nil
}

// getRange returns qty registers starting at addr if every one is cached
// and fresh, concatenated in order.
func (c *RegisterCache) getRange(unitId uint8, code byte, addr, qty uint16) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	payload := make([]byte, 0, int(qty)*2)
	// uint32 end keeps the loop bound itself from wrapping; addr+qty past
	// 0xFFFF would still alias back into register 0 via the uint16(a) cast
	// below. Unreachable in practice - Modbus caps quantity at 125, so
	// addr+qty > 0x10000 is an illegal request mbserver rejects before it
	// reaches here - so not guarded explicitly.
	end := uint32(addr) + uint32(qty)
	for a := uint32(addr); a < end; a++ {
		key := registerKey{unitId, code, uint16(a)}
		e, ok := c.data[key]
		if !ok || now.After(e.expiresAt) {
			return nil, false
		}
		payload = append(payload, e.value[0], e.value[1])
	}
	return payload, true
}

// putRange decomposes payload (qty registers starting at addr) into
// per-register entries, unless a write has invalidated the cache since gen
// was captured at the start of the load that produced payload - in that
// case payload is still handed back to Fetch's caller as the correct answer
// to their specific request, just not cached, so it can't resurrect a value
// a concurrent write just invalidated.
//
// expiresAt is derived from when the load that produced payload started,
// not from now - two overlapping-but-different loads race the device, not
// necessarily in start order, and whichever's result is actually older must
// not evict a register a faster, later-started load already wrote a fresher
// value for. Given a constant ttl, "starts later" and "expires later" are
// the same comparison, so an entry only gets replaced by one with a strictly
// later expiresAt.
func (c *RegisterCache) putRange(unitId uint8, code byte, addr uint16, payload []byte, gen int64, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.generation.Load() != gen {
		return
	}

	for i := 0; 2*i+1 < len(payload); i++ {
		key := registerKey{unitId, code, addr + uint16(i)}
		if e, ok := c.data[key]; ok && e.expiresAt.After(expiresAt) {
			continue
		}
		c.data[key] = registerEntry{value: [2]byte{payload[2*i], payload[2*i+1]}, expiresAt: expiresAt}
	}
}

// Invalidate drops qty cached registers starting at addr for (unitId, code)
// and bumps the generation, so a load already in flight for a range
// covering any of them can't land a stale result in the cache after this
// returns - see putRange.
func (c *RegisterCache) Invalidate(unitId uint8, code byte, addr, qty uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()

	end := uint32(addr) + uint32(qty)
	for a := uint32(addr); a < end; a++ {
		delete(c.data, registerKey{unitId, code, uint16(a)})
	}
	c.generation.Add(1)
}

// Clear drops every cached register and bumps the generation, same as
// Invalidate but for the whole cache - a device may let a write to one
// function code's address space change what a different one reports (a
// coil toggling a mode that shows up in a status register, say), which
// Invalidate's function-code-scoped targeting can't know about.
func (c *RegisterCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.data)
	c.generation.Add(1)
}
