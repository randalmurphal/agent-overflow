// Package power owns the OS sleep inhibitor behind the "keep awake"
// setting: while a long provider turn runs unattended the machine must
// not idle-sleep, and — when the user asks for it — the display must
// stay lit too.
//
// # One API
//
// Apply(mode) is the whole surface. Mode is derived from the two
// persisted settings (ModeFor) and is absolute, not incremental: every
// call states the state the OS should be in, and the package makes it so
// from wherever it currently is. Calling Apply twice with the same mode
// is free; every transition (off→system→display→off, and back) is a
// legal input.
//
// # Failsafes are inherent, never bookkeeping
//
// Nothing here writes a file, registers an atexit hook, or asks to be
// cleaned up on shutdown. Every platform's hold dies with the process:
//
//   - linux: the D-Bus inhibit cookies and the login1 inhibitor fd are
//     owned by our bus connections and our file descriptors. Process
//     death closes both and the inhibit lapses.
//   - darwin: caffeinate(8) is spawned with `-w <our pid>`, so it exits
//     on its own the moment we do — even on SIGKILL.
//   - windows: SetThreadExecutionState is per-thread process state. It
//     evaporates when the process exits.
//
// This is why there is no Release() in the API: ModeOff is the release,
// and forgetting to call it can only ever outlive us by nothing.
//
// # Platform split
//
// The OS call lives in one small backend per GOOS behind //go:build
// tags. The state machine — which is where the bugs would be — is
// platform-neutral and lives here, tested against a fake backend.
//
// On WSL this package is deliberately INERT: the Linux side of a WSL
// install cannot keep the Windows host awake. The shipped Windows path
// runs the Win32 call in the launcher process instead, driven by the
// power:keepawake directive (app_power.go → the notification bridge →
// cmd/agent-overflow-windows). That launcher calls straight back into
// this package's windows backend, so there is exactly one execution-state
// holder implementation in the repo.
package power

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Mode is the state the OS sleep inhibitor should be in. It is also the
// exact wire spelling carried by the power:keepawake directive — one
// string, not two booleans, so the launcher cannot reconstruct a
// combination the backend never meant.
type Mode string

const (
	// ModeOff releases everything: the machine idles and sleeps normally.
	ModeOff Mode = "off"
	// ModeSystem prevents system idle-sleep. The display may still blank
	// and lock; only the machine stays running.
	ModeSystem Mode = "system"
	// ModeDisplay is ModeSystem plus keeping the display on.
	ModeDisplay Mode = "display"
)

// String returns the wire spelling.
func (m Mode) String() string { return string(m) }

// Valid reports whether m is one of the three defined modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeSystem, ModeDisplay:
		return true
	}
	return false
}

// ModeFor derives the mode from the two persisted settings.
// keepScreenOn is only consulted when enabled is set, so the master
// switch alone decides whether anything is held at all.
func ModeFor(enabled, keepScreenOn bool) Mode {
	if !enabled {
		return ModeOff
	}
	if keepScreenOn {
		return ModeDisplay
	}
	return ModeSystem
}

// ParseMode converts a wire string back to a Mode. Unrecognized input
// yields (ModeOff, false): a consumer that cannot understand a directive
// must fall back to holding nothing, never to holding something.
func ParseMode(s string) (Mode, bool) {
	m := Mode(s)
	if !m.Valid() {
		return ModeOff, false
	}
	return m, true
}

// backend is the OS layer. set makes the OS state match mode EXACTLY,
// starting from whatever state the backend is already in — implementations
// own their own transition handling, and the controller guarantees they
// only ever see a genuine change. A non-nil error means the OS state did
// not reach mode, which is what makes the next Apply retry.
type backend interface {
	set(mode Mode) error
}

// noopBackend is the inert implementation: WSL (where the inhibitor lives
// in the Windows launcher instead) and any GOOS without one of its own.
type noopBackend struct{}

func (noopBackend) set(Mode) error { return nil }

// refusingBackend is what a TEST binary gets. Tests must never reach a
// real D-Bus bus, spawn caffeinate(8), or move the calling machine's
// execution state — the same rule that keeps fixtures away from real
// provider binaries (root CLAUDE.md, "Permanent invariants"). The refusal
// is loud rather than silent so a fixture that starts depending on the
// real thing fails visibly instead of quietly mutating the developer's
// machine. Package-internal tests fake the backend seam directly; the
// App-level seam is (*App).keepAwakeApply.
type refusingBackend struct{}

func (refusingBackend) set(mode Mode) error {
	return fmt.Errorf("power: refusing to set OS sleep inhibitor to %q inside a test binary", mode)
}

// controller is the platform-neutral state machine. It is the only thing
// that decides whether the OS layer is called at all.
type controller struct {
	mu sync.Mutex
	// newBackend builds the OS layer on first genuine need. Deferred
	// rather than eager because a backend can own real resources — the
	// windows one parks a dedicated OS thread for the process lifetime —
	// and a user who never turns the feature on should pay none of it.
	newBackend func() backend
	backend    backend
	current    Mode
}

func (c *controller) apply(mode Mode) error {
	if !mode.Valid() {
		return fmt.Errorf("power: unknown keep-awake mode %q", string(mode))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if mode == c.effectiveLocked() {
		// Already there. Re-asserting is not merely wasteful on every
		// platform: on linux it would drop and re-take the inhibitor,
		// opening a window where the machine is free to sleep.
		return nil
	}
	if c.backend == nil {
		// current starts at ModeOff, so the equality check above means
		// the first backend build is always for a real hold.
		c.backend = c.newBackend()
	}
	if err := c.backend.set(mode); err != nil {
		// current deliberately does NOT advance: the OS is in an unknown
		// or unchanged state, and the next Apply with the same mode must
		// be a retry rather than a no-op.
		return err
	}
	c.current = mode
	return nil
}

func (c *controller) currentMode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effectiveLocked()
}

// effectiveLocked reads current as ModeOff until something is actually
// held. A zero-value controller has held nothing, which is exactly what
// ModeOff means — and reading it that way is what keeps a fresh
// controller's first apply(ModeOff) from building a backend to release
// nothing.
func (c *controller) effectiveLocked() Mode {
	if c.current == "" {
		return ModeOff
	}
	return c.current
}

// std is the process-wide controller. There is exactly one OS sleep state
// per process, so the package owns it rather than handing out instances.
var std = &controller{newBackend: newGuardedBackend}

// newGuardedBackend is the single point where a test binary is kept away
// from real system state.
func newGuardedBackend() backend {
	if testing.Testing() {
		return refusingBackend{}
	}
	return newOSBackend()
}

// Apply puts the OS sleep inhibitor into mode. Safe to call from any
// goroutine and from any prior state; a repeat of the current mode does
// nothing.
func Apply(mode Mode) error { return std.apply(mode) }

// Current reports the last mode Apply successfully reached.
func Current() Mode { return std.currentMode() }

// errNoInhibitorAvailable is returned by a backend that reached no
// working mechanism at all. Callers log it; the feature is best-effort by
// nature (a desktop with neither a session manager nor logind simply
// cannot be told to stay awake).
var errNoInhibitorAvailable = errors.New("power: no sleep inhibitor available")
