package browser

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/keybindings"
)

// focusEngine is the fake engine with an app webview to hand keyboard
// focus to: it records each FocusApp in the order of the log it shares with
// the routed chords, so a test reads whether the handoff came before the
// chord left keyChord.
type focusEngine struct {
	*fakeEngine
	mu  sync.Mutex
	log []string
	err error
}

func (e *focusEngine) FocusApp() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, "focus")
	return e.err
}

func (e *focusEngine) note(entry string) {
	e.mu.Lock()
	e.log = append(e.log, entry)
	e.mu.Unlock()
}

func (e *focusEngine) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	log := e.log
	e.log = nil
	return log
}

// Every chord the Manager claims hands keyboard focus to the app before
// keyChord returns, which is before the chord is routed; a chord it does
// not claim, and every bare Escape, leaves focus in the page. Each row is
// pressed twice in a row, in every page state a key can reach the gate in.
func TestKeyChordHandsFocusToTheAppExactlyWhenItClaims(t *testing.T) {
	bound := keybindings.Accelerator{Key: "f", Ctrl: true}
	boundEscape := keybindings.Accelerator{Key: "escape", Ctrl: true}
	rows := []struct {
		name    string
		pressed keybindings.Accelerator
		press   KeyPress
		claimed bool
	}{
		{"a bound chord", bound, KeyPress{}, true},
		{"a bound chord's shifted spelling", keybindings.Accelerator{Key: "F", Ctrl: true, Shift: true}, KeyPress{}, false},
		{"a bound Ctrl+Escape", boundEscape, KeyPress{}, true},
		{"an unbound chord", keybindings.Accelerator{Key: "g", Ctrl: true}, KeyPress{}, false},
		{"a bare Escape", keybindings.Accelerator{Key: "escape"}, KeyPress{}, false},
		{"a held bare Escape", keybindings.Accelerator{Key: "escape"}, KeyPress{Held: true}, false},
		{"a composing bare Escape", keybindings.Accelerator{Key: "escape"}, KeyPress{Composing: true}, false},
		{"a shift-only key", keybindings.Accelerator{Key: "a", Shift: true}, KeyPress{}, false},
	}
	var engine *focusEngine
	f := newFindFixtureWith(t, "https://example.test/alpha", func(m *Manager) {
		engine = &focusEngine{fakeEngine: m.engine.(*fakeEngine)}
		m.engine = engine
		m.accelerators = func() keybindings.AcceleratorSet {
			return keybindings.AcceleratorSet{bound: {}, boundEscape: {}}
		}
	})
	popup := "a-popup-the-manager-has-not-adopted"
	states := []struct {
		name   string
		handle string
		set    func(t *testing.T)
	}{
		{"the page shown", f.fake.Handle(), func(*testing.T) {}},
		{"the page hidden", f.fake.Handle(), func(t *testing.T) {
			hide := false
			if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"the page navigating", f.fake.Handle(), func(t *testing.T) {
			f.fake.commitDocument()
		}},
		{"a page the Manager does not hold", popup, func(*testing.T) {}},
	}
	for _, state := range states {
		state.set(t)
		for _, row := range rows {
			engine.take()
			for range 2 {
				if got := f.manager.keyChord(state.handle, row.pressed, row.press); got != row.claimed {
					t.Fatalf("%s, %s: claimed %v, want %v", state.name, row.name, got, row.claimed)
				}
				engine.note("returned")
			}
			want := []string{"returned", "returned"}
			if row.claimed {
				want = []string{"focus", "returned", "focus", "returned"}
			}
			if got := engine.take(); !slices.Equal(got, want) {
				t.Errorf("%s, %s: %v, want %v", state.name, row.name, got, want)
			}
		}
	}
	drainRouted(f)
}

// A handoff the engine cannot make leaves focus where it was; the chord is
// still claimed and still routed.
func TestKeyChordRoutesAChordWhoseFocusHandoffFailed(t *testing.T) {
	bound := keybindings.Accelerator{Key: "f", Ctrl: true}
	var engine *focusEngine
	f := newFindFixtureWith(t, "https://example.test/alpha", func(m *Manager) {
		engine = &focusEngine{fakeEngine: m.engine.(*fakeEngine), err: errors.New("no app webview")}
		m.engine = engine
		m.accelerators = func() keybindings.AcceleratorSet { return keybindings.AcceleratorSet{bound: {}} }
	})
	if !f.manager.keyChord(f.fake.Handle(), bound, KeyPress{}) {
		t.Fatal("a bound chord was not claimed")
	}
	if got := engine.take(); !slices.Equal(got, []string{"focus"}) {
		t.Fatalf("the handoff log is %v", got)
	}
	if key := <-f.keys; key != bound {
		t.Fatalf("routed %+v, want %+v", key, bound)
	}
}

// drainRouted empties the fixture's routed chords and Escapes, which the
// focus table does not read.
func drainRouted(f *findFixture) {
	for {
		select {
		case <-f.keys:
		case <-f.escapes:
		default:
			return
		}
	}
}
