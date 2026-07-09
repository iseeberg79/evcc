package plugin

import (
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestWatchdogSetterConcurrency(t *testing.T) {
	p := &watchdogPlugin{
		log:     util.NewLogger("foo"),
		timeout: 10 * time.Nanosecond,
		clock:   clock.New(),
	}

	var u atomic.Uint32

	set := setter(p, func(i int) error {
		if !u.CompareAndSwap(0, 1) {
			return errors.New("race")
		}

		time.Sleep(time.Duration(rand.Int32N(int32(p.timeout))))

		if !u.CompareAndSwap(1, 0) {
			return errors.New("race")
		}

		return nil
	}, nil)

	var eg errgroup.Group

	for range 100 {
		eg.Go(func() error {
			return set(1)
		})
	}

	require.NoError(t, eg.Wait())
}

func TestWatchdogDeferredUpdate(t *testing.T) {
	// Test: Value 1 → 3 → 2 with delay
	// 1 → 3: delayed (target 3 is non-reset)
	// 3 → 2: delayed (target 2 is non-reset)
	// Expected: [1, <delay>, 3, <delay>, 2]

	timeout := 60 * time.Second
	c := clock.NewMock()
	p := &watchdogPlugin{
		log:      util.NewLogger("test"),
		timeout:  timeout,
		deferred: true,
		clock:    c,
	}

	var calls []int
	set := setter(p, func(i int) error {
		calls = append(calls, i)
		return nil
	}, []int{1}) // 1 is reset value

	// Value 1 (reset) → should set immediately
	require.NoError(t, set(1))
	require.Equal(t, []int{1}, calls)

	// Value 3 (target is non-reset) → should be delayed
	require.NoError(t, set(3))
	require.Equal(t, []int{1}, calls, "Value 3 should not be set yet")

	// Wait for delay
	expectedDelay := p.timeout + 5*time.Second
	c.Add(expectedDelay)

	// Now value 3 should be set
	require.Equal(t, []int{1, 3}, calls)

	// Value 2 (non-reset to non-reset) → should delay
	require.NoError(t, set(2))
	require.Equal(t, []int{1, 3}, calls, "Value 2 should not be set yet")

	// Wait for delay
	c.Add(expectedDelay)

	// Now value 2 should be set (exactly once)
	require.Equal(t, []int{1, 3, 2}, calls)
}

func TestWatchdogCancelPendingDeferredUpdate(t *testing.T) {
	// Test: Value 3 → 2 started, then set Value 1 during delay
	// Expected: Deferred update cancelled, Value 1 set immediately

	timeout := 60 * time.Second
	c := clock.NewMock()
	p := &watchdogPlugin{
		log:      util.NewLogger("test"),
		timeout:  timeout,
		deferred: true,
		clock:    c,
	}

	var calls []int
	set := setter(p, func(i int) error {
		calls = append(calls, i)
		return nil
	}, []int{1}) // 1 is reset value

	// Value 1 (reset) → establishes a known write time so the subsequent non-reset
	// writes below are deferred based on that, not the fresh-setter startup assumption
	require.NoError(t, set(1))
	require.Equal(t, []int{1}, calls)

	// Value 3 (non-reset) → deferred (no time has passed since the reset write)
	require.NoError(t, set(3))
	require.Equal(t, []int{1}, calls, "Value 3 should not be set yet")
	c.Add(timeout + 5*time.Second)
	require.Equal(t, []int{1, 3}, calls)

	// Value 2 (deferred update)
	require.NoError(t, set(2))
	require.Equal(t, []int{1, 3}, calls, "Value 2 should not be set yet")

	// Wait a bit but not the full delay
	c.Add(30 * time.Second)

	// Value 1 (reset) → should cancel pending deferred update and set immediately
	require.NoError(t, set(1))
	require.Equal(t, []int{1, 3, 1}, calls, "Value 1 should be set, Value 2 should be cancelled")

	// Wait for what would have been the original delay
	c.Add(timeout + 5*time.Second)

	// Value 2 should still not have been set
	require.Equal(t, []int{1, 3, 1}, calls, "Value 2 should remain cancelled")
}

func TestWatchdogFreshSetterAssumesRecentReset(t *testing.T) {
	// A restarted process loses lastUpdated in memory, even though the previous process may
	// have written a reset value (e.g. its shutdown hook) moments before exiting and the
	// device may still be settling from it. A brand new setter must therefore defer its
	// first non-reset write by a full timeout instead of treating "never written in this
	// process" as "ages ago, safe to write immediately" - see discussion on the Kostal
	// register 1034/1038 race after a restart.
	timeout := 60 * time.Second
	c := clock.NewMock()
	p := &watchdogPlugin{
		log:      util.NewLogger("test"),
		timeout:  timeout,
		deferred: true,
		clock:    c,
	}

	var calls []int
	set := setter(p, func(i int) error {
		calls = append(calls, i)
		return nil
	}, []int{1}) // 1 is reset value

	// first-ever call, non-reset target -> must not write immediately
	require.NoError(t, set(4))
	require.Empty(t, calls, "first write after (re)start must be deferred, not immediate")

	c.Add(timeout + 5*time.Second)
	require.Equal(t, []int{4}, calls)
}

func TestWatchdogFreshSetterResetIsImmediate(t *testing.T) {
	// switching to the reset value itself must never be deferred, even on a brand new setter
	p := &watchdogPlugin{
		log:      util.NewLogger("test"),
		timeout:  60 * time.Second,
		deferred: true,
		clock:    clock.NewMock(),
	}

	var calls []int
	set := setter(p, func(i int) error {
		calls = append(calls, i)
		return nil
	}, []int{1})

	require.NoError(t, set(1))
	require.Equal(t, []int{1}, calls)
}

func TestWatchdogDelayBackwardCompatibility(t *testing.T) {
	// Test: deferred=false behaves like old implementation
	// Expected: All updates immediate

	p := &watchdogPlugin{
		log:      util.NewLogger("test"),
		timeout:  60 * time.Second,
		deferred: false, // explicitly false
		clock:    clock.New(),
	}

	var calls []int
	set := setter(p, func(i int) error {
		calls = append(calls, i)
		return nil
	}, []int{1}) // 1 is reset value

	// All updates should be immediate
	require.NoError(t, set(1))
	require.Equal(t, []int{1}, calls)

	require.NoError(t, set(3))
	require.Equal(t, []int{1, 3}, calls)

	require.NoError(t, set(2))
	require.Equal(t, []int{1, 3, 2}, calls, "Value 2 should be set immediately (no delay)")

	require.NoError(t, set(4))
	require.Equal(t, []int{1, 3, 2, 4}, calls, "Value 4 should be set immediately (no delay)")
}

func TestWatchdogResetStopsInflightTick(t *testing.T) {
	// Switching to the reset value must stop the watchdog: an in-flight tick
	// must not re-assert the old value after the reset value was written.
	for iter := range 200 {
		p := &watchdogPlugin{
			log:     util.NewLogger("test"),
			timeout: 2 * time.Millisecond,
			clock:   clock.New(),
		}

		var sawReset, stale atomic.Bool

		set := setter(p, func(v int) error {
			if v == 1 {
				sawReset.Store(true)
				// hold the lock so an in-flight tick queues behind the reset
				time.Sleep(2 * time.Millisecond)
			} else if sawReset.Load() {
				stale.Store(true)
			}
			return nil
		}, []int{1})

		require.NoError(t, set(2)) // non-reset: starts the watchdog
		time.Sleep(3 * time.Millisecond)

		require.NoError(t, set(1)) // reset: must stop the watchdog
		time.Sleep(5 * time.Millisecond)

		require.Falsef(t, stale.Load(), "iter %d: watchdog re-asserted old value after reset", iter)
	}
}
