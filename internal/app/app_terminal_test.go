package app

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/terminal"
)

// newAppWithTerminals constructs an App with a live terminal manager but no
// Wails runtime, so output/exit callbacks become no-ops (they guard on
// a.app == nil).
func newAppWithTerminals() *App {
	app := NewApp()
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	return app
}

func TestOpenTerminalRequiresCwd(t *testing.T) {
	app := newAppWithTerminals()
	_, err := app.OpenTerminal("thread-a", TerminalOpenOptions{})
	if err == nil {
		t.Fatal("expected error for missing cwd")
	}
}

func TestOpenTerminalReturnsHandle(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-a", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	if handle.TerminalID == "" {
		t.Fatal("expected non-empty terminal ID")
	}
	if handle.ThreadID != "thread-a" {
		t.Fatalf("ThreadID = %q", handle.ThreadID)
	}
	if handle.Summary.Shell == "" {
		t.Error("expected summary to include resolved shell")
	}
}

func TestWriteTerminalDecodesBase64(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-w", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("echo hi\n"))
	if err := app.WriteTerminal(handle.TerminalID, payload); err != nil {
		t.Fatalf("WriteTerminal: %v", err)
	}
}

func TestWriteTerminalRejectsBadBase64(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-b", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	err = app.WriteTerminal(handle.TerminalID, "not base64!")
	if err == nil {
		t.Fatal("expected WriteTerminal to reject invalid base64")
	}
	if !strings.Contains(err.Error(), "decode write payload") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestResizeAndCloseTerminal(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-r", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}

	if err := app.ResizeTerminal(handle.TerminalID, 40, 140); err != nil {
		t.Fatalf("ResizeTerminal: %v", err)
	}
	list, err := app.ListTerminals("thread-r")
	if err != nil {
		t.Fatalf("ListTerminals: %v", err)
	}
	if len(list) != 1 || list[0].Rows != 40 || list[0].Cols != 140 {
		t.Fatalf("unexpected list: %+v", list)
	}

	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	list, err = app.ListTerminals("thread-r")
	if err != nil {
		t.Fatalf("ListTerminals after close: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no terminals after close, got %d", len(list))
	}
}

func TestMoveThreadTerminalsRekeysSessions(t *testing.T) {
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })
	thread := testThread("thread-real")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	handle, err := app.OpenTerminal("draft:thread", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}

	moved, err := app.MoveThreadTerminals("draft:thread", "thread-real")
	if err != nil {
		t.Fatalf("MoveThreadTerminals: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved summaries = %d, want 1", len(moved))
	}
	if moved[0].TerminalID != handle.TerminalID {
		t.Fatalf("moved TerminalID = %q, want %q", moved[0].TerminalID, handle.TerminalID)
	}
	if moved[0].ThreadID != "thread-real" {
		t.Fatalf("moved ThreadID = %q, want thread-real", moved[0].ThreadID)
	}
	oldList, err := app.ListTerminals("draft:thread")
	if err != nil {
		t.Fatalf("ListTerminals(old): %v", err)
	}
	if len(oldList) != 0 {
		t.Fatalf("old thread key terminals = %d, want 0", len(oldList))
	}
	newList, err := app.ListTerminals("thread-real")
	if err != nil {
		t.Fatalf("ListTerminals(new): %v", err)
	}
	if len(newList) != 1 || newList[0].TerminalID != handle.TerminalID {
		t.Fatalf("new thread key list = %+v, want moved terminal", newList)
	}
}

func TestMoveThreadTerminalsRequiresDraftSourceAndRealTarget(t *testing.T) {
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	if _, err := app.MoveThreadTerminals("thread-real", "thread-target"); err == nil {
		t.Fatal("expected non-draft source to be rejected")
	}
	if _, err := app.MoveThreadTerminals("draft:thread", "missing-thread"); err == nil {
		t.Fatal("expected missing target thread to be rejected")
	}
	if err := app.CloseThreadTerminals("thread-real"); err == nil {
		t.Fatal("expected non-draft close to be rejected")
	}
}

func TestRestartTerminalReturnsNewHandle(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-rs", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	restarted, err := app.RestartTerminal(handle.TerminalID)
	if err != nil {
		t.Fatalf("RestartTerminal: %v", err)
	}
	if restarted.TerminalID == handle.TerminalID {
		t.Fatal("expected restart to yield a different terminal ID")
	}
}

func TestGetTerminalReplayReturnsBase64(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	handle, err := app.OpenTerminal("thread-g", TerminalOpenOptions{
		Cwd:   t.TempDir(),
		Shell: "/bin/sh",
	})
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	t.Cleanup(func() { _ = app.CloseTerminal(handle.TerminalID) })

	if err := app.WriteTerminal(handle.TerminalID, base64.StdEncoding.EncodeToString([]byte("printf HELLO\n"))); err != nil {
		t.Fatalf("WriteTerminal: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		replay, err := app.GetTerminalReplay(handle.TerminalID)
		if err != nil {
			t.Fatalf("GetTerminalReplay: %v", err)
		}
		raw, decodeErr := base64.StdEncoding.DecodeString(replay.Data)
		if decodeErr != nil {
			t.Fatalf("bad base64 from GetTerminalReplay: %v", decodeErr)
		}
		if strings.Contains(string(raw), "HELLO") {
			if replay.ThroughSequence == 0 {
				t.Fatalf("expected replay sequence watermark to advance")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("did not observe HELLO in replay within timeout")
}

func TestWriteTerminalMissingBindingFails(t *testing.T) {
	// When terminal manager isn't initialized, every binding should report that.
	app := &App{}
	_, err := app.OpenTerminal("t", TerminalOpenOptions{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("expected error when manager not initialized")
	}
	if !strings.Contains(err.Error(), "terminal manager not initialized") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestResizeTerminalOnMissingIsErrorFromManager(t *testing.T) {
	app := newAppWithTerminals()
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	err := app.ResizeTerminal("does-not-exist", 24, 80)
	if !errors.Is(err, terminal.ErrTerminalNotFound) {
		t.Fatalf("expected ErrTerminalNotFound, got %v", err)
	}
}

// TestTerminalExitCallback_EmitsWhenRunning confirms a real, running-time
// terminal exit (ctrl+D or last-tab close) reaches the frontend as a
// `terminal:exit` event with the status copied across. That event is the
// single seam the frontend uses to close the terminal pane (#2) and drop the
// terminal thread from the sidebar (#3); dropping it would strand both.
func TestTerminalExitCallback_EmitsWhenRunning(t *testing.T) {
	app := NewApp()

	got := make(chan TerminalExitEvent, 1)
	app.testEmitHook = func(name string, data any) {
		if name != "terminal:exit" {
			return
		}
		evt, ok := data.(TerminalExitEvent)
		if !ok {
			t.Errorf("terminal:exit payload type = %T, want TerminalExitEvent", data)
			return
		}
		select {
		case got <- evt:
		default:
		}
	}

	app.terminalExitCallback("thread-x", "term-x", terminal.ExitStatus{Code: 137, Reason: "signal:SIGKILL"})

	select {
	case evt := <-got:
		if evt.ThreadID != "thread-x" || evt.TerminalID != "term-x" {
			t.Fatalf("ids = (%q,%q), want (thread-x,term-x)", evt.ThreadID, evt.TerminalID)
		}
		if evt.Code != 137 || evt.Reason != "signal:SIGKILL" {
			t.Fatalf("status = (%d,%q), want (137,signal:SIGKILL)", evt.Code, evt.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("no terminal:exit event emitted for a running-time exit")
	}
}

// TestTerminalExitCallback_SkipsOnShutdown pins the shutdown-suppression
// guard. Manager.Shutdown SIGTERMs every PTY during app quit, firing each
// session's exit callback; without the guard those mass-kill exits would
// reach the frontend and wrongly delete every terminal thread from the
// sidebar. Terminal threads must persist across restart, so a shutdown-time
// exit must NOT emit. A regression that drops the guard fails here.
func TestTerminalExitCallback_SkipsOnShutdown(t *testing.T) {
	app := NewApp()
	app.shuttingDown.Store(true)

	emitted := false
	app.testEmitHook = func(string, any) { emitted = true }

	app.terminalExitCallback("thread-x", "term-x", terminal.ExitStatus{})

	if emitted {
		t.Error("terminal:exit should not fire after shuttingDown is set")
	}
}
