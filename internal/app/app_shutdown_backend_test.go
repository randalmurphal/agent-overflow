package app

import (
	"errors"
	"testing"
	"time"
)

// TestShutdownBackendRepeatsSafely covers the launcher retrying, or a
// window close racing its own backstop: every call reaches the installed
// path, which owns the once-only teardown. The RPC hands each request off
// and answers, so the acknowledgement reaches the caller over a transport
// the teardown is about to drain.
func TestShutdownBackendRepeatsSafely(t *testing.T) {
	t.Parallel()
	requests := make(chan struct{}, 4)
	a := &App{}
	ConfigureBackendShutdown(a, func() error {
		requests <- struct{}{}
		return nil
	})

	for range 2 {
		if err := a.ShutdownBackend(); err != nil {
			t.Fatalf("ShutdownBackend: %v", err)
		}
	}
	for range 2 {
		select {
		case <-requests:
		case <-time.After(2 * time.Second):
			t.Fatal("a repeated shutdown request was dropped")
		}
	}
}

// TestShutdownBackendRefusesWithoutAnInstalledPath is the boot boundary: a
// shell that owns its own quit (the desktop window, the serve host's
// service manager) must not have its process ended through this door, and
// the caller has to learn that rather than wait for an exit that is not
// coming.
func TestShutdownBackendRefusesWithoutAnInstalledPath(t *testing.T) {
	t.Parallel()
	a := &App{}
	if err := a.ShutdownBackend(); !errors.Is(err, ErrShutdownNotOwned) {
		t.Fatalf("ShutdownBackend on a boot with no door = %v, want %v", err, ErrShutdownNotOwned)
	}
}
