//go:build linux

// keepawake_linux.go is the D-Bus inhibitor. Two tiers, tried in order,
// because no single interface is present on every desktop:
//
//  1. org.gnome.SessionManager.Inhibit — one call covers both legs
//     (flag 4 = suspend, flag 8 = idle/screen) and one cookie releases
//     them. Present on GNOME, and on the several desktops that
//     re-implement the interface.
//  2. org.freedesktop.ScreenSaver.Inhibit (session bus) for the screen
//     leg + org.freedesktop.login1.Manager.Inhibit("sleep", …, "block")
//     (system bus) for the machine leg. The ScreenSaver interface is the
//     one KDE/XFCE/Cinnamon all implement; logind is what actually stops
//     systemd suspending the box, and it hands back a FILE DESCRIPTOR
//     rather than a cookie — holding the fd open IS the inhibit, and
//     closing it (or dying) releases it.
//
// Everything is best-effort: a missing service is a logged line, not a
// failure, and only a mode where NOTHING could be taken returns an error
// (which makes the next Apply retry rather than believe it succeeded).
//
// On WSL this file's backend is never built — see newOSBackend.
package power

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"agent-overflow/internal/platform"

	"github.com/godbus/dbus/v5"
)

const (
	// dbusAppID identifies us to the session manager / screensaver. It is
	// what a "what is keeping this machine awake?" listing shows.
	dbusAppID = "Agent Overflow"
	// inhibitReason is the human-readable half of that listing.
	inhibitReason = "Agent Overflow keep awake is on"
)

// GNOME session-manager inhibit flags (org.gnome.SessionManager, flag 4 =
// suspend, flag 8 = session idle). We never use the logout/switch-user
// flags: this feature is about sleep, not about blocking the user.
const (
	gnomeInhibitSuspend uint32 = 4
	gnomeInhibitIdle    uint32 = 8
)

// D-Bus names, paths and methods, spelled once.
const (
	gnomeSessionDest      = "org.gnome.SessionManager"
	gnomeSessionPath      = dbus.ObjectPath("/org/gnome/SessionManager")
	gnomeInhibitMethod    = "org.gnome.SessionManager.Inhibit"
	gnomeUninhibitMethod  = "org.gnome.SessionManager.Uninhibit"
	screenSaverDest       = "org.freedesktop.ScreenSaver"
	screenSaverPath       = dbus.ObjectPath("/org/freedesktop/ScreenSaver")
	screenSaverInhibit    = "org.freedesktop.ScreenSaver.Inhibit"
	screenSaverUninhibit  = "org.freedesktop.ScreenSaver.UnInhibit"
	login1Dest            = "org.freedesktop.login1"
	login1Path            = dbus.ObjectPath("/org/freedesktop/login1")
	login1InhibitMethod   = "org.freedesktop.login1.Manager.Inhibit"
	login1InhibitWhat     = "sleep"
	login1InhibitBehavior = "block"
)

// busConn is the slice of *dbus.Conn this file uses. Narrow on purpose:
// it is the seam the tests fake, and it deliberately has no Close — the
// connections come from dbus.SessionBus / dbus.SystemBus, which are
// SHARED, process-wide and must never be closed by a consumer.
type busConn interface {
	Object(dest string, path dbus.ObjectPath) dbus.BusObject
}

func newOSBackend() backend {
	if platform.IsWSL() {
		// A WSL distro has no session bus worth talking to and cannot
		// influence the Windows host's power state regardless. The real
		// inhibitor for this install runs in the Windows launcher,
		// driven by the power:keepawake directive.
		return noopBackend{}
	}
	return newDBusBackend()
}

func newDBusBackend() *dbusBackend {
	return &dbusBackend{
		sessionBus: func() (busConn, error) { return dbus.SessionBus() },
		systemBus:  func() (busConn, error) { return dbus.SystemBus() },
		adoptFD: func(fd dbus.UnixFD) io.Closer {
			return os.NewFile(uintptr(fd), "login1-sleep-inhibit")
		},
		logf: log.Printf,
	}
}

// cookieInhibit is one cookie-shaped hold, remembered with the object it
// was taken on so the release call cannot drift to a different bus.
type cookieInhibit struct {
	obj    dbus.BusObject
	cookie uint32
}

type dbusBackend struct {
	// Seams. Production wires them to the shared buses; tests substitute
	// in-memory fakes so no test ever reaches a real bus.
	sessionBus func() (busConn, error)
	systemBus  func() (busConn, error)
	adoptFD    func(dbus.UnixFD) io.Closer
	logf       func(string, ...any)

	// Held state. At most one tier is populated at a time.
	gnome       *cookieInhibit
	screenSaver *cookieInhibit
	login1      io.Closer
}

// set releases whatever is held and then takes what the new mode needs.
//
// Release-then-acquire rather than a transition matrix: the matrix has
// six edges and three mechanisms, and every one of its cells is a place
// to leak a cookie. The cost is a sub-millisecond window with nothing
// held, on a transition the user just asked for, against idle timers
// measured in minutes. The controller guarantees mode != current, so this
// never runs for a no-op.
func (b *dbusBackend) set(mode Mode) error {
	b.release()
	if mode == ModeOff {
		return nil
	}
	return b.acquire(mode)
}

func (b *dbusBackend) acquire(mode Mode) error {
	flags := gnomeInhibitSuspend
	if mode == ModeDisplay {
		flags |= gnomeInhibitIdle
	}
	if err := b.acquireGnome(flags); err == nil {
		return nil
	} else {
		b.logf("keep awake: gnome session inhibit unavailable (%v); falling back to freedesktop interfaces", err)
	}

	var failures []error
	if mode == ModeDisplay {
		if err := b.acquireScreenSaver(); err != nil {
			failures = append(failures, fmt.Errorf("screensaver inhibit: %w", err))
		}
	}
	if err := b.acquireLogin1(); err != nil {
		failures = append(failures, fmt.Errorf("login1 sleep inhibit: %w", err))
	}
	if b.screenSaver == nil && b.login1 == nil {
		// Nothing at all was taken. Report it so the controller does not
		// record the mode as reached — an identical Apply later (a bus
		// that came up meanwhile, a desktop session that started) then
		// retries instead of short-circuiting.
		return errors.Join(append([]error{errNoInhibitorAvailable}, failures...)...)
	}
	for _, err := range failures {
		// Partial success: the machine leg without the screen leg is
		// still most of what the user asked for.
		b.logf("keep awake: %v", err)
	}
	return nil
}

func (b *dbusBackend) acquireGnome(flags uint32) error {
	conn, err := b.sessionBus()
	if err != nil {
		return err
	}
	obj := conn.Object(gnomeSessionDest, gnomeSessionPath)
	var cookie uint32
	// Signature: Inhibit(app_id s, toplevel_xid u, reason s, flags u) -> u.
	// We have no X11 toplevel to name, and 0 is the documented "none".
	if err := obj.Call(gnomeInhibitMethod, 0, dbusAppID, uint32(0), inhibitReason, flags).Store(&cookie); err != nil {
		return err
	}
	b.gnome = &cookieInhibit{obj: obj, cookie: cookie}
	return nil
}

func (b *dbusBackend) acquireScreenSaver() error {
	conn, err := b.sessionBus()
	if err != nil {
		return err
	}
	obj := conn.Object(screenSaverDest, screenSaverPath)
	var cookie uint32
	if err := obj.Call(screenSaverInhibit, 0, dbusAppID, inhibitReason).Store(&cookie); err != nil {
		return err
	}
	b.screenSaver = &cookieInhibit{obj: obj, cookie: cookie}
	return nil
}

func (b *dbusBackend) acquireLogin1() error {
	conn, err := b.systemBus()
	if err != nil {
		return err
	}
	obj := conn.Object(login1Dest, login1Path)
	var fd dbus.UnixFD
	// Signature: Inhibit(what s, who s, why s, mode s) -> h. The returned
	// fd IS the inhibitor: logind releases it when the last copy closes,
	// which is what makes process death an automatic release.
	if err := obj.Call(login1InhibitMethod, 0, login1InhibitWhat, dbusAppID, inhibitReason, login1InhibitBehavior).Store(&fd); err != nil {
		return err
	}
	b.login1 = b.adoptFD(fd)
	return nil
}

// release drops every hold. Errors are logged and then forgotten: the
// handle is cleared either way, because a cookie the bus has already
// forgotten (service restarted, session ended) must not be retried
// forever, and every one of these also lapses on process death.
func (b *dbusBackend) release() {
	if b.gnome != nil {
		if call := b.gnome.obj.Call(gnomeUninhibitMethod, 0, b.gnome.cookie); call.Err != nil {
			b.logf("keep awake: release gnome inhibit: %v", call.Err)
		}
		b.gnome = nil
	}
	if b.screenSaver != nil {
		if call := b.screenSaver.obj.Call(screenSaverUninhibit, 0, b.screenSaver.cookie); call.Err != nil {
			b.logf("keep awake: release screensaver inhibit: %v", call.Err)
		}
		b.screenSaver = nil
	}
	if b.login1 != nil {
		if err := b.login1.Close(); err != nil {
			b.logf("keep awake: release login1 inhibit fd: %v", err)
		}
		b.login1 = nil
	}
}
