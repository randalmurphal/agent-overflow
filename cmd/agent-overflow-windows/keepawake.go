//go:build windows

// keepawake.go answers the backend's power:keepawake directive.
//
// In the shipped Windows/WSL split the backend lives inside a Linux
// distro: it owns the SETTING, but it cannot make the Win32 call that
// actually stops this machine sleeping. So it emits a mode
// ("off" | "system" | "display") and this process asserts it.
//
// The holder itself is internal/power's windows backend — the SAME code
// the native Windows build of the app runs, deliberately shared rather
// than reimplemented here, because SetThreadExecutionState's per-OS-
// thread semantics (one goroutine parked on a locked OS thread for the
// process lifetime) are exactly the detail a second copy would get
// subtly wrong.
//
// Nothing needs cleaning up on exit: the execution state is process
// state and dies with us. The explicit "off" mode exists for the case
// the user turns the setting off while the launcher keeps running.
package main

import (
	"log"

	"agent-overflow/internal/power"
)

// applyKeepAwakeDirective is the NotificationClientConfig.HandleKeepAwake
// callback. It runs on its own goroutine (the bridge dispatches it off
// the read loop), and the mode string is untrusted wire input — an
// unrecognized value is dropped rather than defaulted, because both
// possible defaults are wrong: guessing "display" pins the machine awake
// on a garbled frame, and guessing "off" silently drops an inhibit the
// user asked for.
func applyKeepAwakeDirective(mode string) {
	parsed, ok := power.ParseMode(mode)
	if !ok {
		log.Printf("keep awake: ignoring unknown mode %q from backend", mode)
		return
	}
	if err := power.Apply(parsed); err != nil {
		log.Printf("keep awake: apply mode %s: %v", parsed, err)
		return
	}
	log.Printf("keep awake: execution state now %s", parsed)
}
