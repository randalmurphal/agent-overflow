package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// newAppWithTerminals constructs an App with a live terminal manager and a
// chat thread row for each id in threadIDs, since OpenTerminal refuses an id
// with no thread.
func newAppWithTerminals(t *testing.T, threadIDs ...string) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(app.stopTerminalThreadEnds)
	for _, id := range threadIDs {
		createModeThread(t, app, id, threadmode.ModeChat)
	}
	return app
}

func createModeThread(t *testing.T, app *App, id, mode string) {
	t.Helper()
	thread := testThread(id)
	thread.Mode = mode
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", id, err)
	}
}

func TestOpenTerminalRequiresCwd(t *testing.T) {
	t.Parallel()
	app := newAppWithTerminals(t, "thread-a")
	_, err := app.OpenTerminal("thread-a", TerminalOpenOptions{})
	if err == nil {
		t.Fatal("expected error for missing cwd")
	}
}

func TestOpenTerminalReturnsHandle(t *testing.T) {
	t.Parallel()
	app := newAppWithTerminals(t, "thread-a")
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
	t.Parallel()
	app := newAppWithTerminals(t, "thread-w")
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
	t.Parallel()
	app := newAppWithTerminals(t, "thread-b")
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
	t.Parallel()
	app := newAppWithTerminals(t, "thread-r")
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
	t.Parallel()
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
	t.Parallel()
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	if _, err := app.MoveThreadTerminals("thread-real", "thread-target"); err == nil {
		t.Fatal("expected non-draft source to be rejected")
	}
	if _, err := app.MoveThreadTerminals("draft:thread", "missing-thread"); err == nil {
		t.Fatal("expected missing target thread to be rejected")
	}
	if _, err := app.MoveThreadTerminals("draft:thread", "draft:other"); err == nil {
		t.Fatal("expected draft target to be rejected")
	}
	if err := app.CloseThreadTerminals("thread-real"); err == nil {
		t.Fatal("expected non-draft close to be rejected")
	}
}

func TestRestartTerminalReturnsNewHandle(t *testing.T) {
	t.Parallel()
	app := newAppWithTerminals(t, "thread-rs")
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
	t.Parallel()
	app := newAppWithTerminals(t, "thread-g")
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
	t.Parallel()
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
	t.Parallel()
	app := newAppWithTerminals(t)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	err := app.ResizeTerminal("does-not-exist", 24, 80)
	if !errors.Is(err, terminal.ErrTerminalNotFound) {
		t.Fatalf("expected ErrTerminalNotFound, got %v", err)
	}
}

// A terminal exit reaches every client as `terminal:exit` with its status
// copied across. Clients remove the terminal's tab on it.
func TestTerminalExitCallbackEmitsTheExit(t *testing.T) {
	t.Parallel()
	app := NewApp()

	got := make(chan TerminalExitEvent, 1)
	app.testEmitHook = func(name string, data any) {
		if name != string(eventchan.TerminalExit) {
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
		t.Fatal("no terminal:exit event emitted for a terminal exit")
	}
}

// newAppWithReportedTerminalExits is newAppWithTerminals whose manager
// reports each terminal id on the returned channel once the App has handled
// its exit.
func newAppWithReportedTerminalExits(t *testing.T) (*App, <-chan string) {
	t.Helper()
	app, exits, _ := newAppWithReportedTerminalExitsAt(t)
	return app, exits
}

// newAppWithReportedTerminalExitsAt is newAppWithReportedTerminalExits plus
// the database path, for a test that fails a delete from outside the App.
func newAppWithReportedTerminalExitsAt(t *testing.T) (*App, <-chan string, string) {
	t.Helper()
	app, path := newTestAppWithStorePath(t)
	exits := make(chan string, 64)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, func(threadID, terminalID string, status terminal.ExitStatus) {
		app.terminalExitCallback(threadID, terminalID, status)
		exits <- terminalID
	})
	t.Cleanup(app.stopTerminalThreadEnds)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })
	return app, exits, path
}

// execOnStore runs statement on the database at path through its own
// connection, as the tests that inject a failed delete do.
func execOnStore(t *testing.T, path, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

// failDeleteMark makes the delete of threadID fail at its first write,
// before the thread leaves any read.
func failDeleteMark(t *testing.T, path, threadID string) {
	t.Helper()
	execOnStore(t, path, `CREATE TRIGGER fail_delete_mark BEFORE UPDATE OF deleting ON threads
		WHEN NEW.id = '`+threadID+`' BEGIN SELECT RAISE(ABORT, 'injected mark failure'); END`)
}

func openTestTerminal(t *testing.T, app *App, threadID string) TerminalHandle {
	t.Helper()
	handle, err := app.OpenTerminal(threadID, TerminalOpenOptions{Cwd: t.TempDir(), Shell: "/bin/sh"})
	if err != nil {
		t.Fatalf("OpenTerminal(%s): %v", threadID, err)
	}
	return handle
}

// awaitTerminalExit waits for terminalID's exit and for the thread end it
// may have started.
func awaitTerminalExit(t *testing.T, app *App, exits <-chan string, terminalID string) {
	t.Helper()
	select {
	case got := <-exits:
		if got != terminalID {
			t.Fatalf("terminal %s exited, want %s", got, terminalID)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("terminal %s never exited", terminalID)
	}
	app.terminalThreads.wg.Wait()
}

func requireThreadRow(t *testing.T, app *App, threadID string, want bool) {
	t.Helper()
	_, err := app.store.GetThread(threadID)
	switch {
	case want && err != nil:
		t.Fatalf("GetThread(%s) = %v, want the thread kept", threadID, err)
	case !want && !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("GetThread(%s) error = %v, want the thread deleted", threadID, err)
	}
}

// A terminal thread's last shell exiting on its own deletes the thread
// through the ordinary deletion: every client sees the exit, then the
// deletion that closes its panes.
func TestLastTerminalExitDeletesTheTerminalThread(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	snapshot := captureOrderedEmissions(app, string(eventchan.TerminalExit), string(eventchan.ThreadUpdated))

	handle := openTestTerminal(t, app, "thread-term")
	if err := app.WriteTerminal(handle.TerminalID, base64.StdEncoding.EncodeToString([]byte("exit\n"))); err != nil {
		t.Fatalf("WriteTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	requireThreadRow(t, app, "thread-term", false)
	captured := snapshot()
	if got := strings.Join(emissionNames(captured), ","); got != "terminal:exit,thread:updated" {
		t.Fatalf("frames = %s, want the exit then the deletion", got)
	}
	deleted, ok := captured[1].data.(triage.ThreadUpdateEvent)
	if !ok || deleted.Action != triage.ThreadActionDeleted || deleted.ID != "thread-term" {
		t.Fatalf("thread:updated frame = %#v, want the deletion of thread-term", captured[1].data)
	}
	if app.terminalThreads.hasStarted("thread-term") {
		t.Fatal("the deleted thread is still recorded as started")
	}
}

// A terminal thread lives while any of its shells does, and ends with the
// last one, however it ends: here the user closes each tab.
func TestTerminalThreadOutlivesAllButItsLastShell(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	first := openTestTerminal(t, app, "thread-term")
	second := openTestTerminal(t, app, "thread-term")

	if err := app.CloseTerminal(first.TerminalID); err != nil {
		t.Fatalf("CloseTerminal(first): %v", err)
	}
	awaitTerminalExit(t, app, exits, first.TerminalID)
	requireThreadRow(t, app, "thread-term", true)

	if err := app.CloseTerminal(second.TerminalID); err != nil {
		t.Fatalf("CloseTerminal(second): %v", err)
	}
	awaitTerminalExit(t, app, exits, second.TerminalID)
	requireThreadRow(t, app, "thread-term", false)
}

// A chat thread's drawer terminals are tabs: the last one ending leaves the
// thread, and the drawer can open another.
func TestDrawerTerminalExitKeepsTheChatThread(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-chat", threadmode.ModeChat)
	handle := openTestTerminal(t, app, "thread-chat")

	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	requireThreadRow(t, app, "thread-chat", true)
	openTestTerminal(t, app, "thread-chat")
}

// A restart is a close plus an open under the thread's mutation lock, so
// the old shell's exit finds the new one and the terminal thread stays.
func TestRestartTerminalKeepsTheTerminalThread(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	handle := openTestTerminal(t, app, "thread-term")

	restarted, err := app.RestartTerminal(handle.TerminalID)
	if err != nil {
		t.Fatalf("RestartTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	requireThreadRow(t, app, "thread-term", true)
	list, err := app.ListTerminals("thread-term")
	if err != nil {
		t.Fatalf("ListTerminals: %v", err)
	}
	if len(list) != 1 || list[0].TerminalID != restarted.TerminalID {
		t.Fatalf("terminals = %+v, want the restarted one", list)
	}
}

// Shells moved into a terminal thread are its shells: once the last exits,
// the thread has ended and takes no more.
func TestMoveThreadTerminalsIntoATerminalThreadFollowsItsLifetime(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	handle := openTestTerminal(t, app, "draft:terminal")
	if _, err := app.MoveThreadTerminals("draft:terminal", "thread-term"); err != nil {
		t.Fatalf("MoveThreadTerminals: %v", err)
	}
	app.stopTerminalThreadEnds()
	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	openTestTerminal(t, app, "draft:late")
	if _, err := app.MoveThreadTerminals("draft:late", "thread-term"); !errors.Is(err, errTerminalThreadEnded) {
		t.Fatalf("MoveThreadTerminals(ended) error = %v, want errTerminalThreadEnded", err)
	}
	if list, _ := app.ListTerminals("thread-term"); len(list) != 0 {
		t.Fatalf("ended terminal thread got terminals %+v", list)
	}
}

// A terminal thread whose ending delete fails before the thread leaves any
// read stays listed and ended. No caller waits on that delete, so every
// client is told it could not be removed, and it takes no new shell.
func TestFailedTerminalThreadEndIsReportedAndTheThreadStaysEnded(t *testing.T) {
	t.Parallel()
	app, exits, path := newAppWithReportedTerminalExitsAt(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	failDeleteMark(t, path, "thread-term")
	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated), string(eventchan.TerminalEndFailed))

	handle := openTestTerminal(t, app, "thread-term")
	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	if _, err := app.threadApplication().Get("thread-term"); err != nil {
		t.Fatalf("Get after the failed end: %v, want the thread still listed", err)
	}
	captured := snapshot()
	if got := strings.Join(emissionNames(captured), ","); got != "terminal:end_failed" {
		t.Fatalf("frames = %s, want only the failure report", got)
	}
	report, ok := captured[0].data.(TerminalEndFailedEvent)
	if !ok || report.ThreadID != "thread-term" ||
		report.Message != `The terminal "Test Thread" ended, but it could not be removed. Delete it from the sidebar to try again.` {
		t.Fatalf("terminal:end_failed frame = %#v", captured[0].data)
	}
	if _, err := app.OpenTerminal("thread-term", TerminalOpenOptions{Cwd: t.TempDir(), Shell: "/bin/sh"}); !errors.Is(err, errTerminalThreadEnded) {
		t.Fatalf("OpenTerminal after the failed end = %v, want errTerminalThreadEnded", err)
	}
}

// One that fails after the thread left every read has taken it from every
// client all the same, and the next boot finishes it. Every client is told
// its cleanup failed.
func TestTerminalThreadEndFailingPastItsMarkStillDropsTheThread(t *testing.T) {
	t.Parallel()
	app, exits, path := newAppWithReportedTerminalExitsAt(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	execOnStore(t, path, `CREATE TRIGGER fail_drop BEFORE DELETE ON threads
		WHEN OLD.id = 'thread-term' BEGIN SELECT RAISE(ABORT, 'injected drop failure'); END`)
	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated), string(eventchan.TerminalEndFailed))

	handle := openTestTerminal(t, app, "thread-term")
	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)

	captured := snapshot()
	if got := strings.Join(emissionNames(captured), ","); got != "thread:updated,terminal:end_failed" {
		t.Fatalf("frames = %s, want the deletion then the failure report", got)
	}
	if deleted, ok := captured[0].data.(triage.ThreadUpdateEvent); !ok || deleted.Action != triage.ThreadActionDeleted || deleted.ID != "thread-term" {
		t.Fatalf("thread:updated frame = %#v, want the deletion of thread-term", captured[0].data)
	}
	if report, ok := captured[1].data.(TerminalEndFailedEvent); !ok ||
		report.Message != `The terminal "Test Thread" ended and was removed, but part of its cleanup failed.` {
		t.Fatalf("terminal:end_failed frame = %#v", captured[1].data)
	}
	if pending, err := app.store.ListPendingThreadDeletes(); err != nil || !slices.Equal(pending, []string{"thread-term"}) {
		t.Fatalf("pending deletes = %v, %v; want the thread for the next boot", pending, err)
	}
}

// Between a terminal thread's last exit and its deletion, a pane mounting
// the thread must not give it a new shell. A thread's first shell, a chat
// thread's drawer and a draft's drawer open; an id with no thread does not.
func TestOpenTerminalRefusesAnEndedTerminalThread(t *testing.T) {
	t.Parallel()
	app, exits := newAppWithReportedTerminalExits(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	handle := openTestTerminal(t, app, "thread-term")
	// Hold the end, so the thread stays in the window before its delete.
	app.stopTerminalThreadEnds()
	if err := app.CloseTerminal(handle.TerminalID); err != nil {
		t.Fatalf("CloseTerminal: %v", err)
	}
	awaitTerminalExit(t, app, exits, handle.TerminalID)
	requireThreadRow(t, app, "thread-term", true)

	if _, err := app.OpenTerminal("thread-term", TerminalOpenOptions{Cwd: t.TempDir(), Shell: "/bin/sh"}); !errors.Is(err, errTerminalThreadEnded) {
		t.Fatalf("OpenTerminal(ended) error = %v, want errTerminalThreadEnded", err)
	}
	if list, _ := app.ListTerminals("thread-term"); len(list) != 0 {
		t.Fatalf("ended terminal thread got terminals %+v", list)
	}
	if _, err := app.OpenTerminal("thread-missing", TerminalOpenOptions{Cwd: t.TempDir(), Shell: "/bin/sh"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("OpenTerminal(missing) error = %v, want sql.ErrNoRows", err)
	}
	openTestTerminal(t, app, "draft:terminal")
}

// Every terminal thread the last run left ends at boot, archived or not,
// through the ordinary deletion. Chat threads stay.
func TestBootEndsTheTerminalThreadsTheLastRunLeft(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	archived := testThread("thread-term-archived")
	archived.Mode = threadmode.ModeTerminal
	archived.Archived = true
	if err := app.store.CreateThread(archived); err != nil {
		t.Fatal(err)
	}
	createModeThread(t, app, "thread-chat", threadmode.ModeChat)
	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated))

	if err := app.endTerminalThreadsAtBoot(); err != nil {
		t.Fatalf("endTerminalThreadsAtBoot: %v", err)
	}

	requireThreadRow(t, app, "thread-term", false)
	requireThreadRow(t, app, "thread-term-archived", false)
	requireThreadRow(t, app, "thread-chat", true)
	var deleted []string
	for _, frame := range snapshot() {
		if evt, ok := frame.data.(triage.ThreadUpdateEvent); ok && evt.Action == triage.ThreadActionDeleted {
			deleted = append(deleted, evt.ID)
		}
	}
	slices.Sort(deleted)
	if strings.Join(deleted, ",") != "thread-term,thread-term-archived" {
		t.Fatalf("deleted frames = %v, want both terminal threads", deleted)
	}
}

// A terminal thread whose delete fails at boot stays ended: its shells
// ended with the last run, so a pane mounting it is refused a shell rather
// than given a fresh one in the thread's folder.
func TestBootLeavesATerminalThreadItCouldNotDeleteEnded(t *testing.T) {
	t.Parallel()
	app, path := newTestAppWithStorePath(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(app.stopTerminalThreadEnds)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })
	createModeThread(t, app, "thread-term", threadmode.ModeTerminal)
	failDeleteMark(t, path, "thread-term")

	if err := app.endTerminalThreadsAtBoot(); err == nil || !strings.Contains(err.Error(), "injected mark failure") {
		t.Fatalf("endTerminalThreadsAtBoot = %v, want the injected failure", err)
	}
	requireThreadRow(t, app, "thread-term", true)
	if _, err := app.OpenTerminal("thread-term", TerminalOpenOptions{Cwd: t.TempDir(), Shell: "/bin/sh"}); !errors.Is(err, errTerminalThreadEnded) {
		t.Fatalf("OpenTerminal after the failed boot delete = %v, want errTerminalThreadEnded", err)
	}
	if list, _ := app.ListTerminals("thread-term"); len(list) != 0 {
		t.Fatalf("ended terminal thread got terminals %+v", list)
	}
}

// Start ends the last run's terminal threads before the phases that let a
// client in.
func TestStartEndsTheTerminalThreadsTheLastRunLeft(t *testing.T) {
	a := newBootTestApp(t)
	dbPath := filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName)
	migratedDatabaseAt(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{ID: defaultTestProjectID, Path: "/tmp/workspace", Name: "Project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for id, mode := range map[string]string{"thread-term": threadmode.ModeTerminal, "thread-chat": threadmode.ModeChat} {
		thread := testThread(id)
		thread.Mode = mode
		if err := st.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	progress := newRecordingBootProgress("app.start_background_work")
	SetBootProgress(a, progress)
	a.startAsync(context.Background())
	select {
	case <-progress.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("Start never reached its background work")
	}
	_, termErr := a.store.GetThread("thread-term")
	_, chatErr := a.store.GetThread("thread-chat")
	failures := progress.failures()
	close(progress.release)
	a.stopAsyncStart()
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !errors.Is(termErr, sql.ErrNoRows) {
		t.Fatalf("terminal thread after boot: %v, want deleted", termErr)
	}
	if chatErr != nil {
		t.Fatalf("chat thread after boot: %v, want kept", chatErr)
	}
	if len(failures) != 0 {
		t.Fatalf("boot failures = %v", failures)
	}
}
