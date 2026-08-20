package modbus

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRegisterTTL = 2 * time.Second

func TestRegisterCacheGetRangeMiss(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)
	_, ok := c.getRange(1, 3, 10, 2)
	assert.False(t, ok)
}

// TestRegisterCacheServesSubsetFromLargerRead verifies a smaller request
// within an already-cached larger range is served without its own load.
func TestRegisterCacheServesSubsetFromLargerRead(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	var calls atomic.Int32
	load := func(payload []byte) func() ([]byte, error) {
		return func() ([]byte, error) {
			calls.Add(1)
			return payload, nil
		}
	}

	full := []byte{1, 1, 2, 2, 3, 3, 4, 4} // registers 10-13
	got, hit, err := c.Fetch(1, 3, 10, 4, load(full))
	require.NoError(t, err)
	assert.False(t, hit)
	assert.Equal(t, full, got)

	// a subset, different start address and quantity, must not load again
	got, hit, err = c.Fetch(1, 3, 11, 2, func() ([]byte, error) {
		t.Fatal("must not load - already covered by the larger cached read")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, hit)
	assert.Equal(t, []byte{2, 2, 3, 3}, got)
	assert.Equal(t, int32(1), calls.Load())
}

// TestRegisterCacheServesUnionOfSeparateReads verifies a request is served
// from cache once its registers have all been seen, even if they arrived
// via two unrelated prior reads rather than one covering read.
func TestRegisterCacheServesUnionOfSeparateReads(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	_, hit, err := c.Fetch(1, 3, 10, 2, func() ([]byte, error) { return []byte{1, 1, 2, 2}, nil })
	require.NoError(t, err)
	assert.False(t, hit)

	_, hit, err = c.Fetch(1, 3, 12, 2, func() ([]byte, error) { return []byte{3, 3, 4, 4}, nil })
	require.NoError(t, err)
	assert.False(t, hit)

	// spans both prior reads - neither alone covers it
	got, hit, err := c.Fetch(1, 3, 10, 4, func() ([]byte, error) {
		t.Fatal("must not load - union of two prior reads already covers this range")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, hit)
	assert.Equal(t, []byte{1, 1, 2, 2, 3, 3, 4, 4}, got)
}

// TestRegisterCacheDistinguishesFunctionCodeAndUnit verifies holding vs
// input registers, and different unit ids, are cached independently even at
// the same address.
func TestRegisterCacheDistinguishesFunctionCodeAndUnit(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	_, _, err := c.Fetch(1, 3, 10, 1, func() ([]byte, error) { return []byte{0xAA, 0xAA}, nil })
	require.NoError(t, err)

	var calls atomic.Int32
	countingLoad := func(payload []byte) func() ([]byte, error) {
		return func() ([]byte, error) {
			calls.Add(1)
			return payload, nil
		}
	}

	_, hit, err := c.Fetch(1, 4, 10, 1, countingLoad([]byte{0xBB, 0xBB})) // same addr, input instead of holding
	require.NoError(t, err)
	assert.False(t, hit)

	_, hit, err = c.Fetch(2, 3, 10, 1, countingLoad([]byte{0xCC, 0xCC})) // same addr/code, different unit
	require.NoError(t, err)
	assert.False(t, hit)

	assert.Equal(t, int32(2), calls.Load())
}

func TestRegisterCacheExpires(t *testing.T) {
	c := NewRegisterCache(10 * time.Millisecond)

	_, _, err := c.Fetch(1, 3, 10, 1, func() ([]byte, error) { return []byte{1, 2}, nil })
	require.NoError(t, err)

	_, ok := c.getRange(1, 3, 10, 1)
	require.True(t, ok, "must be fresh immediately after load")

	time.Sleep(20 * time.Millisecond)

	_, ok = c.getRange(1, 3, 10, 1)
	assert.False(t, ok, "expired entry must not be served")
}

// TestRegisterCacheInvalidateIsTargeted verifies Invalidate drops only the
// registers it names, leaving unrelated ones cached.
func TestRegisterCacheInvalidateIsTargeted(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	_, _, err := c.Fetch(1, 3, 10, 2, func() ([]byte, error) { return []byte{1, 1, 2, 2}, nil })
	require.NoError(t, err)

	c.Invalidate(1, 3, 10, 1) // only register 10

	_, ok := c.getRange(1, 3, 10, 1)
	assert.False(t, ok, "invalidated register must be dropped")
	_, ok = c.getRange(1, 3, 11, 1)
	assert.True(t, ok, "untouched neighbor must remain cached")
}

// TestRegisterCacheInvalidateDuringLoadPreventsStaleResurrection is the
// generation-counter race guard: a load already past its own cache lookup
// when a write invalidates and overwrites elsewhere must not clobber the
// newer value once it (belatedly) finishes.
func TestRegisterCacheInvalidateDuringLoadPreventsStaleResurrection(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	// an existing, genuinely cached value - not an empty cache
	_, _, err := c.Fetch(1, 3, 10, 1, func() ([]byte, error) { return []byte{0xAA, 0xAA}, nil })
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	staleLoad := func() ([]byte, error) {
		close(entered)
		<-release // hold this load open while a write replaces the entry it's about to overwrite
		return []byte{0xAA, 0xAA}, nil
	}

	// force a miss so the stale load actually starts (a fresh cache entry
	// would short-circuit Fetch before load ever runs)
	c.Invalidate(1, 3, 10, 1)

	var wg sync.WaitGroup
	var got []byte
	var fetchErr error
	wg.Go(func() {
		got, _, fetchErr = c.Fetch(1, 3, 10, 1, staleLoad)
	})
	<-entered

	// the actual write: invalidates again (the stale load's captured
	// generation is now two generations behind) and lands a genuinely
	// newer value
	c.Invalidate(1, 3, 10, 1)
	_, _, err = c.Fetch(1, 3, 10, 1, func() ([]byte, error) { return []byte{0xCC, 0xCC}, nil })
	require.NoError(t, err)

	close(release)
	wg.Wait()

	require.NoError(t, fetchErr)
	assert.Equal(t, []byte{0xAA, 0xAA}, got, "the caller still gets its own correct (if stale) result")

	cached, ok := c.getRange(1, 3, 10, 1)
	require.True(t, ok)
	assert.Equal(t, []byte{0xCC, 0xCC}, cached, "the belated stale load must not have clobbered the newer write's value")
}

// TestRegisterCacheInvalidateClosesJoinWindow verifies a request starting
// after Invalidate never joins a flight that started before it: without
// this, singleflight would hand it the pre-write leader's result directly,
// bypassing putRange's generation check entirely (that check only guards
// what gets cached, not what Fetch returns to a joiner).
func TestRegisterCacheInvalidateClosesJoinWindow(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	entered := make(chan struct{})
	release := make(chan struct{})
	leaderLoad := func() ([]byte, error) {
		close(entered)
		<-release // hold the pre-write leader open past the write below
		return []byte{0xAA, 0xAA}, nil
	}

	var wg sync.WaitGroup
	var leaderGot []byte
	wg.Go(func() {
		leaderGot, _, _ = c.Fetch(1, 3, 10, 1, leaderLoad)
	})
	<-entered // leader is in flight, using the pre-write generation

	c.Invalidate(1, 3, 10, 1) // the write

	// a request starting after the write must not join the pre-write flight
	var joinerGot []byte
	var joinerErr error
	wg.Go(func() {
		joinerGot, _, joinerErr = c.Fetch(1, 3, 10, 1, func() ([]byte, error) { return []byte{0xCC, 0xCC}, nil })
	})

	close(release)
	wg.Wait()

	require.NoError(t, joinerErr)
	assert.Equal(t, []byte{0xAA, 0xAA}, leaderGot)
	assert.Equal(t, []byte{0xCC, 0xCC}, joinerGot, "a request starting after the write must never see the pre-write value")
}

// TestRegisterCachePutRangeKeepsFresherOnOverlap verifies that when two
// differently-shaped reads overlap and race the device with no write
// between them, the one whose load actually started later wins the
// overlapping registers - even if it happens to return first - so a slower,
// older read that lands afterwards can't clobber it.
func TestRegisterCachePutRangeKeepsFresherOnOverlap(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	olderEntered := make(chan struct{})
	olderRelease := make(chan struct{})
	older := func() ([]byte, error) {
		close(olderEntered)
		<-olderRelease
		return []byte{1, 1, 1, 1, 1, 1, 1, 1}, nil // registers 10-13, stale values
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _, err := c.Fetch(1, 3, 10, 4, older)
		assert.NoError(t, err)
	})
	<-olderEntered // older load is in flight, started first

	// a newer, narrower read for the overlapping registers 11-12 completes
	// while the older one is still in flight
	_, _, err := c.Fetch(1, 3, 11, 2, func() ([]byte, error) { return []byte{9, 9, 9, 9}, nil })
	require.NoError(t, err)

	got, ok := c.getRange(1, 3, 11, 2)
	require.True(t, ok)
	require.Equal(t, []byte{9, 9, 9, 9}, got, "sanity: the newer read is cached before the older one lands")

	close(olderRelease)
	wg.Wait()

	got, ok = c.getRange(1, 3, 11, 2)
	require.True(t, ok)
	assert.Equal(t, []byte{9, 9, 9, 9}, got, "the belated older read must not overwrite the newer value already cached for the overlapping registers")
}

// TestRegisterCacheFetchSingleFlight verifies concurrent identical requests
// collapse into a single load.
func TestRegisterCacheFetchSingleFlight(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)

	var calls atomic.Int32
	var once sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	load := func() ([]byte, error) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		return []byte{1, 2, 3, 4}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	payloads := make([][]byte, n)

	wg.Go(func() {
		got, _, err := c.Fetch(1, 3, 10, 2, load)
		assert.NoError(t, err)
		payloads[0] = got
	})

	<-entered

	for i := 1; i < n; i++ {
		wg.Go(func() {
			got, _, err := c.Fetch(1, 3, 10, 2, load)
			assert.NoError(t, err)
			payloads[i] = got
		})
	}

	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load())
	for i := range n {
		assert.Equal(t, []byte{1, 2, 3, 4}, payloads[i])
	}
}

// TestRegisterCacheAdaptiveTTLGrowsOnRepeatedValue verifies a register whose
// value keeps coming back unchanged earns a longer ttl each time, capped at
// registerMaxTTL - see putRange.
func TestRegisterCacheAdaptiveTTLGrowsOnRepeatedValue(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)
	key := registerKey{1, 3, 10}
	value := []byte{9, 9}

	start := time.Now()
	c.putRange(1, 3, 10, value, 0, start)
	assert.Equal(t, testRegisterTTL, c.data[key].ttl, "first load: base ttl")

	start = start.Add(time.Millisecond)
	c.putRange(1, 3, 10, value, 0, start)
	want := time.Duration(float64(testRegisterTTL) * registerGrowthFactor)
	assert.Equal(t, want, c.data[key].ttl, "unchanged value: ttl grows by the factor")

	// many more confirmations must never exceed the cap
	for range 20 {
		start = start.Add(time.Millisecond)
		c.putRange(1, 3, 10, value, 0, start)
	}
	assert.Equal(t, registerMaxTTL, c.data[key].ttl, "capped at registerMaxTTL")
}

// TestRegisterCacheAdaptiveTTLResetsOnChangedValue verifies a grown ttl falls
// back to the base ttl as soon as the register's value actually changes.
func TestRegisterCacheAdaptiveTTLResetsOnChangedValue(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)
	key := registerKey{1, 3, 10}

	start := time.Now()
	c.putRange(1, 3, 10, []byte{9, 9}, 0, start)
	start = start.Add(time.Millisecond)
	c.putRange(1, 3, 10, []byte{9, 9}, 0, start)
	require.Greater(t, c.data[key].ttl, testRegisterTTL, "sanity: grown past the base ttl")

	start = start.Add(time.Millisecond)
	c.putRange(1, 3, 10, []byte{7, 7}, 0, start) // value actually changes
	assert.Equal(t, testRegisterTTL, c.data[key].ttl, "changed value resets to the base ttl")
}

// TestRegisterCacheAdaptiveTTLResetsAfterInvalidate verifies a write - which
// deletes the entry outright, see Invalidate - drops the grown ttl along
// with it: the next load starts back at the base ttl, not wherever the
// deleted entry's growth had reached.
func TestRegisterCacheAdaptiveTTLResetsAfterInvalidate(t *testing.T) {
	c := NewRegisterCache(testRegisterTTL)
	key := registerKey{1, 3, 10}
	value := []byte{9, 9}

	start := time.Now()
	c.putRange(1, 3, 10, value, 0, start)
	start = start.Add(time.Millisecond)
	c.putRange(1, 3, 10, value, 0, start)
	require.Greater(t, c.data[key].ttl, testRegisterTTL, "sanity: grown past the base ttl")

	c.Invalidate(1, 3, 10, 1)

	start = start.Add(time.Millisecond)
	c.putRange(1, 3, 10, value, 1, start) // gen bumped to 1 by Invalidate
	assert.Equal(t, testRegisterTTL, c.data[key].ttl, "same value again, but after invalidation: back to the base ttl")
}
