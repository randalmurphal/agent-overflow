package app

// A held request reads a turn it already read again: a turn's first close is
// not its outcome. And a restart keeps the turn a held request answers from.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// softCloseLine is Claude's soft close of a turn: the message's stop reason,
// before the result envelope that ends the process's turn.
const softCloseLine = `{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}`

// The request's own turn closes softly with a loop armed, so the request is
// held; the result that follows is an error, which triage folds into that
// same turn (TestLateErrorResultOnSettledTurnFoldsWithoutErrorItem,
// TestSoftThenRealTurnComplete_LateResultErrorUpdatesSettledTurn). The
// request settles errored with it, not finished once the loop is cancelled.
func TestAHeldSpawnSettlesErroredWhenItsTurnTakesALateError(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ result, error string }{
		"error during execution": {`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["request failed after retries"]}`,
			"request failed after retries"},
		"an API error result": {`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 529 Overloaded.",` +
			`"usage":{"input_tokens":6,"output_tokens":34}}`, "529 Overloaded"},
	} {
		result := c.result
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerSession()
			closeGate, resultGate := filepath.Join(t.TempDir(), "close"), filepath.Join(t.TempDir(), "result")
			installMockClaudeTurns(t, f.app, [][]string{{
				mockClaudeInitLine,
				`{"type":"assistant","message":{"id":"msg-watch","role":"assistant","content":[{"type":"text","text":"Watching the board."}]}}`,
				testutil.MockWaitForFile(closeGate),
				softCloseLine,
				testutil.MockWaitForFile(resultGate),
				result,
			}})
			ack := f.spawnWorker(t, "watch the board")
			f.awaitTurnRunning(t, ack.Token)
			f.armLoop(t, ack.ThreadID, "board")
			release(t, closeGate)
			f.awaitRestHold(t, ack)

			release(t, resultGate)
			waitUntil(t, 20*time.Second, func() bool {
				turn, found, err := f.app.store.GetTurnByThreadIndex(ack.ThreadID, 0)
				return err == nil && found && turn.StopReason == "error"
			})
			row := f.awaitRequestState(t, ack.Token, store.ThreadRequestErrored)
			if row.AnswerKind != store.ThreadAnswerError || !strings.Contains(string(row.Answer), c.error) {
				t.Fatalf("settled %+v; want errored with %q", row, c.error)
			}
		})
	}
}

// The request's own turn closes softly with a loop armed, so the request is
// held; the model continues that turn, which reopens it, and then it fails.
// The request settles errored, not finished with what the turn wrote.
func TestAHeldSpawnSettlesErroredWhenItsResumedTurnFails(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	closeGate, resumeGate, resultGate := filepath.Join(t.TempDir(), "close"), filepath.Join(t.TempDir(), "resume"), filepath.Join(t.TempDir(), "result")
	installMockClaudeTurns(t, f.app, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-watch","role":"assistant","content":[{"type":"text","text":"Watching the board."}]}}`,
		testutil.MockWaitForFile(closeGate),
		softCloseLine,
		testutil.MockWaitForFile(resumeGate),
		// The model streams on in the same turn, which reopens it.
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-more","role":"assistant"}}}`,
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"One more look."}}}`,
		`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
		`{"type":"assistant","message":{"id":"msg-more","role":"assistant","content":[{"type":"text","text":"One more look."}]}}`,
		testutil.MockWaitForFile(resultGate),
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the board went away"]}`,
	}})
	ack := f.spawnWorker(t, "watch the board")
	f.awaitTurnRunning(t, ack.Token)
	f.armLoop(t, ack.ThreadID, "board")
	release(t, closeGate)
	f.awaitRestHold(t, ack)

	release(t, resumeGate)
	waitUntil(t, 20*time.Second, func() bool {
		turn, found, err := f.app.store.GetTurnByThreadIndex(ack.ThreadID, 0)
		return err == nil && found && turn.CompletedAt == nil
	})
	wakes, err := f.app.store.ListAgentWakes(ack.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	if err := f.app.CancelThreadWake(ack.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v while its turn runs again; want it running", row)
	}
	release(t, resultGate)
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestErrored)
	if !strings.Contains(string(row.Answer), "the board went away") {
		t.Fatalf("settled %+v; want errored with the resumed turn's error", row)
	}
}

// A held request whose thread ran a later turn of tools and no text keeps,
// across a restart, the turn it answers from: at rest it answers with what
// the request's own turn wrote.
func TestARestartKeepsTheTurnAHeldSpawnAnswersFrom(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	ack := f.heldOnALoop(t, []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-tool","role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"make check"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"call-1","type":"tool_result","content":"ok","is_error":false}]}}`,
		mockClaudeResultLine,
	})
	if _, err := f.app.SendMessageWithOptions(t.Context(), ack.ThreadID, "check it again", SendMessageOptions{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		turn, found, err := f.app.store.GetTurnByThreadIndex(ack.ThreadID, 1)
		return err == nil && found && turn.CompletedAt != nil
	})
	f.app.threadRequestsWG.Wait()

	after := f.restart(t)
	after.watchAgentRows()
	after.app.sweepThreadRequestsAtBoot()
	after.app.recoverRestAtBoot()
	if !after.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatal("the boot did not hold the request again")
	}
	wakes, err := after.app.store.ListAgentWakes(ack.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	if err := after.app.CancelThreadWake(ack.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Watching the board." {
		t.Fatalf("settled with %q; want what the request's own turn wrote", row.Answer)
	}
}
