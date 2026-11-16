package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// cleanupFailureFixture is an app whose thread has a live session entry,
// with the database path for injecting store failures.
type cleanupFailureFixture struct {
	app    *App
	dbPath string
	thread store.Thread
}

func newCleanupFailureFixture(t *testing.T) cleanupFailureFixture {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	thread := testThread(uuid.NewString())
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Token: "tok", Liveness: newSessionLiveness(time.Now()),
	})
	return cleanupFailureFixture{app: app, dbPath: dbPath, thread: thread}
}

// failBackgroundRetire lets the provider close and makes the teardown's
// next step fail: it leaves a running Codex background call the teardown
// marks ended, and refuses that write.
func (f cleanupFailureFixture) failBackgroundRetire(t *testing.T) {
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

// unreachableComputer gives the app a set of paired computers that does not
// include the one it returns, so every call to it fails.
func (f cleanupFailureFixture) unreachableComputer(t *testing.T) string {
	t.Helper()
	manager, err := attachedbackends.New(t.TempDir(), "cleanup source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	f.app.backends = manager
	return uuid.NewString()
}

// startRemoteCommand records a running command on computerID that the
// thread started.
func (f cleanupFailureFixture) startRemoteCommand(t *testing.T, computerID string) string {
	t.Helper()
	requestID := uuid.NewString()
	if _, err := f.app.store.RegisterRemoteWatch(store.RemoteWatch{
		ComputerID: computerID, RequestID: requestID, ThreadID: f.thread.ID,
		Fingerprint: strings.Repeat("ab", 32), Label: "dev server", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("RegisterRemoteWatch: %v", err)
	}
	return requestID
}

// startAsk records an open ask the thread owns on computerID.
func (f cleanupFailureFixture) startAsk(t *testing.T, computerID string) {
	t.Helper()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: newThreadRequestToken(), CallerThreadID: f.thread.ID, Kind: store.ThreadRequestAsk,
		State: store.ThreadRequestAccepted, TargetComputerID: computerID, TargetThreadID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
}

// requireCleanupError checks that err is the cleanup failure of an archive
// or delete that happened, whose public message names exactly the parts
// given, and returns that message.
func requireCleanupError(t *testing.T, err error, outcome string, parts ...string) string {
	t.Helper()
	var cleanup *threadCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("error = %v, want a *threadCleanupError", err)
	}
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != threadtools.CodeCleanupFailed {
		t.Fatalf("public details = %q %q %v, want code %s", code, message, ok, threadtools.CodeCleanupFailed)
	}
	if !strings.HasPrefix(message, "The thread was "+outcome+", but ") {
		t.Fatalf("message = %q, want it to say the thread was %s", message, outcome)
	}
	for _, part := range []string{"provider session could not be closed", "background tasks", "browser pages", "an ask", "for up to an hour", "may not all have been cancelled"} {
		want := false
		for _, named := range parts {
			want = want || named == part
		}
		if strings.Contains(message, part) != want {
			t.Fatalf("message = %q: mentions %q = %v, want %v", message, part, !want, want)
		}
	}
	return message
}

// requireArchived checks that the row is archived and every client was told
// to drop it.
func (f cleanupFailureFixture) requireArchived(t *testing.T, broadcasts *threadBroadcasts) {
	t.Helper()
	row, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !row.Archived {
		t.Fatal("thread is not archived")
	}
	broadcasts.expectRow(triage.ThreadActionUnlisted, f.thread.ID)
}

// Each cleanup failure after the archive committed reaches the person who
// archived, as an error that says the thread was archived and names only
// what failed.
func TestArchiveThreadReturnsCleanupFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		arrange func(*testing.T, cleanupFailureFixture)
		part    string
		check   func(*testing.T, cleanupFailureFixture, error)
	}{
		{"provider close", func(t *testing.T, f cleanupFailureFixture) {
			f.app.closeProviderSessionFn = func(string) error { return errors.New("injected provider close failure") }
		}, "provider session could not be closed", nil},
		{"background calls not marked ended", func(t *testing.T, f cleanupFailureFixture) {
			f.failBackgroundRetire(t)
		}, "background tasks", func(t *testing.T, _ cleanupFailureFixture, err error) {
			if !strings.Contains(err.Error(), "injected retire failure") {
				t.Fatalf("error %q lost its cause", err)
			}
		}},
		{"turn history read", func(t *testing.T, f cleanupFailureFixture) {
			if err := f.app.store.InsertTurn(store.Turn{TurnID: "turn-unreadable", ThreadID: f.thread.ID, StartedAt: 1}); err != nil {
				t.Fatal(err)
			}
			execOnStore(t, f.dbPath, `UPDATE turns SET started_at = 'unreadable' WHERE turn_id = 'turn-unreadable'`)
		}, "provider session could not be closed", func(t *testing.T, f cleanupFailureFixture, _ error) {
			if _, live := f.app.sessionManager().get(f.thread.ID); !live {
				t.Fatal("an unreadable turn history closed the session anyway")
			}
		}},
		{"browser pages", func(t *testing.T, f cleanupFailureFixture) {
			f.app.closeBrowserPagesFn = func(string) error { return errors.New("injected page close failure") }
		}, "browser pages", nil},
		{"remote command cancel not delivered", func(t *testing.T, f cleanupFailureFixture) {
			f.startRemoteCommand(t, f.unreachableComputer(t))
		}, "for up to an hour", func(t *testing.T, f cleanupFailureFixture, _ error) {
			rows, err := f.app.store.ListRemoteWatches(f.thread.ID, 0, 10)
			if err != nil || len(rows) != 1 || rows[0].Notification != "dismissed" {
				t.Fatalf("watches = %+v, %v; want the undelivered command dismissed anyway", rows, err)
			}
		}},
		{"ask not stopped", func(t *testing.T, f cleanupFailureFixture) {
			f.startAsk(t, f.unreachableComputer(t))
		}, "an ask", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCleanupFailureFixture(t)
			tc.arrange(t, f)
			broadcasts := captureThreadBroadcasts(t, f.app)

			err := f.app.ArchiveThread(f.thread.ID)
			requireCleanupError(t, err, "archived", tc.part)
			f.requireArchived(t, broadcasts)
			if tc.check != nil {
				tc.check(t, f, err)
			}
		})
	}
}

// A session kept because a turn started after the archive was requested is
// the archive working as meant, not a failure.
func TestArchiveThreadKeepingAReengagedSessionReturnsNil(t *testing.T) {
	t.Parallel()
	f := newCleanupFailureFixture(t)
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
	f := newCleanupFailureFixture(t)
	f.failBackgroundRetire(t)

	report, err := f.app.threadToolsAdapter().UpdateThreads(t.Context(), caller(uuid.NewString()), threadtools.UpdateCall{
		ThreadIDs: []string{f.thread.ID}, Archived: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	row := updateResultFor(t, report, f.thread.ID)
	if !row.Updated || row.ErrorCode != threadtools.CodeCleanupFailed || !strings.HasPrefix(row.Error, "The thread was archived, but its background tasks") {
		t.Fatalf("result = %+v, want an updated row carrying the cleanup failure", row)
	}
	if strings.Contains(row.Error, "injected") {
		t.Fatalf("result %q leaks the private cause", row.Error)
	}

	// The internal door returns the same error with the applied result.
	g := newCleanupFailureFixture(t)
	g.startAsk(t, g.unreachableComputer(t))
	broadcasts := captureThreadBroadcasts(t, g.app)
	archived := true
	applied, err := g.app.applyThreadOrganizePatch(t.Context(), g.thread.ID, threadapp.OrganizePatch{Archived: &archived})
	requireCleanupError(t, err, "archived", "an ask")
	g.requireArchived(t, broadcasts)
	if !applied.Thread.Archived {
		t.Fatalf("applied result = %+v, want the archived row", applied.Thread)
	}
}

// A delete that finished but could not stop the thread's work elsewhere
// says the thread was deleted and what may keep running.
func TestDeleteThreadReportsWorkItCouldNotStop(t *testing.T) {
	t.Parallel()
	f := newCleanupFailureFixture(t)
	computer := f.unreachableComputer(t)
	f.startRemoteCommand(t, computer)
	f.startAsk(t, computer)

	err := f.app.DeleteThread(f.thread.ID)
	requireCleanupError(t, err, "deleted", "an ask", "for up to an hour")
	if _, getErr := f.app.store.GetThread(f.thread.ID); getErr == nil {
		t.Fatal("the thread was not deleted")
	}
}

// The message names each part that failed, in one sentence.
func TestThreadCleanupErrorMessage(t *testing.T) {
	t.Parallel()
	failed := errors.New("failed")
	for _, tc := range []struct {
		cleanup threadCleanupError
		want    string
	}{
		{threadCleanupError{outcome: "archived"}, ""},
		{threadCleanupError{outcome: "archived", browser: failed}, "The thread was archived, but its browser pages could not all be closed."},
		{threadCleanupError{outcome: "archived", session: failed, browser: failed}, "The thread was archived, but its provider session could not be closed, so it and the processes it started may still be running, and its browser pages could not all be closed."},
		{threadCleanupError{outcome: "deleted", asks: failed, commands: failed, commandRecords: failed}, "The thread was deleted, but an ask it sent to another thread could not be stopped and may keep running, a command it started on another computer could not be cancelled and may keep running there for up to an hour, and its commands on other computers may not all have been cancelled."},
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

// A delete stops each thread's remote work twice, and an ask the first pass
// could not stop is tried again: the second pass's answer stands. A command
// whose cancel was not delivered is dismissed by the first pass and never
// tried again, so it stays reported.
func TestDeleteReportsAnAskByItsLastStopAttempt(t *testing.T) {
	t.Parallel()
	f := newCleanupFailureFixture(t)
	computer := f.unreachableComputer(t)
	f.startRemoteCommand(t, computer)
	f.startAsk(t, computer)

	unstopped := threadCleanupError{outcome: "deleted"}
	ports := f.app.threadDeletePorts(&unstopped)
	if err := ports.StopRemoteWork(f.thread.ID); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	requireCleanupError(t, unstopped.err(), "deleted", "an ask", "for up to an hour")

	// The ask ends before the second pass, as it does when that pass's
	// cancel reaches the computer.
	execOnStore(t, f.dbPath, `UPDATE thread_requests SET state = 'cancelled'`)
	if err := ports.StopRemoteWork(f.thread.ID); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	requireCleanupError(t, unstopped.err(), "deleted", "for up to an hour")

	// Another thread's ask that stays unstopped is still reported.
	other := testThread(uuid.NewString())
	if err := f.app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: newThreadRequestToken(), CallerThreadID: other.ID, Kind: store.ThreadRequestAsk,
		State: store.ThreadRequestAccepted, TargetComputerID: computer, TargetThreadID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if err := ports.StopRemoteWork(other.ID); err != nil {
		t.Fatalf("other thread: %v", err)
	}
	if err := ports.StopRemoteWork(f.thread.ID); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	requireCleanupError(t, unstopped.err(), "deleted", "an ask", "for up to an hour")
}

// A cancel that was not delivered and a watch that could not be dismissed
// are returned apart, so a caller that stops on the store failure still
// has the undelivered cancel to report or log.
func TestCancelThreadRemoteCommandsReturnsBothFailures(t *testing.T) {
	t.Parallel()
	f := newCleanupFailureFixture(t)
	f.startRemoteCommand(t, f.unreachableComputer(t))
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_dismiss BEFORE UPDATE ON remote_watches
		WHEN NEW.notification = 'dismissed' BEGIN SELECT RAISE(ABORT, 'injected dismiss failure'); END`)

	undelivered, err := f.app.cancelThreadRemoteCommands(t.Context(), f.thread.ID)
	if undelivered == nil || !strings.Contains(undelivered.Error(), "cancel not delivered") {
		t.Fatalf("undelivered = %v, want the cancel that did not reach the computer", undelivered)
	}
	if err == nil || !strings.Contains(err.Error(), "injected dismiss failure") {
		t.Fatalf("err = %v, want the dismiss failure", err)
	}
}
