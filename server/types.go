package server

import (
	"time"

	"github.com/evcc-io/evcc/api"
)

type planStrategyPayload struct {
	Continuous           bool  `json:"continuous"`
	Precondition         int64 `json:"precondition"`
	PreconditionEnforced bool  `json:"preconditionEnforced"`
}

func planStrategyPayloadFromApi(ps api.PlanStrategy) planStrategyPayload {
	return planStrategyPayload{
		Continuous:           ps.Continuous,
		Precondition:         int64(ps.Precondition.Seconds()),
		PreconditionEnforced: ps.PreconditionEnforced,
	}
}

type planGoal[T any] struct {
	Time  time.Time `json:"time"`
	Value T         `json:"value"`
}
