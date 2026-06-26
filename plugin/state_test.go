package plugin

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/require"
)

func setCache(t *testing.T, key string, val any) {
	t.Helper()
	c := util.NewParamCache()
	c.Add(key, util.Param{Key: key, Val: val})
	util.SetDefaultParamCache(c)
	t.Cleanup(func() { util.SetDefaultParamCache(nil) })
}

func TestStateGetter(t *testing.T) {
	setCache(t, "batteryHoldChargePower", []int64{0, 1500, 3000})

	for _, tc := range []struct {
		index int
		scale float64
		want  float64
	}{
		{0, 1, 0},
		{1, 1, 1500},
		{2, 1, 3000},
		{3, 1, 0},       // out of range
		{1, 0.001, 1.5}, // scale
	} {
		other := map[string]any{"key": "batteryHoldChargePower", "index": tc.index}
		if tc.scale != 1 {
			other["scale"] = tc.scale
		}
		p, err := NewStateFromConfig(t.Context(), other)
		require.NoError(t, err)

		g, err := p.(FloatGetter).FloatGetter()
		require.NoError(t, err)
		v, err := g()
		require.NoError(t, err)
		require.Equal(t, tc.want, v, "index %d scale %v", tc.index, tc.scale)
	}
}

func TestStateGetterMissingKey(t *testing.T) {
	util.SetDefaultParamCache(util.NewParamCache())
	t.Cleanup(func() { util.SetDefaultParamCache(nil) })

	p, err := NewStateFromConfig(t.Context(), map[string]any{"key": "doesNotExist", "index": 0})
	require.NoError(t, err)
	g, err := p.(FloatGetter).FloatGetter()
	require.NoError(t, err)
	v, err := g()
	require.NoError(t, err)
	require.Equal(t, 0.0, v)
}

// TestStateIntSetterForward verifies the nested-setter path: the incoming int is
// ignored, the indexed cached value is read and forwarded to the nested setter.
func TestStateIntSetterForward(t *testing.T) {
	setCache(t, "batteryHoldChargePower", []int64{0, 1500})

	var mu sync.Mutex
	var written string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		written = r.URL.Query().Get("v")
		mu.Unlock()
	}))
	defer srv.Close()

	p, err := NewStateFromConfig(t.Context(), map[string]any{
		"key":   "batteryHoldChargePower",
		"index": 1,
		"set":   map[string]any{"source": "http", "uri": srv.URL + "/write?v={{.foo}}"},
	})
	require.NoError(t, err)

	set, err := p.(IntSetter).IntSetter("foo")
	require.NoError(t, err)

	// incoming value (99) is ignored; cached value at index 1 (1500) is forwarded
	require.NoError(t, set(99))

	mu.Lock()
	got, _ := strconv.ParseFloat(written, 64)
	mu.Unlock()
	require.Equal(t, 1500.0, got)
}
