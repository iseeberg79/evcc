package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMedianOf(t *testing.T) {
	t.Run("too few samples", func(t *testing.T) {
		v, ok := medianOf([]float64{1, 2, 3}, 5)
		assert.False(t, ok)
		assert.Equal(t, 1.0, v)
	})

	t.Run("odd count", func(t *testing.T) {
		v, ok := medianOf([]float64{3, 1, 2, 5, 4}, 3)
		assert.True(t, ok)
		assert.Equal(t, 3.0, v)
	})

	t.Run("even count averages the middle two", func(t *testing.T) {
		v, ok := medianOf([]float64{4, 1, 3, 2}, 3)
		assert.True(t, ok)
		assert.Equal(t, 2.5, v)
	})

	t.Run("robust against a single outlier", func(t *testing.T) {
		// one huge day does not move the median the way a mean would
		v, _ := medianOf([]float64{1, 1, 1, 1, 1, 1, 20}, 3)
		assert.Equal(t, 1.0, v)
	})
}

func TestForecastRates(t *testing.T) {
	start := time.Unix(1735689600, 0)

	for _, tc := range []struct {
		desc  string
		rates api.Rates
		want  string
	}{
		{desc: "nil", rates: nil, want: "null"},
		{desc: "empty", rates: api.Rates{}, want: "null"},
		{
			desc: "slots",
			rates: api.Rates{
				{Start: start, End: start.Add(time.Hour), Value: 0.25},
				{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour), Value: -0.1},
			},
			want: "[[1735689600,1735693200,0.25],[1735693200,1735696800,-0.1]]",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			b, err := json.Marshal(forecastRates(tc.rates))
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))
		})
	}
}

func TestTimeseriesMarshal(t *testing.T) {
	start := time.Unix(1735689600, 0)

	for _, tc := range []struct {
		desc string
		ts   timeseries
		want string
	}{
		{desc: "nil", ts: nil, want: "null"},
		{desc: "empty", ts: timeseries{}, want: "[]"},
		{
			desc: "entries",
			ts: timeseries{
				{Timestamp: start, Value: 1000},
				{Timestamp: start.Add(time.Hour), Value: 0},
			},
			want: "[[1735689600,1000],[1735693200,0]]",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			b, err := json.Marshal(tc.ts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))

			b, err = tc.ts.MarshalBytes()
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(b))
		})
	}
}
