package app

import (
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
)

// A thread error AO cannot save still reaches the user: live on
// thread:unsaved_error and in GetThreadLiveState, never in history.

func newUnsavedErrorApp(t *testing.T, threadID string) (*App, string, *emitRecorder) {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	app.ensureTriageRouter()
	if err := app.store.CreateThread(testThread(threadID)); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return app, dbPath, rec
}

func failErrorRowWrites(t *testing.T, dbPath string) {
	t.Helper()
	execOnFile(t, dbPath, `CREATE TRIGGER fail_error_row BEFORE INSERT ON items WHEN NEW.kind = 'error' BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`)
}

func unsavedErrorFrames(rec *emitRecorder) []UnsavedThreadErrorsEvent {
	var frames []UnsavedThreadErrorsEvent
	for _, call := range rec.snapshot() {
		if call.Channel != eventchan.ThreadUnsavedError.String() {
			continue
		}
		if evt, ok := call.Data.(UnsavedThreadErrorsEvent); ok {
			frames = append(frames, evt)
		}
	}
	return frames
}

func liveUnsavedErrors(t *testing.T, app *App, threadID string) *UnsavedThreadErrorsEvent {
	t.Helper()
	state, err := app.GetThreadLiveState(threadID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	return state.UnsavedErrors
}

func TestUnsavedThreadErrorIsDeliveredLive(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedErrorApp(t, "thread-unsaved")
	failErrorRowWrites(t, dbPath)

	app.emitErrorToThread("thread-unsaved", "session could not be resumed automatically: boom")

	frames := unsavedErrorFrames(rec)
	if len(frames) != 1 {
		t.Fatalf("thread:unsaved_error frames = %+v, want one", frames)
	}
	frame := frames[0]
	if frame.ThreadID != "thread-unsaved" || len(frame.Errors) != 1 {
		t.Fatalf("frame = %+v, want one error on thread-unsaved", frame)
	}
	got := frame.Errors[0]
	if got.Message != "session could not be resumed automatically: boom" {
		t.Fatalf("message = %q", got.Message)
	}
	if !strings.Contains(got.SaveError, "database or disk is full") {
		t.Fatalf("saveError = %q, want the write failure", got.SaveError)
	}
	if got.ID == "" || got.OccurredAt == 0 {
		t.Fatalf("error = %+v, want an id and a time", got)
	}
	if rows := threadErrorRows(t, app.store, "thread-unsaved", ""); len(rows) != 0 {
		t.Fatalf("error rows = %q, want none: an unsaved error is not history", rows)
	}
	live := liveUnsavedErrors(t, app, "thread-unsaved")
	if live == nil || len(live.Errors) != 1 || live.Errors[0] != got {
		t.Fatalf("live state unsaved errors = %+v, want the delivered error", live)
	}
}

// The provider's own wire errors take the same path: the event worker's
// triage.Handle reaches the observer the router was built with.
func TestUnsavedWireErrorIsDeliveredLive(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedErrorApp(t, "thread-wire")
	failErrorRowWrites(t, dbPath)

	app.emitWireErrorToThread("thread-wire", "discussion sync failed: boom")

	frames := unsavedErrorFrames(rec)
	if len(frames) != 1 || frames[0].Errors[0].Message != "discussion sync failed: boom" {
		t.Fatalf("thread:unsaved_error frames = %+v, want the wire error", frames)
	}
}

func TestSavedThreadErrorDeliversNoUnsavedError(t *testing.T) {
	t.Parallel()
	app, _, rec := newUnsavedErrorApp(t, "thread-saved")

	app.emitErrorToThread("thread-saved", "reconnect failed")

	if frames := unsavedErrorFrames(rec); len(frames) != 0 {
		t.Fatalf("thread:unsaved_error frames = %+v, want none for a saved error", frames)
	}
	if rows := threadErrorRows(t, app.store, "thread-saved", "reconnect failed"); len(rows) != 1 {
		t.Fatalf("error rows = %q, want the saved row", rows)
	}
	if live := liveUnsavedErrors(t, app, "thread-saved"); live != nil {
		t.Fatalf("live state unsaved errors = %+v, want none", live)
	}
}

func TestUnsavedThreadErrorsStayBounded(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedErrorApp(t, "thread-bounded")
	failErrorRowWrites(t, dbPath)

	total := maxUnsavedThreadErrors + 3
	for i := range total {
		app.emitErrorToThread("thread-bounded", fmt.Sprintf("failure %d", i))
	}

	frames := unsavedErrorFrames(rec)
	if len(frames) != total {
		t.Fatalf("frames = %d, want one per failure (%d)", len(frames), total)
	}
	last := frames[len(frames)-1]
	if len(last.Errors) != maxUnsavedThreadErrors || last.Omitted != 3 {
		t.Fatalf("last frame errors = %d omitted = %d, want %d and 3", len(last.Errors), last.Omitted, maxUnsavedThreadErrors)
	}
	if last.Errors[0].Message != "failure 3" || last.Errors[len(last.Errors)-1].Message != fmt.Sprintf("failure %d", total-1) {
		t.Fatalf("kept errors = %+v, want the newest %d in order", last.Errors, maxUnsavedThreadErrors)
	}
	live := liveUnsavedErrors(t, app, "thread-bounded")
	if live == nil || len(live.Errors) != maxUnsavedThreadErrors || live.Omitted != 3 {
		t.Fatalf("live state = %+v, want the same bounded list", live)
	}
}

func TestDismissClearsUnsavedThreadErrors(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedErrorApp(t, "thread-dismiss")
	failErrorRowWrites(t, dbPath)
	app.emitErrorToThread("thread-dismiss", "reconnect failed")
	rec.reset()

	if err := app.DismissSessionBanner("thread-dismiss", unsavedErrorBannerKind); err != nil {
		t.Fatalf("DismissSessionBanner: %v", err)
	}

	frames := unsavedErrorFrames(rec)
	if len(frames) != 1 || frames[0].ThreadID != "thread-dismiss" || len(frames[0].Errors) != 0 || frames[0].Errors == nil {
		t.Fatalf("dismiss frames = %+v, want one empty list for every client", frames)
	}
	if live := liveUnsavedErrors(t, app, "thread-dismiss"); live != nil {
		t.Fatalf("live state after dismiss = %+v, want none", live)
	}

	rec.reset()
	if err := app.DismissSessionBanner("thread-dismiss", unsavedErrorBannerKind); err != nil {
		t.Fatalf("second DismissSessionBanner: %v", err)
	}
	if frames := unsavedErrorFrames(rec); len(frames) != 0 {
		t.Fatalf("a dismiss with nothing shown emitted %+v", frames)
	}
}

func TestDeletingAThreadForgetsItsUnsavedErrors(t *testing.T) {
	t.Parallel()
	app, dbPath, _ := newUnsavedErrorApp(t, "thread-deleted")
	failErrorRowWrites(t, dbPath)
	app.emitErrorToThread("thread-deleted", "reconnect failed")
	if live := liveUnsavedErrors(t, app, "thread-deleted"); live == nil {
		t.Fatal("setup: no unsaved error retained")
	}

	if err := app.DeleteThread("thread-deleted"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if retained := app.unsavedErrors.snapshot("thread-deleted"); retained != nil {
		t.Fatalf("retained after delete = %+v, want none", retained)
	}
}

// Before the router is wired there is no store write to try; the error
// is still kept for the client that opens the thread.
func TestErrorBeforeTriageIsWiredIsKeptLive(t *testing.T) {
	t.Parallel()
	app := &App{}

	app.emitErrorToThread("thread-early", "fell back")

	retained := app.unsavedErrors.snapshot("thread-early")
	if retained == nil || len(retained.Errors) != 1 || retained.Errors[0].Message != "fell back" ||
		!strings.Contains(retained.Errors[0].SaveError, "not wired") {
		t.Fatalf("retained = %+v, want the error with the reason it was not saved", retained)
	}
}
