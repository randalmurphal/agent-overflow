//go:build darwin

package power

import (
	"os/exec"
	"testing"
)

// The spawn is faked at the start seam. No test here runs caffeinate(8):
// startCaffeinate refuses inside a test binary regardless, and this is
// what lets the transition behavior be asserted anyway.
func TestCaffeinateBackendStartsAndStopsAcrossTransitions(t *testing.T) {
	var started []Mode
	stopped := 0
	b := &caffeinateBackend{start: func(mode Mode) (*exec.Cmd, error) {
		started = append(started, mode)
		// A Cmd with no Process is enough: stop() must tolerate it, which
		// is also the real shape after a failed Start.
		return &exec.Cmd{}, nil
	}}
	// Count releases through the handle going away, since the fake never
	// has a process to kill.
	release := func() {
		if b.cmd != nil {
			stopped++
		}
	}

	if err := b.set(ModeSystem); err != nil {
		t.Fatalf("set(system) error = %v", err)
	}
	release()
	if err := b.set(ModeDisplay); err != nil {
		t.Fatalf("set(display) error = %v", err)
	}
	release()
	if err := b.set(ModeOff); err != nil {
		t.Fatalf("set(off) error = %v", err)
	}
	if b.cmd != nil {
		t.Fatal("caffeinate handle survived the release to off")
	}

	if len(started) != 2 || started[0] != ModeSystem || started[1] != ModeDisplay {
		t.Fatalf("started modes = %v, want [system display]", started)
	}
	if stopped != 2 {
		t.Fatalf("released %d holds, want 2 (one per transition off the previous mode)", stopped)
	}
}

// The production spawn must be unreachable from a test binary — the
// securityCommand precedent in internal/provideraccounts.
func TestStartCaffeinateRefusesInsideATestBinary(t *testing.T) {
	cmd, err := startCaffeinate(ModeDisplay)
	if err == nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatal("startCaffeinate() error = nil inside a test binary, want a refusal")
	}
}
