package cmd

import (
	"strings"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
)

func TestWithDeviceName(t *testing.T) {
	// injects the device name, preserves other keys, leaves the input untouched
	in := map[string]any{"host": "localhost"}
	out := withDeviceName(in, "db:42")
	assert.Equal(t, "db:42", out["name"])
	assert.Equal(t, "localhost", out["host"])
	_, mutated := in["name"]
	assert.False(t, mutated, "input map must not be modified")

	// nil input is handled
	assert.Equal(t, "db:7", withDeviceName(nil, "db:7")["name"])
}

func TestYamlOff(t *testing.T) {
	var conf globalconfig.All
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader(`loadpoints:
- mode: off
`)); err != nil {
		t.Error(err)
	}

	if err := viper.UnmarshalExact(&conf); err != nil {
		t.Error(err)
	}

	var lp core.Loadpoint
	if err := util.DecodeOther(conf.Loadpoints[0].Other, &lp); err != nil {
		t.Error(err)
	}

	if lp.DefaultMode != api.ModeOff {
		t.Errorf("expected `off`, got %s", lp.DefaultMode)
	}
}
