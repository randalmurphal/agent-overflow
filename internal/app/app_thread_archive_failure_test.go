package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// archiveFailureFixture is an app whose thread has a live session entry,
// with the database path for injecting store failures.
type archiveFailureFixture struct {
	app    *App
	dbPath string
	thread store.Thread
}

func newArchiveFailureFixture(t *testing.T) archiveFailureFixture {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	thread := testThread("thread-archive-failure")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Token: "tok", Liveness: newSessionLiveness(time.Now()),
	})
	return archiveFailureFixture{app: app, dbPath: dbPath, thread: thread}
}

// failSessionClose makes the session's teardown fail: it leaves a running
// Codex background call the teardown retires, and refuses that write.
func (f archiveFailureFixture) failSessionClose(t *testing.T) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := f.app.store.AppendItem(store.Item{
		ID: "bg-archive-failure", ThreadID: f.thread.ID, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "running", IsBackground: true, Summary: "Bash: dev server", ToolName: "Bash",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed running background call: %v", err)
	}
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_retire BEFORE UPDATE ON items
		WHEN OLD.id = 'bg-archive-failure' AND NEW.decision = 'lost' BEGIN SELECT RAISE(ABORT, 'injected retire failure'); END`)
}

// requireArchivedWithCleanupError checks that err is the cleanup failure of
// an archive that happened: the row is archived, every client was told to
// drop it, and the public message says it was archived and names part.
func (f archiveFailureFixture) requireArchivedWithCleanupError(t *testing.T, err error, broadcasts *threadBroadcasts, part string) {
	t.Helper()
	var cleanup *archiveCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("archive error = %v, want an *archiveCleanupError", err)
	}
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != threadtools.CodeArchiveCleanupFailed {
		t.Fatalf("public details = %q %q %v, want code %s", code, message, ok, threadtools.CodeArchiveCleanupFailed)
	}
	if !strings.HasPrefix(message, "The thread was archived, but ") || !strings.Contains(message, part) {
		t.Fatalf("message = %q, want it to say the thread was archived and mention %q", message, part)
	}
	row, getErr := f.app.store.GetThread(f.thread.ID)
	if getErr != nil {
		t.Fatalf("GetThread: %v", getErr)
	}
	if !row.Archived {
		t.Fatal("thread is not archived")
	}
	broadcasts.expectRow(triage.ThreadActionUnlisted, f.thread.ID)
}

// Each cleanup failure after the archive committed reaches the person who
// archived, as an error that says the thread was archived.
func TestArchiveThreadReturnsCleanupFailures(t *testing.T) {
	t.Parallel()
	t.Run("session close", func(t *testing.T) {
		t.Parallel()
		f := newArchiveFailureFixture(t)
		f.failSessionClose(t)
		broadcasts := captureThreadBroadcasts(t, f.app)

		err := f.app.ArchiveThread(f.thread.ID)
		f.requireArchivedWithCleanupError(t, err, broadcasts, "provider session could not be closed")
		if !strings.Contains(err.Error(), "injected retire failure") {
			t.Fatalf("error %q lost its cause", err)
		}
	})
	t.Run("turn history read", func(t *testing.T) {
		t.Parallel()
		f := newArchiveFailureFixture(t)
		if err := f.app.store.InsertTurn(store.Turn{TurnID: "turn-unreadable", ThreadID: f.thread.ID, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		execOnStore(t, f.dbPath, `UPDATE turns SET started_at = 'unreadable' WHERE turn_id = 'turn-unreadable'`)
		broadcasts := captureThreadBroadcasts(t, f.app)

		err := f.app.ArchiveThread(f.thread.ID)
		f.requireArchivedWithCleanupError(t, err, broadcasts, "provider session could not be closed")
		if _, live := f.app.sessionManager().get(f.thread.ID); !live {
			t.Fatal("an unreadable turn history closed the session anyway")
		}
	})
	t.Run("browser pages", func(t *testing.T) {
		t.Parallel()
		f := newArchiveFailureFixture(t)
		f.app.closeBrowserPagesFn = func(string) error { return errors.New("injected page close failure") }
		broadcasts := captureThreadBroadcasts(t, f.app)

		err := f.app.ArchiveThread(f.thread.ID)
		f.requireArchivedWithCleanupError(t, err, broadcasts, "browser pages could not all be closed")
		if strings.Contains(err.Error(), "provider session") {
			t.Fatalf("message %q names a session that closed", err)
		}
	})
}

// A session kept because a turn started after the archive was requested is
// the archive working as meant, not a failure.
func TestArchiveThreadKeepingAReengagedSessionReturnsNil(t *testing.T) {
	t.Parallel()
	f := newArchiveFailureFixture(t)
	f.app.closeBrowserPagesFn = func(string) error { return errors.New("pages must stay open") }
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "turn-after", ThreadID: f.thread.ID, StartedAt: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.ArchiveThread(f.thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if _, live := f.app.sessionManager().get(f.thread.ID); !live {
		t.Fatal("the re-engaged session was closed")
	}
}

// An agent's thread_update that archives reports the cleanup failure in the
// thread's row: updated, with the same message, so the agent can tell the
// user. The archive is not reported as refused.
func TestThreadUpdateArchiveReportsCleanupFailure(t *testing.T) {
	t.Parallel()
	f := newArchiveFailureFixture(t)
	f.failSessionClose(t)
	broadcasts := captureThreadBroadcasts(t, f.app)

	report, err := f.app.threadToolsAdapter().UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{f.thread.ID}, Archived: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	row := updateResultFor(t, report, f.thread.ID)
	if !row.Updated || row.ErrorCode != threadtools.CodeArchiveCleanupFailed || !strings.HasPrefix(row.Error, "The thread was archived, but its provider session") {
		t.Fatalf("result = %+v, want an updated row carrying the cleanup failure", row)
	}
	if strings.Contains(row.Error, "injected") {
		t.Fatalf("result %q leaks the private cause", row.Error)
	}

	// The internal door returns the same error with the applied result.
	f2 := newArchiveFailureFixture(t)
	f2.failSessionClose(t)
	broadcasts2 := captureThreadBroadcasts(t, f2.app)
	archived := true
	applied, err := f2.app.applyThreadOrganizePatch(t.Context(), f2.thread.ID, threadapp.OrganizePatch{Archived: &archived})
	f2.requireArchivedWithCleanupError(t, err, broadcasts2, "provider session could not be closed")
	if !applied.Thread.Archived {
		t.Fatalf("applied result = %+v, want the archived row", applied.Thread)
	}
	broadcasts.expectRow(triage.ThreadActionUnlisted, f.thread.ID)
}

// The message names each part that failed, in one sentence.
func TestArchiveCleanupErrorMessage(t *testing.T) {
	t.Parallel()
	failed := errors.New("failed")
	for _, tc := range []struct {
		cleanup archiveCleanupError
		want    string
	}{
		{archiveCleanupError{}, ""},
		{archiveCleanupError{browser: failed}, "The thread was archived, but its browser pages could not all be closed."},
		{archiveCleanupError{session: failed, browser: failed}, "The thread was archived, but its provider session could not be closed, so it and the processes it started may still be running, and its browser pages could not all be closed."},
		{archiveCleanupError{session: failed, browser: failed, remote: failed}, "The thread was archived, but its provider session could not be closed, so it and the processes it started may still be running, its browser pages could not all be closed, and its commands on other computers may not all have been cancelled."},
	} {
		err := tc.cleanup.err()
		if tc.want == "" {
			if err != nil {
				t.Errorf("no failure returned %v", err)
			}
			continue
		}
		if _, message, _ := errorsx.PublicDetails(err); message != tc.want {
			t.Errorf("message = %q\nwant      %q", message, tc.want)
		}
	}
}
