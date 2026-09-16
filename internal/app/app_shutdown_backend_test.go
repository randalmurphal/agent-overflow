package app

import (
	"errors"
	"testing"
	"time"
)

// TestShutdownBackendHandsTheRequestOffAndAnswersFirst pins the shape the
// launcher depends on: the RPC returns while the shutdown is still to come,
// so the acknowledgement reaches the caller over a transport the teardown
// is about to drain.
func TestShutdownBackendHandsTheRequestOffAndAnswersFirst(t *testing.T) {
	requested := make(chan struct{})
	a := &App{}
	ConfigureBackendShutdown(a, func() error {
		close(requested)
		return nil
	})

	if err := a.ShutdownBackend(); err != nil {
		t.Fatalf("ShutdownBackend: %v", err)
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("the installed shutdown path was never asked to run")
	}
}

// TestShutdownBackendRepeatsSafely covers the launcher retrying, or a
// window close racing its own backstop: every call reaches the installed
// path, which owns the once-only teardown.
func TestShutdownBackendRepeatsSafely(t *testing.T) {
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
	a := &App{}
	if err := a.ShutdownBackend(); !errors.Is(err, ErrShutdownNotOwned) {
		t.Fatalf("ShutdownBackend on a boot with no door = %v, want %v", err, ErrShutdownNotOwned)
	}
}
