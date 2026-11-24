package provider

import (
	"strings"
	"testing"
)

// One name is one handle, so a pin made from a boot map and a pin made from a
// constant collide in EnvInput.Pins instead of producing the variable twice.
func TestAOEnvInternsEachName(t *testing.T) {
	if AOEnv("AO_TEST_INTERN") != AOEnv("AO_TEST_INTERN") {
		t.Fatal("one name produced two handles")
	}
	pins := AOPins(map[string]string{"AO_TEST_INTERN": "boot"})
	pins[AOEnv("AO_TEST_INTERN")] = "constant"
	if len(pins) != 1 {
		t.Fatalf("pins = %v, want one entry", pins)
	}
}

// A name outside the prefix would become a handle for a variable the table
// does not reserve, so it panics instead.
func TestAOEnvRefusesANameWithoutThePrefix(t *testing.T) {
	defer func() {
		recovered := recover()
		message, _ := recovered.(string)
		if !strings.Contains(message, `"PATH"`) || !strings.Contains(message, ReservedEnvPrefix) {
			t.Fatalf("recovered %v, want a panic naming the variable and the prefix", recovered)
		}
	}()
	AOEnv("PATH")
}

// The zero handle is the one value another package can write without the
// table; building an environment from it panics instead of setting nothing.
func TestTheZeroReservedEnvPanicsWhenBuilt(t *testing.T) {
	for name, in := range map[string]EnvInput{
		"pin":     {Pins: map[ReservedEnv]string{{}: "1"}},
		"default": {Defaults: map[ReservedEnv]string{{}: "1"}},
		"clear":   {Clears: []ReservedEnv{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if message, _ := recover().(string); !strings.Contains(message, "zero ReservedEnv") {
					t.Fatalf("recovered %q, want the zero-handle panic", message)
				}
			}()
			BuildEnvironment(in)
		})
	}
}
