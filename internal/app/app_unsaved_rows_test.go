package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

// A thread error or notice AO cannot save still reaches the user: live on
// thread:unsaved_rows and in GetThreadLiveState, never in history.

func newUnsavedRowApp(t *testing.T, threadID string) (*App, string, *emitRecorder) {
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

func unsavedRowFrames(rec *emitRecorder) []UnsavedThreadRowsEvent {
	var frames []UnsavedThreadRowsEvent
	for _, call := range rec.snapshot() {
		if call.Channel != eventchan.ThreadUnsavedRows.String() {
			continue
		}
		if evt, ok := call.Data.(UnsavedThreadRowsEvent); ok {
			frames = append(frames, evt)
		}
	}
	return frames
}

func liveUnsavedRows(t *testing.T, app *App, threadID string) *UnsavedThreadRowsEvent {
	t.Helper()
	state, err := app.GetThreadLiveState(threadID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	return state.UnsavedRows
}

func TestUnsavedThreadErrorIsDeliveredLive(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedRowApp(t, "thread-unsaved")
	failErrorRowWrites(t, dbPath)

	app.emitErrorToThread("thread-unsaved", "session could not be resumed automatically: boom")

	frames := unsavedRowFrames(rec)
	if len(frames) != 1 {
		t.Fatalf("thread:unsaved_rows frames = %+v, want one", frames)
	}
	frame := frames[0]
	if frame.ThreadID != "thread-unsaved" || len(frame.Rows) != 1 {
		t.Fatalf("frame = %+v, want one error on thread-unsaved", frame)
	}
	got := frame.Rows[0]
	if got.Message != "session could not be resumed automatically: boom" {
		t.Fatalf("message = %q", got.Message)
	}
	if !strings.Contains(got.SaveError, "database or disk is full") {
		t.Fatalf("saveError = %q, want the write failure", got.SaveError)
	}
	if got.Severity != "error" {
		t.Fatalf("severity = %q, want error", got.Severity)
	}
	if got.ID == "" || got.OccurredAt == 0 {
		t.Fatalf("error = %+v, want an id and a time", got)
	}
	if rows := threadErrorRows(t, app.store, "thread-unsaved", ""); len(rows) != 0 {
		t.Fatalf("error rows = %q, want none: an unsaved error is not history", rows)
	}
	live := liveUnsavedRows(t, app, "thread-unsaved")
	if live == nil || len(live.Rows) != 1 || live.Rows[0] != got {
		t.Fatalf("live state unsaved errors = %+v, want the delivered error", live)
	}
}

// The provider's own wire errors take the same path: the event worker's
// triage.Handle reaches the observer the router was built with.
func TestUnsavedWireErrorIsDeliveredLive(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedRowApp(t, "thread-wire")
	failErrorRowWrites(t, dbPath)

	app.emitWireErrorToThread("thread-wire", "discussion sync failed: boom")

	frames := unsavedRowFrames(rec)
	if len(frames) != 1 || frames[0].Rows[0].Message != "discussion sync failed: boom" {
		t.Fatalf("thread:unsaved_rows frames = %+v, want the wire error", frames)
	}
}

func TestSavedThreadErrorDeliversNoUnsavedRow(t *testing.T) {
	t.Parallel()
	app, _, rec := newUnsavedRowApp(t, "thread-saved")

	app.emitErrorToThread("thread-saved", "reconnect failed")

	if frames := unsavedRowFrames(rec); len(frames) != 0 {
		t.Fatalf("thread:unsaved_rows frames = %+v, want none for a saved error", frames)
	}
	if rows := threadErrorRows(t, app.store, "thread-saved", "reconnect failed"); len(rows) != 1 {
		t.Fatalf("error rows = %q, want the saved row", rows)
	}
	if live := liveUnsavedRows(t, app, "thread-saved"); live != nil {
		t.Fatalf("live state unsaved errors = %+v, want none", live)
	}
}

func TestUnsavedThreadRowsStayBounded(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedRowApp(t, "thread-bounded")
	failErrorRowWrites(t, dbPath)

	total := maxUnsavedThreadRows + 3
	for i := range total {
		app.emitErrorToThread("thread-bounded", fmt.Sprintf("failure %d", i))
	}

	frames := unsavedRowFrames(rec)
	if len(frames) != total {
		t.Fatalf("frames = %d, want one per failure (%d)", len(frames), total)
	}
	last := frames[len(frames)-1]
	if len(last.Rows) != maxUnsavedThreadRows || last.Omitted != 3 {
		t.Fatalf("last frame errors = %d omitted = %d, want %d and 3", len(last.Rows), last.Omitted, maxUnsavedThreadRows)
	}
	if last.Rows[0].Message != "failure 3" || last.Rows[len(last.Rows)-1].Message != fmt.Sprintf("failure %d", total-1) {
		t.Fatalf("kept errors = %+v, want the newest %d in order", last.Rows, maxUnsavedThreadRows)
	}
	live := liveUnsavedRows(t, app, "thread-bounded")
	if live == nil || len(live.Rows) != maxUnsavedThreadRows || live.Omitted != 3 {
		t.Fatalf("live state = %+v, want the same bounded list", live)
	}
}

func TestDismissClearsUnsavedThreadRows(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedRowApp(t, "thread-dismiss")
	failErrorRowWrites(t, dbPath)
	app.emitErrorToThread("thread-dismiss", "reconnect failed")
	rec.reset()

	if err := app.DismissSessionBanner("thread-dismiss", unsavedRowsBannerKind); err != nil {
		t.Fatalf("DismissSessionBanner: %v", err)
	}

	frames := unsavedRowFrames(rec)
	if len(frames) != 1 || frames[0].ThreadID != "thread-dismiss" || len(frames[0].Rows) != 0 || frames[0].Rows == nil {
		t.Fatalf("dismiss frames = %+v, want one empty list for every client", frames)
	}
	if live := liveUnsavedRows(t, app, "thread-dismiss"); live != nil {
		t.Fatalf("live state after dismiss = %+v, want none", live)
	}

	rec.reset()
	if err := app.DismissSessionBanner("thread-dismiss", unsavedRowsBannerKind); err != nil {
		t.Fatalf("second DismissSessionBanner: %v", err)
	}
	if frames := unsavedRowFrames(rec); len(frames) != 0 {
		t.Fatalf("a dismiss with nothing shown emitted %+v", frames)
	}
}

func TestDeletingAThreadForgetsItsUnsavedRows(t *testing.T) {
	t.Parallel()
	app, dbPath, _ := newUnsavedRowApp(t, "thread-deleted")
	failErrorRowWrites(t, dbPath)
	app.emitErrorToThread("thread-deleted", "reconnect failed")
	if live := liveUnsavedRows(t, app, "thread-deleted"); live == nil {
		t.Fatal("setup: no unsaved error retained")
	}

	if err := app.DeleteThread("thread-deleted"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if retained := app.unsavedRows.snapshot("thread-deleted"); retained != nil {
		t.Fatalf("retained after delete = %+v, want none", retained)
	}
}

func failNoticeRowWrites(t *testing.T, dbPath string) {
	t.Helper()
	execOnFile(t, dbPath, `CREATE TRIGGER fail_notice_row BEFORE INSERT ON items WHEN NEW.kind = 'notification' BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`)
}

// The app's notices reach the same list as warnings, from every caller,
// because the refusal is handled where the row is written.
func TestUnsavedAppNoticesAreDeliveredLiveAsWarnings(t *testing.T) {
	t.Parallel()
	app, dbPath, rec := newUnsavedRowApp(t, "thread-notice")
	failNoticeRowWrites(t, dbPath)

	if err := app.emitNoticeToThread("thread-notice", noticeKindWorktreeCleanup, "Removed worktree feature."); err == nil {
		t.Fatal("emitNoticeToThread returned nil for a refused notice")
	}
	app.noticeClaudeTUIDiscardedMessage("thread-notice")
	app.postMergedPRKept([]string{"thread-notice"}, "PR !7", "feature", "2 uncommitted files")

	frames := unsavedRowFrames(rec)
	if len(frames) != 3 {
		t.Fatalf("thread:unsaved_rows frames = %d, want one per notice", len(frames))
	}
	rows := frames[len(frames)-1].Rows
	want := []string{"Removed worktree feature.", claudeTUIDiscardedMessageNotice, "PR !7 merged. Kept worktree feature: 2 uncommitted files."}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d notices", rows, len(want))
	}
	for i, row := range rows {
		if row.Message != want[i] || row.Severity != "warning" || !strings.Contains(row.SaveError, "database or disk is full") {
			t.Fatalf("row %d = %+v, want %q as a warning with the write failure", i, row, want[i])
		}
	}
}

// One list holds both: an error and a notice keep their own severities.
func TestUnsavedErrorsAndNoticesShareOneList(t *testing.T) {
	t.Parallel()
	app, dbPath, _ := newUnsavedRowApp(t, "thread-mixed")
	failErrorRowWrites(t, dbPath)
	failNoticeRowWrites(t, dbPath)

	app.emitErrorToThread("thread-mixed", "reconnect failed")
	_ = app.emitNoticeToThread("thread-mixed", noticeKindWarning, "Auto is back on.")

	live := liveUnsavedRows(t, app, "thread-mixed")
	if live == nil || len(live.Rows) != 2 || live.Rows[0].Severity != "error" || live.Rows[1].Severity != "warning" {
		t.Fatalf("live = %+v, want the error then the notice", live)
	}
}

// A refused retry of the same row (a replayed session death) replaces its
// entry instead of filling the list.
func TestARetriedUnsavedRowReplacesItsEntry(t *testing.T) {
	t.Parallel()
	app := &App{}
	row := triage.UnsavedRow{ThreadID: "thread-retry", ItemID: "session_died:3", Severity: triage.UnsavedSeverityWarning, Summary: "Provider session exited with code 1"}

	app.deliverUnsavedThreadRow(triage.UnsavedRow{ThreadID: "thread-retry", ItemID: "error:3:0", Severity: triage.UnsavedSeverityError, Summary: "boom"}, errors.New("full"))
	app.deliverUnsavedThreadRow(row, errors.New("full"))
	app.deliverUnsavedThreadRow(row, errors.New("still full"))

	retained := app.unsavedRows.snapshot("thread-retry")
	if retained == nil || len(retained.Rows) != 2 || retained.Rows[1].ID != "session_died:3" || retained.Rows[1].SaveError != "still full" {
		t.Fatalf("retained = %+v, want the error and one session_died entry with the latest failure", retained)
	}
}

// Before the router is wired there is no store write to try; the error
// is still kept for the client that opens the thread.
func TestErrorBeforeTriageIsWiredIsKeptLive(t *testing.T) {
	t.Parallel()
	app := &App{}

	app.emitErrorToThread("thread-early", "fell back")
	if err := app.emitNoticeToThread("thread-early", noticeKindWarning, "noted"); err == nil {
		t.Fatal("emitNoticeToThread returned nil without a router")
	}

	retained := app.unsavedRows.snapshot("thread-early")
	if retained == nil || len(retained.Rows) != 2 ||
		retained.Rows[0].Message != "fell back" || retained.Rows[0].Severity != "error" ||
		retained.Rows[1].Message != "noted" || retained.Rows[1].Severity != "warning" ||
		!strings.Contains(retained.Rows[0].SaveError, "not wired") {
		t.Fatalf("retained = %+v, want the error and the notice with the reason they were not saved", retained)
	}
}
