//go:build linux

package power

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

// The bus is faked at the connection seam. No test here opens a socket:
// a real session or system bus would mean a developer's desktop actually
// taking (and, worse, possibly leaking) a sleep inhibitor during
// `make go-test`.

type fakeCall struct {
	method string
	args   []any
}

type fakeObject struct {
	dbus.BusObject
	bus    *fakeBus
	dest   string
	unsupp map[string]bool
	ret    map[string][]any
}

func (o *fakeObject) Call(method string, _ dbus.Flags, args ...any) *dbus.Call {
	o.bus.calls = append(o.bus.calls, fakeCall{method: method, args: args})
	if o.unsupp[method] {
		return &dbus.Call{Err: errors.New("org.freedesktop.DBus.Error.ServiceUnknown: " + o.dest)}
	}
	return &dbus.Call{Body: o.ret[method]}
}

type fakeBus struct {
	calls []fakeCall
	// unsupported names the methods this bus answers with a
	// ServiceUnknown error, which is how an absent desktop service
	// actually presents.
	unsupported map[string]bool
	returns     map[string][]any
}

func (b *fakeBus) Object(dest string, _ dbus.ObjectPath) dbus.BusObject {
	return &fakeObject{bus: b, dest: dest, unsupp: b.unsupported, ret: b.returns}
}

func (b *fakeBus) methods() []string {
	out := make([]string, 0, len(b.calls))
	for _, call := range b.calls {
		out = append(out, call.method)
	}
	return out
}

func (b *fakeBus) argsFor(method string) []any {
	for _, call := range b.calls {
		if call.method == method {
			return call.args
		}
	}
	return nil
}

type fakeFD struct{ closed int }

func (f *fakeFD) Close() error { f.closed++; return nil }

// newFakeDBusBackend wires a backend onto two fake buses. Both start
// fully capable; a test disables what its scenario lacks.
func newFakeDBusBackend() (*dbusBackend, *fakeBus, *fakeBus, *fakeFD) {
	session := &fakeBus{
		unsupported: map[string]bool{},
		returns: map[string][]any{
			gnomeInhibitMethod: {uint32(41)},
			screenSaverInhibit: {uint32(42)},
		},
	}
	system := &fakeBus{
		unsupported: map[string]bool{},
		returns:     map[string][]any{login1InhibitMethod: {dbus.UnixFD(7)}},
	}
	fd := &fakeFD{}
	b := &dbusBackend{
		sessionBus: func() (busConn, error) { return session, nil },
		systemBus:  func() (busConn, error) { return system, nil },
		adoptFD:    func(dbus.UnixFD) io.Closer { return fd },
		logf:       func(string, ...any) {},
	}
	return b, session, system, fd
}

// Tier 1: one GNOME call covers both legs, and the flags are where the
// screen axis actually lives.
func TestDBusBackendPrefersGnomeAndEncodesTheScreenAxisInFlags(t *testing.T) {
	t.Run("system mode asks for suspend only", func(t *testing.T) {
		b, session, system, _ := newFakeDBusBackend()
		if err := b.set(ModeSystem); err != nil {
			t.Fatalf("set(system) error = %v", err)
		}
		args := session.argsFor(gnomeInhibitMethod)
		if len(args) != 4 {
			t.Fatalf("gnome Inhibit args = %v, want 4 (app_id, xid, reason, flags)", args)
		}
		if got := args[3].(uint32); got != gnomeInhibitSuspend {
			t.Fatalf("gnome flags = %d, want %d (suspend only)", got, gnomeInhibitSuspend)
		}
		// Tier 1 succeeded, so the fallback interfaces must not be touched
		// at all — a doubled inhibit is a leak waiting for a release path
		// that only clears one of them.
		if len(system.calls) != 0 {
			t.Fatalf("system bus calls = %v, want none while gnome answered", system.methods())
		}
		if got := session.methods(); len(got) != 1 {
			t.Fatalf("session bus calls = %v, want only the gnome inhibit", got)
		}
	})

	t.Run("display mode adds the idle flag", func(t *testing.T) {
		b, session, _, _ := newFakeDBusBackend()
		if err := b.set(ModeDisplay); err != nil {
			t.Fatalf("set(display) error = %v", err)
		}
		want := gnomeInhibitSuspend | gnomeInhibitIdle
		if got := session.argsFor(gnomeInhibitMethod)[3].(uint32); got != want {
			t.Fatalf("gnome flags = %d, want %d (suspend|idle)", got, want)
		}
	})

	t.Run("release uninhibits the cookie the bus handed back", func(t *testing.T) {
		b, session, _, _ := newFakeDBusBackend()
		if err := b.set(ModeDisplay); err != nil {
			t.Fatalf("set(display) error = %v", err)
		}
		if err := b.set(ModeOff); err != nil {
			t.Fatalf("set(off) error = %v", err)
		}
		args := session.argsFor(gnomeUninhibitMethod)
		if len(args) != 1 || args[0].(uint32) != 41 {
			t.Fatalf("gnome Uninhibit args = %v, want the cookie 41", args)
		}
		if b.gnome != nil {
			t.Fatal("gnome inhibit handle survived release")
		}
	})
}

// Tier 2: no GNOME session manager. The screen leg and the machine leg
// come from two different services on two different buses.
func TestDBusBackendFallsBackToFreedesktopInterfaces(t *testing.T) {
	t.Run("display mode takes both legs", func(t *testing.T) {
		b, session, system, fd := newFakeDBusBackend()
		session.unsupported[gnomeInhibitMethod] = true

		if err := b.set(ModeDisplay); err != nil {
			t.Fatalf("set(display) error = %v", err)
		}
		if b.screenSaver == nil {
			t.Fatal("screensaver inhibit not held in display mode")
		}
		if b.login1 == nil {
			t.Fatal("login1 sleep inhibit not held")
		}
		args := system.argsFor(login1InhibitMethod)
		if len(args) != 4 || args[0].(string) != login1InhibitWhat || args[3].(string) != login1InhibitBehavior {
			t.Fatalf("login1 Inhibit args = %v, want (%q, who, why, %q)", args, login1InhibitWhat, login1InhibitBehavior)
		}

		if err := b.set(ModeOff); err != nil {
			t.Fatalf("set(off) error = %v", err)
		}
		if got := session.argsFor(screenSaverUninhibit); len(got) != 1 || got[0].(uint32) != 42 {
			t.Fatalf("screensaver UnInhibit args = %v, want the cookie 42", got)
		}
		// Closing the fd IS the release — logind holds the inhibit for as
		// long as any copy of it is open, which is why process death
		// releases it for free.
		if fd.closed != 1 {
			t.Fatalf("login1 inhibit fd closed %d times, want 1", fd.closed)
		}
		if b.screenSaver != nil || b.login1 != nil {
			t.Fatal("fallback inhibit handles survived release")
		}
	})

	t.Run("system mode takes only the machine leg", func(t *testing.T) {
		b, session, _, _ := newFakeDBusBackend()
		session.unsupported[gnomeInhibitMethod] = true

		if err := b.set(ModeSystem); err != nil {
			t.Fatalf("set(system) error = %v", err)
		}
		if b.screenSaver != nil {
			t.Fatal("screensaver inhibited in system mode; the display is supposed to be free to blank")
		}
		if b.login1 == nil {
			t.Fatal("login1 sleep inhibit not held in system mode")
		}
	})

	t.Run("a missing screensaver still leaves the machine held", func(t *testing.T) {
		b, session, _, _ := newFakeDBusBackend()
		session.unsupported[gnomeInhibitMethod] = true
		session.unsupported[screenSaverInhibit] = true

		// Partial success is success: most of what the user asked for
		// landed, and reporting failure would make the controller retry
		// forever against a service that does not exist.
		if err := b.set(ModeDisplay); err != nil {
			t.Fatalf("set(display) error = %v, want partial success", err)
		}
		if b.login1 == nil {
			t.Fatal("login1 sleep inhibit not held after the screensaver leg failed")
		}
	})
}

func TestDBusBackendReportsAnErrorWhenNothingCouldBeHeld(t *testing.T) {
	b, session, system, _ := newFakeDBusBackend()
	session.unsupported[gnomeInhibitMethod] = true
	session.unsupported[screenSaverInhibit] = true
	system.unsupported[login1InhibitMethod] = true

	err := b.set(ModeDisplay)
	if err == nil {
		t.Fatal("set(display) error = nil with every interface absent, want a failure so the next apply retries")
	}
	if !errors.Is(err, errNoInhibitorAvailable) {
		t.Fatalf("set(display) error = %v, want it to wrap errNoInhibitorAvailable", err)
	}
	// The diagnosis has to name what was tried; "keep awake didn't work"
	// is not actionable.
	if !strings.Contains(err.Error(), "login1") {
		t.Fatalf("set(display) error = %v, want the underlying failures joined in", err)
	}
}

// Every transition, driven through the same backend instance, asserting
// no hold is ever left behind on a tier change.
func TestDBusBackendReleasesTheOldTierOnEveryTransition(t *testing.T) {
	b, session, _, fd := newFakeDBusBackend()

	// gnome for the first hold…
	if err := b.set(ModeDisplay); err != nil {
		t.Fatalf("set(display) error = %v", err)
	}
	// …then the session manager goes away (a desktop restart), so the
	// next mode lands on the fallback tier.
	session.unsupported[gnomeInhibitMethod] = true
	if err := b.set(ModeSystem); err != nil {
		t.Fatalf("set(system) error = %v", err)
	}
	if b.gnome != nil {
		t.Fatal("gnome cookie survived a transition onto the fallback tier")
	}
	if got := session.argsFor(gnomeUninhibitMethod); len(got) != 1 {
		t.Fatalf("gnome Uninhibit args = %v, want the old cookie released", got)
	}
	if b.login1 == nil {
		t.Fatal("login1 inhibit not held after the tier change")
	}

	if err := b.set(ModeOff); err != nil {
		t.Fatalf("set(off) error = %v", err)
	}
	if b.gnome != nil || b.screenSaver != nil || b.login1 != nil {
		t.Fatal("a hold survived the release to off")
	}
	if fd.closed != 1 {
		t.Fatalf("login1 inhibit fd closed %d times, want 1", fd.closed)
	}
}
