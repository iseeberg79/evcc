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
		{"holdcharge action", map[string]batterySuggestion{"b": {Action: api.BatteryHoldCharge.String()}}, api.BatteryHoldCharge},
		{"hold action", map[string]batterySuggestion{"b": {Action: api.BatteryHold.String()}}, api.BatteryHold},
		{"charge action", map[string]batterySuggestion{"b": {Action: api.BatteryCharge.String()}}, api.BatteryCharge},
		{"normal action", map[string]batterySuggestion{"b": {Action: api.BatteryNormal.String()}}, api.BatteryNormal},
		{"empty action (default)", map[string]batterySuggestion{"b": {}}, api.BatteryNormal},
		{"holdcharge wins over charge", map[string]batterySuggestion{"a": {Action: api.BatteryCharge.String()}, "b": {Action: api.BatteryHoldCharge.String()}}, api.BatteryHoldCharge},
		{"hold wins over charge", map[string]batterySuggestion{"a": {Action: api.BatteryCharge.String()}, "b": {Action: api.BatteryHold.String()}}, api.BatteryHold},
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
