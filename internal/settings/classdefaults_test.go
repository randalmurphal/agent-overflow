package settings

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// The table is TOTAL over the declared classes, and the empty rows are rows.
// A class arriving without one would resolve to "no overrides" by accident
// rather than by decision, which is the difference this test keeps.
func TestClassDefaultsCoverEveryDeclaredClass(t *testing.T) {
	for _, class := range DeviceClasses {
		if _, placed := classDefaults[class]; !placed {
			t.Errorf("%s has no row in classDefaults: decide what that class starts from, even if the answer is nothing", class)
		}
	}
	for class := range classDefaults {
		if !class.Valid() {
			t.Errorf("classDefaults names %q, which is not a declared DeviceClass", class)
		}
	}
}

// Product defaults: normal power on every screen. Saved choices remain
// covered by the layer tests below, using a synthetic differing class row.
func TestAllDeviceClassesDefaultToNormalPower(t *testing.T) {
	for _, class := range DeviceClasses {
		if len(classDefaults[class]) != 0 {
			t.Errorf("%s has unexpected overrides: %v", class, classDefaults[class])
		}
	}
	if DefaultSettings.LowPowerMode {
		t.Fatal("low-power mode must be opt-in")
	}
}

// Keep exercising a genuinely different class layer even when production
// classes share defaults. These tests are non-parallel; restore on cleanup.
func withPhoneClassDefault(t *testing.T) {
	t.Helper()
	previous := classRows
	rows := maps.Clone(previous())
	rows[DevicePhone] = map[string]string{"lowPowerMode": "true"}
	classRows = func() map[DeviceClass]map[string]string { return rows }
	t.Cleanup(func() { classRows = previous })
}

// Only DEVICE-tier keys may carry a class default. A host key here would be a
// per-screen answer to a question about the backend machine and a user key a
// per-screen answer to a question about the person — and applyRows would drop
// either silently, so the table would look like it worked.
func TestEveryClassDefaultNamesADeviceTierKey(t *testing.T) {
	for _, key := range classDefaultKeys() {
		tier, known := TierForKey(key)
		if !known {
			t.Errorf("class default %q has no tier at all: place it in tier.go", key)
			continue
		}
		if tier != TierDevice {
			t.Errorf("class default %q is tier %q, want %q: a class describes a SCREEN", key, tier, TierDevice)
		}
	}
}

// A class default has to be a value a device could also have written. One the
// strict validator refuses could never be saved, and one the LENIENT
// sanitizer clamps would be erased on every read — getFor runs it over the
// merged result — so the table would advertise a default nothing can hold.
func TestClassDefaultsSurviveValidationAndSanitizing(t *testing.T) {
	for _, class := range DeviceClasses {
		rows := classOverrides(class)
		if len(rows) == 0 {
			continue
		}
		candidate := copyDefaults()
		applyRows(&candidate, rows, TierDevice)

		validated, err := validateSettings(candidate)
		if err != nil {
			t.Errorf("the %s class defaults do not validate: %v", class, err)
			continue
		}
		assertRowsSurvive(t, class, "validateSettings", rows, validated)
		assertRowsSurvive(t, class, "sanitizeLoadedSettings", rows, sanitizeLoadedSettings(candidate))
	}
}

// assertRowsSurvive re-projects a Settings value and checks the class row's
// keys still carry the values the table named.
func assertRowsSurvive(t *testing.T, class DeviceClass, pass string, rows map[string]string, got Settings) {
	t.Helper()
	values, err := keyValues(got)
	if err != nil {
		t.Fatalf("project the %s result: %v", pass, err)
	}
	for key, want := range rows {
		if values[key] != want {
			t.Errorf("%s dropped the %s class default %s: %q, want %q", pass, class, key, values[key], want)
		}
	}
}

// The encoded projection must say the same thing as the table it comes from.
// applyRows consumes the encoding, so a divergence would be invisible.
func TestClassRowsEncodeTheTable(t *testing.T) {
	for class, overrides := range classDefaults {
		rows := classOverrides(class)
		if len(rows) != len(overrides) {
			t.Errorf("%s: %d encoded rows for %d overrides", class, len(rows), len(overrides))
			continue
		}
		for key, value := range overrides {
			want, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("%s[%s]: %v", class, key, err)
			}
			if rows[key] != string(want) {
				t.Errorf("%s[%s] encoded as %q, want %q", class, key, rows[key], want)
			}
		}
	}
}

// The headline: DefaultSettings < the class row < the bucket's own write, and
// each step observable on its own.
func TestDeviceClassDefaultsResolveUnderTheBucketsOwnWrite(t *testing.T) {
	withPhoneClassDefault(t)
	svc, store := tieredService(t)
	const bucket = "device:phone-0001"

	// 1. Default < class. A phone that never wrote the key reads its class's
	//    answer; a desktop with the same empty bucket reads the global one.
	if !svc.For(bucket, DevicePhone).Get().LowPowerMode {
		t.Fatal("a phone with no rows did not pick up its class default")
	}
	if svc.For(bucket, DeviceDesktop).Get().LowPowerMode {
		t.Fatal("a desktop with no rows picked up the phone's class default")
	}
	if DefaultSettings.LowPowerMode {
		t.Fatal("the global default is already on; this test proves nothing")
	}

	// 2. Class < write, and the write may be the class default's OPPOSITE.
	//    Without the class in mutate's pre-read this patch would move
	//    nothing, persist nothing, and read back as true.
	if _, err := svc.For(bucket, DevicePhone).Update(map[string]any{"lowPowerMode": false}); err != nil {
		t.Fatalf("phone Update: %v", err)
	}
	if got := store.scopes[bucket]["lowPowerMode"]; got != "false" {
		t.Fatalf("%s[lowPowerMode] = %q, want an explicit false", bucket, got)
	}
	if svc.For(bucket, DevicePhone).Get().LowPowerMode {
		t.Fatal("the phone's own write did not outrank its class default")
	}

	// 3. CLEARING returns to the CLASS default, not to the global one. The
	//    only clear that exists is dropping the row — DeleteUIState reaches
	//    these rows, since they share the bucket — and with the row gone the
	//    read falls through to the layer below it, which is the class's.
	delete(store.scopes[bucket], "lowPowerMode")
	if !svc.For(bucket, DevicePhone).Get().LowPowerMode {
		t.Fatal("clearing the row fell through to the global default instead of the class default")
	}
}

// A class default is resolved at READ and never written. That is what lets a
// device that only ever read the key track a later change to the table.
func TestAClassDefaultIsNeverWrittenIntoTheBucket(t *testing.T) {
	withPhoneClassDefault(t)
	svc, store := tieredService(t)
	const bucket = "device:phone-0002"

	phone := svc.For(bucket, DevicePhone)
	if !phone.Get().LowPowerMode {
		t.Fatal("the class default did not resolve")
	}
	// An unrelated device-tier write is the moment a naive implementation
	// would flush the whole resolved slice into the bucket.
	if _, err := phone.Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("phone Update: %v", err)
	}
	phone.AddRecentWorkspace("/repo/one")

	if _, written := store.scopes[bucket]["lowPowerMode"]; written {
		t.Errorf("the class default was persisted into %s: %v", bucket, store.scopes[bucket])
	}
	// Still resolved, and still only from the table.
	if !phone.Get().LowPowerMode {
		t.Error("the class default stopped resolving after an unrelated write")
	}
}

// A class layer belongs to ONE caller. The shared snapshot every bucket-less
// backend read serves must not carry it, or a phone's write would make
// lowPowerMode this backend's answer for every screen.
func TestAClassDefaultDoesNotLeakIntoTheSharedSnapshot(t *testing.T) {
	withPhoneClassDefault(t)
	svc, _ := tieredService(t)
	if _, err := svc.For("device:phone-0003", DevicePhone).Update(map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("phone Update: %v", err)
	}
	if svc.Get().LowPowerMode {
		t.Error("the phone's class default reached the shared snapshot")
	}
	if svc.BackendScreen().Get().LowPowerMode {
		t.Error("the phone's class default reached the backend's own screen")
	}
	if svc.For("client:desk-0003", DeviceDesktop).Get().LowPowerMode {
		t.Error("the phone's class default reached another screen")
	}
}

// No store, no residency, no class. The pre-database boot readers (main.go,
// main_desktop.go) keep every tier in settings.json, so the FILE holds the
// screen's own value there — a class row applied over the top would outrank
// the very write it is supposed to sit under.
func TestAStoreLessServiceIgnoresClassDefaults(t *testing.T) {
	withPhoneClassDefault(t)
	svc := NewService(t.TempDir())
	if svc.For("device:phone-0004", DevicePhone).Get().LowPowerMode {
		t.Error("a store-less service applied a class default")
	}
}

// Two phones, one table, two independent answers: the class layer is shared
// and the bucket layer is not.
func TestTwoPhonesResolveTheSameClassAndKeepSeparateWrites(t *testing.T) {
	withPhoneClassDefault(t)
	svc, _ := tieredService(t)
	const kept, changed = "device:phone-kept", "device:phone-changed"

	if _, err := svc.For(changed, DevicePhone).Update(map[string]any{"lowPowerMode": false}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !svc.For(kept, DevicePhone).Get().LowPowerMode {
		t.Error("one phone's write reached another phone")
	}
	if svc.For(changed, DevicePhone).Get().LowPowerMode {
		t.Error("the phone that wrote lost its own value")
	}
}

// classDefaultKeys is what the tier guard enumerates the table with; it has
// to actually see every row.
func TestClassDefaultKeysEnumeratesEveryRow(t *testing.T) {
	want := map[string]struct{}{}
	for _, overrides := range classDefaults {
		for key := range overrides {
			want[key] = struct{}{}
		}
	}
	got := classDefaultKeys()
	if len(got) != len(want) {
		t.Fatalf("classDefaultKeys() = %v, want %d distinct keys", got, len(want))
	}
	if !slices.IsSorted(got) {
		t.Errorf("classDefaultKeys() = %v, want sorted", got)
	}
	for _, key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("classDefaultKeys() invented %q", key)
		}
	}
}
