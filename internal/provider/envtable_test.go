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

// Every row of the table settles what settings does with its name: a row with
// a refusal is refused for its provider in any case with that reason, a row
// without one is accepted, and no two rows that reach one provider share a
// name.
func TestEveryReservedEnvRowDecidesItsRefusal(t *testing.T) {
	if len(reservedEnvTable) == 0 {
		t.Fatal("the reserved table is empty")
	}
	providers := []string{"claude", "codex", "agy"}
	owner := map[string]*reservedEnvEntry{}
	for _, entry := range reservedEnvTable {
		reaches := providers
		if entry.provider != "" {
			reaches = []string{entry.provider}
		}
		for _, providerKey := range reaches {
			key := providerKey + "/" + entry.name
			if owner[key] != nil {
				t.Errorf("%s is declared twice for %s", entry.name, providerKey)
			}
			owner[key] = entry
			want := EnvRefusal{}
			if entry.refusal != "" {
				want = EnvRefusal{Name: entry.name, Reason: entry.refusal}
			}
			for _, spelling := range []string{entry.name, strings.ToLower(entry.name)} {
				got, refused := ReservedEnvRefusal(providerKey, spelling)
				if refused != (entry.refusal != "") || got != want {
					t.Errorf("ReservedEnvRefusal(%q, %q) = %+v, %v; want %+v", providerKey, spelling, got, refused, want)
				}
			}
		}
		if entry.refusal != "" && len(entry.name) > MaxRefusedEnvNameLength {
			t.Errorf("%s is longer than MaxRefusedEnvNameLength (%d)", entry.name, MaxRefusedEnvNameLength)
		}
	}
}

// A name under the prefix is refused for the prefix, with no table name.
func TestAPrefixedNameIsRefusedForItsPrefix(t *testing.T) {
	got, refused := ReservedEnvRefusal("codex", "ao_token")
	if !refused || got != (EnvRefusal{Reason: reservedPrefixRefusal}) {
		t.Fatalf("ReservedEnvRefusal(codex, ao_token) = %+v, %v; want the prefix refusal", got, refused)
	}
}
