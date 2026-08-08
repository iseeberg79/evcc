package metrics

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/jinzhu/now"
	"github.com/stretchr/testify/assert"
)

func TestMeterEnergyMeterTotal(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(10)
	assert.Equal(t, 0.0, me.Energy)
	me.SetEnergyMeterTotal(11)
	assert.Equal(t, 1.0, me.Energy)
	me.SetEnergyMeterTotal(11)
	assert.Equal(t, 1.0, me.Energy)
}

// Regression test for #29286/#30555: a single low reading must not rebase
// the baseline, so the recovery isn't booked as one giant delta.
func TestMeterEnergyMeterTotalTransientRollback(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(25567.546)
	assert.Equal(t, 0.0, me.Energy)
	me.SetEnergyMeterTotal(25567.548)
	assert.InDelta(t, 0.002, me.Energy, 1e-9)

	good := me.Updated()
	clock.Add(time.Second)

	me.SetEnergyMeterTotal(0) // transient bad read
	assert.InDelta(t, 0.002, me.Energy, 1e-9, "spurious drop must not be booked")
	assert.Equal(t, good, me.Updated(), "updated must not advance while a drop is pending confirmation")

	clock.Add(time.Second)
	me.SetEnergyMeterTotal(25567.550) // recovers before confirmation: baseline preserved
	assert.InDelta(t, 0.004, me.Energy, 1e-9, "recovery must not produce a spike")
	assert.True(t, me.Updated().After(good), "updated must advance on an accepted reading")
}

// A drop confirmed over rollbackConfirmations readings is accepted as a
// genuine reset/rollover instead of being ignored forever (#30267).
func TestMeterEnergyMeterTotalConfirmedReset(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(500)
	me.SetEnergyMeterTotal(501)
	assert.Equal(t, 1.0, me.Energy)

	clock.Add(time.Second)
	me.SetEnergyMeterTotal(0.1) // 1st low reading: pending
	assert.Equal(t, 1.0, me.Energy)

	clock.Add(time.Second)
	me.SetEnergyMeterTotal(0.2) // 2nd low reading: confirmed reset
	assert.Equal(t, 1.0, me.Energy)

	clock.Add(time.Second)
	me.SetEnergyMeterTotal(0.3)
	assert.InDelta(t, 1.1, me.Energy, 1e-9)
}

func TestMeterEnergyAddPower(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	clock.Add(60 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 0.0, me.Energy)

	clock.Add(60 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 1.0, me.Energy)

	clock.Add(30 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 1.5, me.Energy)
}
