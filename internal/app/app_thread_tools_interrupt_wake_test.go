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
		name string
		// before sets up another thread's cancel and returns that thread,
		// and the request it left open on the stopped thread, if any.
		before func(t *testing.T, f *requestFixture, threadID string) (threadtools.Caller, string)
		stop   func(f *requestFixture, threadID string, by threadtools.Caller) error
		want   func(by threadtools.Caller) string
		label  func(by threadtools.Caller) string
	}{
		{
			name: "the user's Stop",
			stop: func(f *requestFixture, threadID string, _ threadtools.Caller) error {
				return f.app.InterruptTurn(threadID, nil)
			},
			want:  func(threadtools.Caller) string { return "]: " + threadtools.WakePersonStopped },
			label: func(threadtools.Caller) string { return "Stopped by user" },
		},
		{
			// A thread that sent to it, not the one that spawned it: a
			// spawner's thread_cancel brings the thread to rest instead.
			name: "another thread's thread_cancel",
			// Its own message goes to the session during the turn it then
			// stops, and no turn reads it.
			before: func(t *testing.T, f *requestFixture, threadID string) (threadtools.Caller, string) {
				other := f.targetIdentity(t, f.plainThread(t).ID)
				sent, err := f.adapter().Send(t.Context(), other, threadtools.SendCall{ThreadID: threadID, Message: "and this"})
				if err != nil {
					t.Fatalf("the other thread's send: %v", err)
				}
				// The send returns once its message is handed to the
				// dispatch. One that reaches the session only after the
				// stopped turn ended opens a turn of its own, which no stop
				// is kept for.
				f.awaitReceiptRunning(t, sent.Token)
				return other, sent.Token
			},
			stop: func(f *requestFixture, threadID string, by threadtools.Caller) error {
				_, err := f.adapter().Cancel(context.Background(), by, threadtools.CancelCall{ThreadID: threadID})
				return err
			},
			want: func(by threadtools.Caller) string {
				return fmt.Sprintf("]: %q (%s) stopped its turn with thread_cancel before it answered.", by.Title, by.ThreadID)
			},
			label: func(by threadtools.Caller) string { return fmt.Sprintf("Stopped by thread %q", by.Title) },
		},
		{
			name: "the user taking the thread over from its workflow",
			stop: func(f *requestFixture, threadID string, _ threadtools.Caller) error {
				return f.app.interruptTurnForWorkflow(context.Background(), threadID, workflowhost.InterruptForTakeover)
			},
			want: func(threadtools.Caller) string {
				return "]: The user took the thread over from its workflow before it answered."
			},
			label: func(threadtools.Caller) string { return "Stopped by user" },
		},
		{
			name: "its workflow ending the attempt",
			stop: func(f *requestFixture, threadID string, _ threadtools.Caller) error {
				return f.app.interruptTurnForWorkflow(context.Background(), threadID, workflowhost.InterruptStoppedAttempt)
			},
			want: func(threadtools.Caller) string {
				return "]: Its workflow stopped the turn before it answered."
			},
			label: func(threadtools.Caller) string { return "Stopped by its workflow" },
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			gate := f.mockClaudeHeldUntilStopped(t)
			f.holdCallerTurn(t)
			ack := f.runningRequest(t, "filter the previews")
			// The receipt runs before triage opens the turn; a stop is of an
			// open turn.
			waitUntil(t, 10*time.Second, func() bool { return f.app.triage.OpenTurnIndex(ack.ThreadID) >= 0 })

			// The interrupt waits for the CLI's ack, which the mock sends only
			// once the gate lets it read past the held turn. The gate opens
			// once the stop is recorded, which is just before the interrupt
			// is sent, so the turn ends after the stop sampled it.
			var by threadtools.Caller
			var open string
			if testCase.before != nil {
				by, open = testCase.before(t, f, ack.ThreadID)
			}
			stopped := make(chan error, 1)
			go func() { stopped <- testCase.stop(f, ack.ThreadID, by) }()
			waitUntil(t, 10*time.Second, func() bool { return f.app.holdsTurnStop(ack.ThreadID) })
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatalf("open gate: %v", err)
			}
			if err := <-stopped; err != nil {
				t.Fatalf("stop: %v", err)
			}
			f.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)

			message := f.awaitWake(t, ack.Token)
			if want := testCase.want(by); !strings.Contains(message, want) {
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
			if want := testCase.label(by); len(stopRows) != 1 || stopRows[0] != want {
				t.Errorf("stop rows in the stopped thread = %q, want [%q]", stopRows, want)
			}

			// The stop leaves with the thread's last open request. A request
			// whose message the session holds unread is still open, so the
			// stop stays until it settles.
			if open != "" {
				f.awaitRequestState(t, open, store.ThreadRequestRunning)
				if !f.app.holdsTurnStop(ack.ThreadID) {
					t.Fatal("the stop left while a request was still open on the thread")
				}
				if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{Token: open, Text: "seen"}); err != nil {
					t.Fatalf("reply to the open request: %v", err)
				}
			}
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

	app.recordTurnStop("thread", stoppedTurn{index: 3}, stop)
	if got := app.turnStopCause("thread", 3); got != turnStoppedUnknown {
		t.Fatalf("a thread with no open request kept a stop: %+v", got)
	}

	app.noteReceiptRunning("thread", "tok-a", testSessionToken(app, "thread"))
	app.noteReceiptRunning("thread", "tok-b", testSessionToken(app, "thread"))
	app.recordTurnStop("thread", stoppedTurn{index: -1}, stop)
	if app.holdsTurnStop("thread") {
		t.Fatal("a stop with no open turn was kept")
	}
	app.recordTurnStop("thread", stoppedTurn{index: 3}, stop)
	want := threadtools.Cause{Code: threadtools.CauseThreadCancel, Arg: `"Lead" (lead-id, on Studio)`}
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("cause = %+v, want %+v", got, want)
	}
	if words := want.Answer(""); words != `"Lead" (lead-id, on Studio) stopped its turn with thread_cancel before it answered.` {
		t.Fatalf("the cause reads %q", words)
	}
	if got := app.turnStopCause("thread", 4); got != turnStoppedUnknown {
		t.Fatalf("another turn read the stop: %+v", got)
	}

	// An undelivered interrupt of some other turn leaves this one's stop.
	app.dropTurnStop("thread", 4)
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("dropping another turn's stop removed this one: %+v", got)
	}
	app.dropTurnStop("thread", 3)
	if got := app.turnStopCause("thread", 3); got != turnStoppedUnknown {
		t.Fatalf("an undelivered stop was kept: %+v", got)
	}

	// Two requests answered by one turn both read it; it leaves with the last.
	app.recordTurnStop("thread", stoppedTurn{index: 3}, stop)
	app.forgetReceiptRunning("thread", "tok-a")
	if got := app.turnStopCause("thread", 3); got != want {
		t.Fatalf("the second request lost the stop: %+v", got)
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
	for _, stop := range []turnStop{{}, {cause: threadtools.Cause{Code: threadtools.CauseWorkflow}}, {label: "l"}, {label: "l", cause: threadtools.Cause{Code: "evicted"}}} {
		if _, err := app.interruptTurnAtIndex(context.Background(), "thread", anyOpenTurn, stop); err == nil || !strings.Contains(err.Error(), "no stop cause") {
			t.Errorf("interruptTurnAtIndex(%+v): err = %v, want the missing cause refused", stop, err)
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
