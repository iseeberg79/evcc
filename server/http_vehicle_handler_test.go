package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanStrategyHandlerSetter(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected api.PlanStrategy
	}{
		{
			name: "with preconditionEnforced true",
			body: `{"continuous":false,"precondition":900,"preconditionEnforced":true}`,
			expected: api.PlanStrategy{
				Continuous:           false,
				Precondition:         900 * time.Second,
				PreconditionEnforced: true,
			},
		},
		{
			name: "with preconditionEnforced false",
			body: `{"continuous":true,"precondition":1800,"preconditionEnforced":false}`,
			expected: api.PlanStrategy{
				Continuous:           true,
				Precondition:         1800 * time.Second,
				PreconditionEnforced: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")

			var result api.PlanStrategy
			err := planStrategyHandlerSetter(req, func(ps api.PlanStrategy) error {
				result = ps
				return nil
			})

			require.NoError(t, err)
			assert.Equal(t, tt.expected.Continuous, result.Continuous)
			assert.Equal(t, tt.expected.Precondition, result.Precondition)
			assert.Equal(t, tt.expected.PreconditionEnforced, result.PreconditionEnforced)
		})
	}
}
