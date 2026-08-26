//go:build nogui

// main_harness_window_nogui.go is the no-window half of the --window
// flag. The WSL backend payload links without the Wails application
// package (build/windows/Taskfile.yml builds it with -tags nogui), so
// there is nothing here to attach a window to — the honest answer is a
// boot-time refusal naming the build that can.
//
// The implementation lives in main_harness_window.go behind !nogui.
package main

import (
	"agent-overflow/internal/transport"
)

func requireWindowedBuild() {
	fatalf("--window needs a GUI build; this binary is the headless WSL backend payload (built with -tags nogui) and links no webview. Build the desktop binary (`make harness-build`) and run it there, or drop --window to boot headless.")
}

// runWindowedShell is unreachable: requireWindowedBuild exits before any
// caller gets here. It exists so both build tags carry the same symbols.
func runWindowedShell(_ *App, _ *transport.Server, _ string) error {
	requireWindowedBuild()
	return nil
}
