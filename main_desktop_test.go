//go:build !nogui

package main

import (
	"testing"

	"agent-overflow/internal/uikeys"

	"github.com/wailsapp/wails/v3/pkg/application"
)

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

// scriptWindow records ExecJS and reloads; every other Window method is unset.
type scriptWindow struct {
	application.Window
	scripts []string
	reloads int
}

func (w *scriptWindow) ExecJS(js string) { w.scripts = append(w.scripts, js) }
func (w *scriptWindow) Reload()          { w.reloads++ }
func (w *scriptWindow) SetURL(string) application.Window {
	w.reloads++
	return w
}

// The desktop window hands Ctrl/Cmd+R to the SPA while it reports its
// browser pane focused, and reloads the app otherwise.
func TestWebviewShellRoutesReloadToTheFocusedBrowserPane(t *testing.T) {
	shell := webviewShell{
		title:   "Agent Overflow",
		pageURL: func() string { return "http://127.0.0.1:4100/" },
		chords:  uikeys.NewPageChords(),
	}
	opts, err := shell.windowOptions()
	if err != nil {
		t.Fatal(err)
	}
	reload := opts.KeyBindings["CmdOrCtrl+r"]
	w := &scriptWindow{}
	reload(w)
	if w.reloads != 1 || len(w.scripts) != 0 {
		t.Fatalf("unfocused: reloads=%d scripts=%d", w.reloads, len(w.scripts))
	}
	shell.chords.DocumentLoaded(w)
	w.scripts = nil
	shell.chords.HandleMessage(w, uikeys.RouteMessage(1, 1, true))
	reload(w)
	if w.reloads != 1 || len(w.scripts) != 1 || w.scripts[0] != uikeys.DispatchScript {
		t.Fatalf("focused: reloads=%d scripts=%v", w.reloads, w.scripts)
	}
}

// Before the window exists, no raw message reaches the chord state.
func TestWebviewShellRawMessagesNeedTheOwnWindow(t *testing.T) {
	shell := webviewShell{pageURL: func() string { return "http://127.0.0.1:4100/" }, chords: uikeys.NewPageChords()}
	handle := shell.rawMessageHandler(func() *application.WebviewWindow { return nil })
	w := &scriptWindow{}
	shell.chords.DocumentLoaded(w)
	handle(w, uikeys.RouteMessage(1, 1, true), &application.OriginInfo{Origin: "http://127.0.0.1:4100"})
	if shell.chords.Routing() {
		t.Fatal("a message with no own window set the focus")
	}
}
