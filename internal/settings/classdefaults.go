package settings

import (
	"encoding/json"
	"log"
	"maps"
	"slices"
	"sync"
)

// Device-class defaults sit between DefaultSettings and a screen's own writes:
//
//     DefaultSettings < class row < bucket's own rows
//
// Resolve at read, never persist defaults into a bucket. Untouched devices
// follow future default changes; saved overrides win, and clearing one returns
// to the current default. Mutate uses the same resolved pre-read so writing a
// value opposite to a class override persists correctly. Writing an unchanged
// value creates no override, preserving the existing sparse-settings contract.

// DeviceClass is what kind of screen a device-tier read is being resolved
// for. The values mirror identity.DeviceClass exactly.
//
// MIRRORED RATHER THAN IMPORTED, and the direction is the deliberate half.
// This package is dependency-free on purpose (see the Anti-patterns section
// of AGENTS.md: internal/provider's two tables are duplicated here, and
// internal/spinner's id grammar with them), while internal/identity sits on
// internal/store. Importing identity would put a database package underneath
// every pre-database settings reader — main.go and main_desktop.go build a
// Service before the store exists — to gain five string constants.
//
// So the CALLER converts. internal/app already holds both packages, and
// TestSettingsDeviceClassesMirrorTheIdentityVocabulary there fails in both
// directions on drift, which is the same shape as the provider deny-list's
// root-package cross-check.
type DeviceClass string

const (
	// DeviceDesktop is a native desktop app instance. It is also what a
	// screen with no device row of its own resolves to (residency.go's
	// BackendScreen, the local page channel's client buckets).
	DeviceDesktop DeviceClass = "desktop"
	// DeviceBrowser is one browser profile.
	DeviceBrowser DeviceClass = "browser"
	// DevicePhone is a phone or tablet app instance.
	DevicePhone DeviceClass = "phone"
	// DeviceCLI is a command-line client on some host.
	DeviceCLI DeviceClass = "cli"
	// DeviceBackendPeer is another backend enrolled for team sharing.
	DeviceBackendPeer DeviceClass = "backend-peer"
)

// DeviceClasses is every declared class, in the order identity spells them.
var DeviceClasses = []DeviceClass{
	DeviceDesktop, DeviceBrowser, DevicePhone, DeviceCLI, DeviceBackendPeer,
}

// Valid reports whether c is a declared class.
func (c DeviceClass) Valid() bool { return slices.Contains(DeviceClasses, c) }

// classDefaults is the table: per class, the device-tier keys whose default
// differs from DefaultSettings for that kind of screen.
//
// TOTAL over DeviceClasses, and the empty rows are the point. A class with no
// overrides is a decision somebody made — "a browser is a screen like any
// other" — not a class nobody got round to, and
// TestClassDefaultsCoverEveryDeclaredClass fails when a new class arrives
// without one.
//
// Only device-tier keys may appear. A host key here would be a per-screen
// answer to a question about the backend machine, and a user key would be a
// per-screen answer to a question about the person; applyRows would silently
// skip either, so TestEveryClassDefaultNamesADeviceTierKey fails instead.
// TestClassDefaultsSurviveValidation additionally drives each row through
// Validate, because a default no write could ever produce is not a default.
//
// All classes currently use the global defaults. Low-power mode is opt-in on
// phones too; keep the layer for deliberate device-class differences.
var classDefaults = map[DeviceClass]map[string]any{
	DeviceDesktop:     {},
	DeviceBrowser:     {},
	DevicePhone:       {},
	DeviceCLI:         {},
	DeviceBackendPeer: {},
}

// classRows is classDefaults under the same per-key JSON projection the
// `ui_state` rows use, computed once. Sharing the encoding is what lets
// applyRows apply a class row and a bucket's rows through one code path — the
// overlay order is then just the call order, with no second decoder to keep
// in step.
var classRows = sync.OnceValue(func() map[DeviceClass]map[string]string {
	rows := make(map[DeviceClass]map[string]string, len(classDefaults))
	for class, overrides := range classDefaults {
		if len(overrides) == 0 {
			continue
		}
		encoded := make(map[string]string, len(overrides))
		for key, value := range overrides {
			raw, err := json.Marshal(value)
			if err != nil {
				// A compile-time literal of JSON-encodable types; unreachable
				// without the table itself being wrong, which the tests catch.
				log.Printf("settings: encode the %s class default for %q: %v", class, key, err)
				continue
			}
			encoded[key] = string(raw)
		}
		rows[class] = encoded
	}
	return rows
})

// classOverrides answers one class's encoded rows.
//
// An UNDECLARED class — including the empty string a zero-valued device row
// carries — answers nothing, which is exactly the behaviour every device had
// before this table existed. Callers that can name a class pass one;
// internal/app's conversion resolves an unreadable class to DeviceDesktop
// rather than to nothing, and says why there.
//
// The returned map is shared and must not be written to. applyRows only
// reads, and every caller in this package goes through it.
func classOverrides(class DeviceClass) map[string]string {
	return classRows()[class]
}

// classDefaultKeys is every key named by any class row, sorted. Test-facing
// today; it exists so a guard can enumerate the table without reaching into
// its internals and without a second spelling of the traversal.
func classDefaultKeys() []string {
	keys := map[string]struct{}{}
	for _, overrides := range classDefaults {
		for key := range overrides {
			keys[key] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}
