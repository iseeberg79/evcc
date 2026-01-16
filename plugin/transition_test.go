package plugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mockIntSetter records all calls to the setter
type mockIntSetter struct {
	mu     sync.Mutex
	calls  []int64
	delays []time.Duration
	last   time.Time
}

func (m *mockIntSetter) record(val int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if !m.last.IsZero() {
		m.delays = append(m.delays, now.Sub(m.last))
	}
	m.last = now
	m.calls = append(m.calls, val)
}

func (m *mockIntSetter) getCalls() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.calls...)
}

func (m *mockIntSetter) getDelays() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Duration(nil), m.delays...)
}

func (m *mockIntSetter) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
	m.delays = nil
	m.last = time.Time{}
}

// mockPlugin implements IntSetter interface
type mockPlugin struct {
	mock *mockIntSetter
}

func (p *mockPlugin) IntSetter(param string) (func(int64) error, error) {
	return func(val int64) error {
		p.mock.record(val)
		return nil
	}, nil
}

// newMockPlugin factory for mock plugin
func newMockPlugin(ctx context.Context, other map[string]any) (Plugin, error) {
	mock, ok := other["_mock"].(*mockIntSetter)
	if !ok {
		mock = &mockIntSetter{}
	}
	return &mockPlugin{mock: mock}, nil
}

func init() {
	registry.AddCtx("mock", newMockPlugin)
}

func TestTransitionNoTimeout(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	// No timeout configured - all transitions should be immediate
	other := map[string]any{
		"set": Config{
			Source: "mock",
			Other: map[string]any{
				"_mock": mock,
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok, "plugin should implement IntSetter")

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// All transitions should be immediate
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, mock.getCalls())

	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 2, 3}, mock.getCalls())
}

func TestTransitionWithWatchdogResetModes(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	// Simulate Kostal config: timeout + reset mode 1
	other := map[string]any{
		"timeout": "100ms",
		"reset":   1, // mode 1 = normal (no delay when leaving)
		"set": Config{
			Source: "mock",
			Other: map[string]any{
				"_mock": mock,
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok)

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// Mode 1 (normal/reset) - immediate (first call)
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	// Mode 3 (charge/non-reset) - DELAYED (to non-reset mode)
	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls(), "should not apply yet")

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 3}, mock.getCalls(), "should apply after delay")

	// Mode 2 (hold/non-reset) - DELAYED (to non-reset mode)
	// First applies reset mode to stop watchdog, then waits, then applies mode 2
	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1}, mock.getCalls(), "should apply reset mode to stop watchdog")

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 3, 1, 2}, mock.getCalls(), "should apply mode 2 after delay")

	// Mode 1 (normal/reset) - IMMEDIATE (to reset mode)
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1, 2, 1}, mock.getCalls(), "immediate to reset mode")

	// Mode 3 (charge/non-reset) - DELAYED (to non-reset mode)
	// Already in reset mode (1), so no additional reset needed
	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1, 2, 1}, mock.getCalls(), "should not apply yet")

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 3, 1, 2, 1, 3}, mock.getCalls(), "should apply after delay")
}

func TestTransitionSimpleTimeout(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	// Simple timeout without watchdog - should delay ALL transitions
	other := map[string]any{
		"timeout": "100ms",
		"set": Config{
			Source: "mock",
			Other: map[string]any{
				"_mock": mock,
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok)

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// First call - immediate
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	// All subsequent transitions should delay
	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 2}, mock.getCalls())

	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, mock.getCalls())

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 2, 3}, mock.getCalls())
}

func TestTransitionCancellation(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	other := map[string]any{
		"timeout": "200ms",
		"reset":   1,
		"set": Config{
			Source: "mock",
			Other: map[string]any{
				"_mock": mock,
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok)

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// Mode 1 (reset) - immediate (first call)
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	// Mode 3 (non-reset) - delayed (to non-reset)
	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	time.Sleep(250 * time.Millisecond)
	assert.Equal(t, []int64{1, 3}, mock.getCalls())

	// Start transition 3->2 (should delay - to non-reset)
	// First applies reset to stop watchdog
	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1}, mock.getCalls(), "applies reset to stop watchdog")

	time.Sleep(50 * time.Millisecond)

	// Cancel by requesting mode 1 (to reset - should be immediate)
	// Since we're already in reset mode (from previous step), just apply mode 1
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1, 1}, mock.getCalls(), "immediate to reset mode")

	// Mode 2 should never have been applied (transition was cancelled)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, []int64{1, 3, 1, 1}, mock.getCalls(), "mode 2 was cancelled")
}

func TestTransitionSkipsWritesDuringDelay(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	other := map[string]any{
		"timeout": "200ms",
		"reset":   1,
		"set": Config{
			Source: "mock",
			Other: map[string]any{
				"_mock": mock,
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok)

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// Mode 1 (reset, first call) - immediate
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	// Mode 3 (non-reset) - should delay
	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	time.Sleep(250 * time.Millisecond)
	assert.Equal(t, []int64{1, 3}, mock.getCalls())

	// Request mode 2 - should start delay (to non-reset)
	// First applies reset mode to stop watchdog
	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1}, mock.getCalls(), "applies reset to stop watchdog")

	// Request mode 2 again during delay - should skip (already pending)
	time.Sleep(50 * time.Millisecond)
	err = setter(2)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1}, mock.getCalls(), "should skip redundant call")

	// Wait for transition to complete
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, []int64{1, 3, 1, 2}, mock.getCalls())
}

func TestTransitionAutoDetectFromWatchdog(t *testing.T) {
	ctx := context.Background()
	mock := &mockIntSetter{}

	// Test auto-detection of reset mode from nested watchdog config
	other := map[string]any{
		"timeout": "100ms",
		// No explicit reset - should auto-detect from watchdog
		"set": Config{
			Source: "watchdog",
			Other: map[string]any{
				"reset": 1, // Should be auto-detected
				"set": Config{
					Source: "mock",
					Other: map[string]any{
						"_mock": mock,
					},
				},
			},
		},
	}

	p, err := NewTransitionFromConfig(ctx, other)
	assert.NoError(t, err)

	intSetter, ok := p.(IntSetter)
	assert.True(t, ok)

	setter, err := intSetter.IntSetter("")
	assert.NoError(t, err)

	// Mode 1 (reset) - immediate (first call)
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	// Mode 3 (non-reset) - delayed (to non-reset)
	err = setter(3)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1}, mock.getCalls())

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, []int64{1, 3}, mock.getCalls())

	// Mode 1 (reset) - immediate (to reset mode)
	err = setter(1)
	assert.NoError(t, err)
	assert.Equal(t, []int64{1, 3, 1}, mock.getCalls())
}
