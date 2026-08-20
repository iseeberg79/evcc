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

// a torn/implausible read (spurious decrease, e.g. a modbus glitch) must not
// rebase the baseline - otherwise the next valid reading would book the gap
// it covers as one inflated delta instead of recovering cleanly
func TestMeterEnergyMeterTotalIgnoresBackwardRead(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(10)
	assert.Equal(t, 0.0, me.Energy)

	ok := me.SetEnergyMeterTotal(9.99) // well beyond meterTotalNoiseFloor
	assert.False(t, ok)
	assert.Equal(t, 0.0, me.Energy)

	me.SetEnergyMeterTotal(10.5)
	assert.Equal(t, 0.5, me.Energy)
}

// a dip within meterTotalNoiseFloor (register/timing jitter, not a real decrease)
// must not be flagged - unlike a genuine torn read, it's expected noise, not
// something worth logging for diagnosis
func TestMeterEnergyMeterTotalIgnoresNoiseFloorDip(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(10)
	assert.Equal(t, 0.0, me.Energy)

	ok := me.SetEnergyMeterTotal(10 - meterTotalNoiseFloor/2)
	assert.True(t, ok, "a dip within the noise floor must not be flagged")
	assert.Equal(t, 0.0, me.Energy, "must not book negative energy for the dip")

	// a dip beyond the noise floor is still a torn/implausible read
	ok = me.SetEnergyMeterTotal(10 - 2*meterTotalNoiseFloor)
	assert.False(t, ok)
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
