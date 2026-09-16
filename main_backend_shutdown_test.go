package main

import (
	"testing"
	"time"
)

// TestArmBackendShutdownDoorWakesTheSignalWait pins the root half of the
// launcher's window-close path: the RPC the launcher calls has to reach
// the wait that owns this process's ordered teardown. Without this seam
// the backend is only ever killed, and the next boot settles its in-flight
// turns as interrupted.
func TestArmBackendShutdownDoorWakesTheSignalWait(t *testing.T) {
	appService := NewApp()
	requested := armBackendShutdownDoor(appService)

	select {
	case <-requested:
		t.Fatal("the wait was woken before anything asked for a shutdown")
	default:
	}

	if err := appService.ShutdownBackend(); err != nil {
		t.Fatalf("ShutdownBackend: %v", err)
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("ShutdownBackend did not wake the signal wait")
	}

	// A retry, or a window close racing its own backstop, must stay a
	// no-op rather than blocking the RPC handler on a closed channel.
	if err := appService.ShutdownBackend(); err != nil {
		t.Fatalf("second ShutdownBackend: %v", err)
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown channel did not stay closed")
	}
}
