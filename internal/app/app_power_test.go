package app

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/power"
	"agent-overflow/internal/settings"
)

// keepAwakeRecorder replaces the OS seam and the event bus so a test can
// assert BOTH legs of an apply: what this process's OS was asked for, and
// what the launcher was told. They are separate mechanisms — on WSL the
// local leg is a deliberate no-op and the directive is the whole feature
// — so neither implies the other.
type keepAwakeRecorder struct {
	applied   []power.Mode
	emitted   []string
	applyErr  error
	otherEmit int
}

func newKeepAwakeApp(t *testing.T) (*App, *keepAwakeRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	rec := &keepAwakeRecorder{}
	app.keepAwakeApply = func(mode power.Mode) error {
		rec.applied = append(rec.applied, mode)
		return rec.applyErr
	}
	app.testEmitHook = func(name string, data any) {
		if name != string(eventchan.PowerKeepAwake) {
			rec.otherEmit++
			return
		}
		directive, ok := data.(keepAwakeDirective)
		if !ok {
			t.Fatalf("power:keepawake payload = %T, want keepAwakeDirective", data)
		}
		rec.emitted = append(rec.emitted, directive.Mode)
	}
	return app, rec
}

func TestUpdateSettingsFansOutKeepAwake(t *testing.T) {
	t.Run("flipping the master switch applies and emits the derived mode", func(t *testing.T) {
		app, rec := newKeepAwakeApp(t)

		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeEnabled": true}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		// keepAwakeScreen defaults TRUE, so the master switch alone means
		// "display": the mode is derived from the pair, never from the
		// patch key that happened to change.
		assertModes(t, rec.applied, power.ModeDisplay)
		assertEmitted(t, rec.emitted, "display")

		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeEnabled": false}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		assertModes(t, rec.applied, power.ModeDisplay, power.ModeOff)
		assertEmitted(t, rec.emitted, "display", "off")
	})

	t.Run("the screen axis alone reaches the OS while the feature stays on", func(t *testing.T) {
		app, rec := newKeepAwakeApp(t)

		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeEnabled": true}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeScreen": false}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		assertModes(t, rec.applied, power.ModeDisplay, power.ModeSystem)
		assertEmitted(t, rec.emitted, "display", "system")
	})

	t.Run("the screen axis is inert while the master switch is off", func(t *testing.T) {
		app, rec := newKeepAwakeApp(t)

		// Still a fan-out — presence in the patch is what triggers it —
		// but the derived mode is off, because the master switch decides
		// whether anything is held at all.
		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeScreen": false}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		assertModes(t, rec.applied, power.ModeOff)
		assertEmitted(t, rec.emitted, "off")
	})

	t.Run("an unrelated patch does not touch the inhibitor", func(t *testing.T) {
		app, rec := newKeepAwakeApp(t)

		if _, err := app.UpdateSettings(context.Background(), map[string]any{"fontSize": 15}); err != nil {
			t.Fatalf("UpdateSettings() error = %v", err)
		}
		assertModes(t, rec.applied)
		assertEmitted(t, rec.emitted)
	})

	t.Run("a failed OS apply still emits the directive", func(t *testing.T) {
		app, rec := newKeepAwakeApp(t)
		// The WSL shape in miniature: the local leg cannot work, and the
		// launcher's leg is the entire feature. A local failure must never
		// swallow the directive.
		rec.applyErr = errors.New("no inhibitor available")

		if _, err := app.UpdateSettings(context.Background(), map[string]any{"keepAwakeEnabled": true}); err != nil {
			t.Fatalf("UpdateSettings() error = %v, want the save to succeed regardless", err)
		}
		assertEmitted(t, rec.emitted, "display")
	})
}

// The persisted state must be asserted without the user touching
// anything; the boot path is the only thing that can do that.
func TestApplyKeepAwakeAtBoot(t *testing.T) {
	app, rec := newKeepAwakeApp(t)
	if _, err := app.settings.Update(map[string]any{
		"keepAwakeEnabled": true,
		"keepAwakeScreen":  false,
	}); err != nil {
		t.Fatalf("settings.Update() error = %v", err)
	}

	app.applyKeepAwake(app.currentSettings())

	assertModes(t, rec.applied, power.ModeSystem)
	assertEmitted(t, rec.emitted, "system")
}

// The two-key → one-mode collapse, pinned at the settings boundary: the
// wire carries a mode string, never the booleans, so this is the only
// place the pair is interpreted.
func TestKeepAwakeDefaultsAndModeDerivation(t *testing.T) {
	if settings.DefaultSettings.KeepAwakeEnabled {
		t.Error("keepAwakeEnabled must default false — an upgrading user must not start pinning their machine awake unasked")
	}
	if !settings.DefaultSettings.KeepAwakeScreen {
		t.Error("keepAwakeScreen must default true — an absent key has to read as the meaning of the phrase")
	}

	app, rec := newKeepAwakeApp(t)
	// Defaults alone resolve to off, which is what makes the boot apply on
	// a fresh install a no-op rather than a held inhibitor.
	app.applyKeepAwake(settings.DefaultSettings)
	assertModes(t, rec.applied, power.ModeOff)
	assertEmitted(t, rec.emitted, "off")
}

func assertModes(t *testing.T, got []power.Mode, want ...power.Mode) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("applied modes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applied modes = %v, want %v", got, want)
		}
	}
}

func assertEmitted(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("emitted power:keepawake modes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("emitted power:keepawake modes = %v, want %v", got, want)
		}
	}
}
