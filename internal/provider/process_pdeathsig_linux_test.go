//go:build linux

package provider

import (
	"context"
	"syscall"
	"testing"
)

// TestApplySysProcAttrSetsPdeathsigOnLinux guards the platform-split: the
// Linux path must keep delivering SIGTERM to provider children when the
// parent dies. Dropping Pdeathsig from process_linux.go would silently
// lose ungraceful-death cleanup on Linux with no other failing test — the
// shared TestSetpgid only checks Setpgid.
func TestApplySysProcAttrSetsPdeathsigOnLinux(t *testing.T) {
	p, err := Spawn(context.Background(), SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	defer p.Kill()

	if p.cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if p.cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Errorf("Pdeathsig = %v, want SIGTERM", p.cmd.SysProcAttr.Pdeathsig)
	}
}
