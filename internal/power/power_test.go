package power

import (
	"errors"
	"testing"
)

// fakeBackend stands in for the OS layer. No test in this package may
// reach a real D-Bus bus, spawn caffeinate(8), or move the calling
// machine's execution state; the package-level Apply refuses inside a
// test binary (newGuardedBackend), and these tests drive their own
// controller so they can assert what the OS layer was asked for.
type fakeBackend struct {
	calls []Mode
	err   error
}

func (f *fakeBackend) set(mode Mode) error {
	f.calls = append(f.calls, mode)
	return f.err
}

func newFakeController() (*controller, *fakeBackend) {
	fake := &fakeBackend{}
	built := 0
	c := &controller{newBackend: func() backend {
		built++
		return fake
	}}
	return c, fake
}

func TestModeFor(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		screen  bool
		want    Mode
	}{
		// The master switch alone decides whether anything is held: the
		// screen axis is inert while the feature is off, which is what
		// lets the frontend persist the user's screen preference without
		// it meaning anything yet.
		{"off ignores the screen axis", false, true, ModeOff},
		{"off with screen off", false, false, ModeOff},
		{"on without screen keeps only the machine", true, false, ModeSystem},
		{"on with screen keeps the display too", true, true, ModeDisplay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModeFor(tc.enabled, tc.screen); got != tc.want {
				t.Fatalf("ModeFor(%v, %v) = %q, want %q", tc.enabled, tc.screen, got, tc.want)
			}
		})
	}
}

func TestParseModeRejectsUnknownInputWithoutDefaulting(t *testing.T) {
	for _, valid := range []Mode{ModeOff, ModeSystem, ModeDisplay} {
		got, ok := ParseMode(string(valid))
		if !ok || got != valid {
			t.Fatalf("ParseMode(%q) = (%q, %v), want (%q, true)", valid, got, ok, valid)
		}
	}
	// A directive nobody can read must not resolve to "hold something".
	for _, bad := range []string{"", "on", "system+display", "OFF", "true"} {
		got, ok := ParseMode(bad)
		if ok {
			t.Fatalf("ParseMode(%q) accepted an unknown mode as %q", bad, got)
		}
		if got != ModeOff {
			t.Fatalf("ParseMode(%q) rejected value = %q, want %q", bad, got, ModeOff)
		}
	}
}

// The transition matrix. The controller is the only thing that decides
// whether the OS layer is touched at all, so every edge is asserted
// against the exact call list the backend saw.
func TestControllerTransitions(t *testing.T) {
	t.Run("off to on and back", func(t *testing.T) {
		c, fake := newFakeController()
		if err := c.apply(ModeSystem); err != nil {
			t.Fatalf("apply(system) error = %v", err)
		}
		if err := c.apply(ModeOff); err != nil {
			t.Fatalf("apply(off) error = %v", err)
		}
		assertCalls(t, fake, ModeSystem, ModeOff)
		if got := c.currentMode(); got != ModeOff {
			t.Fatalf("current = %q, want %q", got, ModeOff)
		}
	})

	t.Run("mode flip while on reaches the OS layer", func(t *testing.T) {
		c, fake := newFakeController()
		mustApply(t, c, ModeSystem)
		// The screen axis flipping is a real change even though the
		// feature stayed on the whole time.
		mustApply(t, c, ModeDisplay)
		mustApply(t, c, ModeSystem)
		assertCalls(t, fake, ModeSystem, ModeDisplay, ModeSystem)
	})

	t.Run("double apply of the same mode is a no-op", func(t *testing.T) {
		c, fake := newFakeController()
		mustApply(t, c, ModeDisplay)
		mustApply(t, c, ModeDisplay)
		mustApply(t, c, ModeDisplay)
		// Re-asserting is not merely wasteful: on linux it would drop and
		// retake the inhibitor, opening a window where the machine could
		// sleep.
		assertCalls(t, fake, ModeDisplay)
	})

	t.Run("off while already off never builds a backend", func(t *testing.T) {
		fake := &fakeBackend{}
		built := 0
		c := &controller{newBackend: func() backend {
			built++
			return fake
		}}
		mustApply(t, c, ModeOff)
		if built != 0 {
			t.Fatalf("backend built %d times for a no-op apply, want 0", built)
		}
		// …and it is built exactly once thereafter, however many
		// transitions follow.
		mustApply(t, c, ModeSystem)
		mustApply(t, c, ModeDisplay)
		mustApply(t, c, ModeOff)
		if built != 1 {
			t.Fatalf("backend built %d times, want 1", built)
		}
	})

	t.Run("a failed apply does not advance the recorded state", func(t *testing.T) {
		c, fake := newFakeController()
		fake.err = errors.New("no inhibitor")
		if err := c.apply(ModeDisplay); err == nil {
			t.Fatal("apply(display) error = nil, want the backend's failure")
		}
		if got := c.currentMode(); got != ModeOff {
			t.Fatalf("current = %q after a failed apply, want %q", got, ModeOff)
		}
		// The retry is the point: a bus that was not up at boot may be up
		// now, and a short-circuit here would strand the feature off.
		fake.err = nil
		mustApply(t, c, ModeDisplay)
		assertCalls(t, fake, ModeDisplay, ModeDisplay)
	})

	t.Run("an unknown mode is refused before the OS layer", func(t *testing.T) {
		c, fake := newFakeController()
		if err := c.apply(Mode("system+display")); err == nil {
			t.Fatal("apply(bogus) error = nil, want a rejection")
		}
		assertCalls(t, fake)
	})
}

// The package-level Apply must be inert against real system state inside
// a test binary — the same rule that keeps fixtures away from real
// provider binaries.
func TestPackageApplyRefusesInsideATestBinary(t *testing.T) {
	if _, ok := newGuardedBackend().(refusingBackend); !ok {
		t.Fatal("newGuardedBackend() did not return the refusing backend inside a test binary")
	}
	if err := Apply(ModeDisplay); err == nil {
		t.Fatal("Apply(display) error = nil inside a test binary, want a refusal")
	}
	if got := Current(); got != ModeOff {
		t.Fatalf("Current() = %q after a refused apply, want %q", got, ModeOff)
	}
}

func mustApply(t *testing.T, c *controller, mode Mode) {
	t.Helper()
	if err := c.apply(mode); err != nil {
		t.Fatalf("apply(%s) error = %v", mode, err)
	}
}

func assertCalls(t *testing.T, fake *fakeBackend, want ...Mode) {
	t.Helper()
	if len(fake.calls) != len(want) {
		t.Fatalf("backend calls = %v, want %v", fake.calls, want)
	}
	for i := range want {
		if fake.calls[i] != want[i] {
			t.Fatalf("backend calls = %v, want %v", fake.calls, want)
		}
	}
}
