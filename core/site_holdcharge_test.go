package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/require"
)

func TestHoldChargeMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		suggestions map[string]batterySuggestion
		want        api.BatteryMode
	}{
		{"charge -> holdcharge", map[string]batterySuggestion{"b": {Charge: 1500}}, api.BatteryHoldCharge},
		{"discharge -> normal", map[string]batterySuggestion{"b": {Discharge: 800}}, api.BatteryNormal},
		{"idle -> hold", map[string]batterySuggestion{"b": {}}, api.BatteryHold},
		{"below threshold -> hold", map[string]batterySuggestion{"b": {Charge: 20, Discharge: 10}}, api.BatteryHold},
		{"charge wins over discharge", map[string]batterySuggestion{"a": {Charge: 1000}, "b": {Discharge: 500}}, api.BatteryHoldCharge},
	} {
		site := &Site{holdChargeSuggestions: tc.suggestions}
		require.Equal(t, tc.want, site.holdChargeMode(), tc.name)
	}
}

func TestHoldChargePlanAvailable(t *testing.T) {
	site := &Site{}
	require.False(t, site.holdChargePlanAvailable(), "no plan")

	site.holdChargeSuggestions = map[string]batterySuggestion{"b": {Charge: 1000}}
	site.holdChargeUpdated = time.Now()
	require.True(t, site.holdChargePlanAvailable(), "fresh plan")

	site.holdChargeUpdated = time.Now().Add(-2 * holdChargeStale)
	require.False(t, site.holdChargePlanAvailable(), "stale plan")
}
