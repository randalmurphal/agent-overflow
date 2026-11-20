//go:build !nogui

package main

import "testing"

// macOS must quit when the last window closes, matching the Wails default
// on Linux/Windows. The framework defaults this to FALSE on macOS; without
// the override, closing the window leaves a headless backend (transport
// server, SQLite handle, provider subprocesses) running with no tray, menu,
// or reopenable window — ServiceShutdown never fires until Force Quit.
func TestDesktopApplicationOptionsQuitOnLastWindowClosed(t *testing.T) {
	opts := desktopApplicationOptions("Agent Overflow")
	if !opts.Mac.ApplicationShouldTerminateAfterLastWindowClosed {
		t.Fatal("Mac.ApplicationShouldTerminateAfterLastWindowClosed must be true: closing the last window on macOS would otherwise leave a zombie headless backend")
	}
	if opts.Name != "Agent Overflow" {
		t.Fatalf("Name = %q, want %q", opts.Name, "Agent Overflow")
	}
}
