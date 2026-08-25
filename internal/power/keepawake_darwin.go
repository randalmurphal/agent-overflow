//go:build darwin

// keepawake_darwin.go holds the assertion with caffeinate(8), the same
// tool the platform ships for exactly this purpose.
//
//	caffeinate -i        prevent system idle sleep
//	caffeinate -i -d     …and prevent the display sleeping
//	caffeinate -w <pid>  exit when <pid> exits
//
// `-w os.Getpid()` is the failsafe and the reason a child process is a
// better answer here than IOPMAssertionCreateWithName over cgo: caffeinate
// polls the pid and exits on its own if we are SIGKILLed, panic, or are
// force-quit. There is no state to clean up and nothing to leak.
package power

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// caffeinatePath is absolute on purpose — the same reasoning as
// provideraccounts' security(1) call: never let PATH decide which binary
// gets to change machine state.
const caffeinatePath = "/usr/bin/caffeinate"

func newOSBackend() backend {
	return &caffeinateBackend{start: startCaffeinate}
}

type caffeinateBackend struct {
	// start is the spawn seam. Tests substitute it; the production
	// implementation additionally refuses to run inside a test binary at
	// all, so a fixture that forgets the seam fails loudly instead of
	// spawning (the securityCommand precedent in
	// internal/provideraccounts/claude_keychain.go).
	start func(Mode) (*exec.Cmd, error)
	cmd   *exec.Cmd
}

func (b *caffeinateBackend) set(mode Mode) error {
	b.stop()
	if mode == ModeOff {
		return nil
	}
	cmd, err := b.start(mode)
	if err != nil {
		return err
	}
	b.cmd = cmd
	return nil
}

func (b *caffeinateBackend) stop() {
	if b.cmd == nil {
		return
	}
	cmd := b.cmd
	b.cmd = nil
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	// Reap off the caller's goroutine: Wait blocks until the child is
	// gone and this runs on the settings-save path.
	go func() { _ = cmd.Wait() }()
}

func startCaffeinate(mode Mode) (*exec.Cmd, error) {
	if testing.Testing() {
		return nil, errors.New("power: refusing to spawn caffeinate(8) inside a test binary — fake the start seam")
	}
	args := make([]string, 0, 4)
	args = append(args, "-i")
	if mode == ModeDisplay {
		args = append(args, "-d")
	}
	args = append(args, "-w", strconv.Itoa(os.Getpid()))
	cmd := exec.Command(caffeinatePath, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("power: start caffeinate: %w", err)
	}
	return cmd, nil
}
