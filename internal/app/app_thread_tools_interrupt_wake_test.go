package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflowhost"
)

// Whoever stops a requested turn is who the caller's wake and the stopped
// thread's timeline name: the interrupted state alone cannot tell the user's
// Stop from an agent's cancel or a workflow's takeover, so each stop records
// its cause.
func TestAStoppedRequestsWakeNamesWhoStoppedIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stop  func(f *requestFixture, threadID string) error
		want  func(f *requestFixture) string
		label func(f *requestFixture) string
	}{
		{
			name:  "the user's Stop",
			stop:  func(f *requestFixture, threadID string) error { return f.app.InterruptTurn(threadID, nil) },
			want:  func(*requestFixture) string { return "was interrupted: The user stopped its turn before it answered." },
			label: func(*requestFixture) string { return "Stopped by user" },
		},
		{
			name: "another thread's thread_cancel",
			stop: func(f *requestFixture, threadID string) error {
				_, err := f.adapter().Cancel(context.Background(), f.callerIdentity(), threadtools.CancelCall{ThreadID: threadID})
				return err
			},
			want: func(f *requestFixture) string {
				return fmt.Sprintf("was interrupted: %q (%s) stopped its turn with thread_cancel before it answered.", f.caller.Title, f.caller.ID)
			},
			label: func(f *requestFixture) string { return fmt.Sprintf("Stopped by thread %q", f.caller.Title) },
		},
		{
			name: "the user taking the thread over from its workflow",
			stop: func(f *requestFixture, threadID string) error {
				return f.app.interruptTurnForWorkflow(context.Background(), threadID, workflowhost.InterruptForTakeover)
			},
			want: func(*requestFixture) string {
				return "was interrupted: The user took the thread over from its workflow before it answered."
			},
			label: func(*requestFixture) string { return "Stopped by user" },
		},
		{
			name: "its workflow ending the attempt",
			stop: func(f *requestFixture, threadID string) error {
				return f.app.interruptTurnForWorkflow(context.Background(), threadID, workflowhost.InterruptStoppedAttempt)
			},
			want: func(*requestFixture) string {
				return "was interrupted: Its workflow stopped the turn before it answered."
			},
			label: func(*requestFixture) string { return "Stopped by its workflow" },
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			gate := f.mockClaudeHeldUntilStopped(t)
			// The caller is mid-turn, so the wake queues and stays readable.
			if err := f.app.store.InsertTurn(store.Turn{
				TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
			}); err != nil {
				t.Fatalf("InsertTurn: %v", err)
			}
			ack := f.runningRequest(t, "filter the previews")
			// The receipt runs before triage opens the turn; a stop is of an
			// open turn.
			waitUntil(t, 10*time.Second, func() bool { return f.app.triage.OpenTurnIndex(ack.ThreadID) >= 0 })

			// The interrupt waits for the CLI's ack, which the mock sends only
			// once the gate lets it read past the held turn. The gate opens
			// once the stop is recorded, which is just before the interrupt
			// is sent, so the turn ends after the stop sampled it.
			stopped := make(chan error, 1)
			go func() { stopped <- testCase.stop(f, ack.ThreadID) }()
			waitUntil(t, 10*time.Second, func() bool { return f.app.holdsTurnStop(ack.ThreadID) })
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatalf("open gate: %v", err)
			}
			if err := <-stopped; err != nil {
				t.Fatalf("stop: %v", err)
			}
			f.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)

			message := f.awaitWake(t, ack.Token)
			if want := testCase.want(f); !strings.Contains(message, want) {
				t.Errorf("wake = %q, want it to contain %q", message, want)
			}
			var stopRows []string
			items, err := f.app.store.ListItems(ack.ThreadID)
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			for _, item := range items {
				if item.Kind == "error" && strings.HasPrefix(item.Summary, "Stopped by") {
					stopRows = append(stopRows, item.Summary)
				}
			}
			if want := testCase.label(f); len(stopRows) != 1 || stopRows[0] != want {
				t.Errorf("stop rows in the stopped thread = %q, want [%q]", stopRows, want)
			}

			// The stop leaves with the thread's last open request.
			waitUntil(t, 10*time.Second, func() bool { return !f.app.holdsTurnStop(ack.ThreadID) })
		})
	}
}

// mockClaudeHeldUntilStopped holds the turn open until the returned gate file
// exists, then ends it the way the CLI ends a turn that was stopped.
func (f *requestFixture) mockClaudeHeldUntilStopped(t *testing.T) string {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "stop")
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-stop","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"assistant","message":{"id":"msg-stop","role":"assistant","content":[{"type":"text","text":"working on it"}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[Request interrupted by user]"]}`,
	}})
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
	return gate
}

// awaitWake returns the wake queued on the caller's thread for token.
func (f *requestFixture) awaitWake(t *testing.T, token string) string {
	t.Helper()
	var message string
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(token) {
				message = queued.Message
				return true
			}
		}
		return false
	})
	return message
}

// A stop is kept only while the thread has an open request, matched to the
// turn it stopped, and withdrawn when its interrupt was not delivered.
func TestTurnStopRecordLifecycle(t *testing.T) {
	t.Parallel()
	app := &App{}
	stop := stopByThread("Lead", "lead-id", "Studio")

	app.recordTurnStop("thread", 3, stop)
	if got := app.turnStopCause("thread", 3); got != turnStoppedUnknown {
		t.Fatalf("a thread with no open request kept a stop: %q", got)
	}

	app.noteReceiptRunning("thread", "tok-a")
	app.noteReceiptRunning("thread", "tok-b")
	app.recordTurnStop("thread", -1, stop)
	if app.holdsTurnStop("thread") {
		t.Fatal("a stop with no open turn was kept")
	}
	app.recordTurnStop("thread", 3, stop)
	want := `"Lead" (lead-id, on Studio) stopped its turn with thread_cancel before it answered.`
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
	if got := app.turnStopCause("thread", 4); got != turnStoppedUnknown {
		t.Fatalf("another turn read the stop: %q", got)
	}

	// An undelivered interrupt of some other turn leaves this one's stop.
	app.dropTurnStop("thread", 4)
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("dropping another turn's stop removed this one: %q", got)
	}
	app.dropTurnStop("thread", 3)
	if got := app.turnStopCause("thread", 3); got != turnStoppedUnknown {
		t.Fatalf("an undelivered stop was kept: %q", got)
	}

	// Two requests answered by one turn both read it; it leaves with the last.
	app.recordTurnStop("thread", 3, stop)
	app.forgetReceiptRunning("thread", "tok-a")
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("the second request lost the stop: %q", got)
	}
	app.forgetReceiptRunning("thread", "tok-b")
	if app.holdsTurnStop("thread") {
		t.Fatal("the stop outlived the thread's last open request")
	}
}

// An interrupt that does not say who is stopping the turn is refused before
// it does anything.
func TestInterruptWithoutACauseIsRefused(t *testing.T) {
	t.Parallel()
	app := &App{}
	for _, stop := range []turnStop{{}, {cause: "c"}, {label: "l"}} {
		if _, err := app.interruptTurnAtIndex(context.Background(), "thread", anyOpenTurn, stop); err == nil || !strings.Contains(err.Error(), "no stop cause") {
			t.Errorf("interruptTurnAtIndex(%+v): err = %v, want the missing cause refused", stop, err)
		}
		if err := app.runPlainInterruptLocked("thread", stop); err == nil || !strings.Contains(err.Error(), "no stop cause") {
			t.Errorf("runPlainInterruptLocked(%+v): err = %v, want the missing cause refused", stop, err)
		}
	}
	if err := app.interruptTurnForWorkflow(context.Background(), "thread", 0); err == nil || !strings.Contains(err.Error(), "unknown workflow interrupt") {
		t.Errorf("interruptTurnForWorkflow(0): err = %v, want it refused", err)
	}
}

func (a *App) holdsTurnStop(threadID string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	_, kept := a.threadRequests.stops[threadID]
	return kept
}
