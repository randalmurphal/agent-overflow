package app

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// An interruption's cause crosses computers as data. The answering computer
// reports its code and argument beside its words; the asking computer
// records them, with what the turn had written as the answer, and renders
// its own words and facts from them, whatever words came with them. An
// answer with no cause it can read, from an earlier build or with a code it
// does not know, is recorded as its text and shown whole, naming no stopper.
func TestAnInterruptionsCauseCrossesComputersAsData(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	written := strings.Repeat("Halfway there. ", threadtools.WakePreviewBytes/15+40)

	collected := func(t *testing.T, reword func(*ThreadPeerRequest), settle func(receipt store.ThreadRequestReceipt)) store.ThreadRequest {
		t.Helper()
		spawn := pair.spawnThere(t, "work there", nil)
		receipt := pair.receipt(t, spawn.Token)
		settle(receipt)
		if settled := pair.receipt(t, spawn.Token); settled.State != store.ThreadReceiptInterrupted {
			t.Fatalf("the destination settled %s as %q", spawn.Token, settled.State)
		}
		if reword != nil {
			faults.reword.Store(&reword)
		}
		pair.poll(t)
		faults.reword.Store(nil)
		row := pair.request(t, spawn.Token)
		if row.State != store.ThreadRequestInterrupted {
			t.Fatalf("the source collected %s as %q", spawn.Token, row.State)
		}
		return row
	}
	settleThere := func(cause threadtools.Cause, text string) func(store.ThreadRequestReceipt) {
		return func(receipt store.ThreadRequestReceipt) {
			if err := pair.dest.settleThreadReceipt(receipt.Token, store.ThreadReceiptOpenStates(), interruptedSettlement(cause, text)); err != nil {
				t.Fatal(err)
			}
		}
	}
	wakeOf := func(t *testing.T, row store.ThreadRequest) (string, threadtools.MessageFacts) {
		t.Helper()
		message, err := soloThreadWakeMessage(row, false)
		if err != nil {
			t.Fatal(err)
		}
		return pair.source.threadWakeFor(message).Message()
	}
	statusWords := func(row store.ThreadRequest) string { return pair.adapter().requestStateView(row).Answer }

	t.Run("a move, reworded there", func(t *testing.T) {
		row := collected(t, func(r *ThreadPeerRequest) {
			if r.Cause != nil {
				r.Answer = "Moved off this machine (reworded by another build)."
			}
		}, func(receipt store.ThreadRequestReceipt) {
			if _, err := pair.dest.stopThreadRequestsForMovedThread(context.Background(), receipt.TargetThreadID, "Desk"); err != nil {
				t.Fatal(err)
			}
		})
		if row.Cause != threadtools.CauseMoved || row.CauseArg != "Desk" || len(row.Answer) != 0 {
			t.Fatalf("collected cause %q arg %q answer %q, want the move to Desk and no text", row.Cause, row.CauseArg, row.Answer)
		}
		const words = "The thread was moved to Desk before it answered."
		if got := statusWords(row); got != words {
			t.Errorf("thread_status reads %q, want %q", got, words)
		}
		text, facts := wakeOf(t, row)
		if !strings.HasSuffix(text, "]: "+words) || facts.StoppedBy != "a move to Desk" || len(facts.Body) != 0 {
			t.Errorf("wake %q with facts %+v, want this computer's words and the move as the stopper", text, facts)
		}
	})

	t.Run("a session that ended mid-turn, with what the turn wrote", func(t *testing.T) {
		row := collected(t, nil, settleThere(threadtools.Cause{Code: threadtools.CauseSessionEndedMidTurn}, written))
		if row.Cause != threadtools.CauseSessionEndedMidTurn || string(row.Answer) != written {
			t.Fatalf("collected cause %q with %d answer bytes, want the session's end with what the turn wrote", row.Cause, len(row.Answer))
		}
		opening := "The thread's session ended during the turn that read the request." + threadtools.SessionEndedWrittenSoFar
		if got := statusWords(row); got != opening+written {
			t.Errorf("thread_status reads %.120q, want the sentence and what the turn wrote", got)
		}
		text, facts := wakeOf(t, row)
		if facts.StoppedBy != "the session ended during the turn that read the request" || !facts.Clipped ||
			facts.BodyOf(text) != written[:threadtools.WakePreviewBytes] || !strings.Contains(text, "]: "+opening) {
			t.Errorf("wake facts %+v, want what the turn wrote as the body, clipped as an answer", facts)
		}
	})

	t.Run("an earlier build that reports no cause", func(t *testing.T) {
		row := collected(t, func(r *ThreadPeerRequest) { r.Cause = nil },
			settleThere(threadtools.Cause{Code: threadtools.CauseSessionEndedUnread}, ""))
		const words = "The thread's session ended before it read the request."
		if row.Cause != "" || row.CauseArg != "" || string(row.Answer) != words {
			t.Fatalf("collected cause %q answer %q, want the words alone", row.Cause, row.Answer)
		}
		if got := statusWords(row); got != words {
			t.Errorf("thread_status reads %q, want %q", got, words)
		}
		text, facts := wakeOf(t, row)
		if facts.StoppedBy != "" || facts.BodyOf(text) != words {
			t.Errorf("wake %q with facts %+v, want the words whole as the body and no stopper", text, facts)
		}
	})

	t.Run("the user's Stop, with what the turn said", func(t *testing.T) {
		const said = "Half done."
		var reported ThreadPeerRequest
		row := collected(t, func(r *ThreadPeerRequest) { reported = *r },
			settleThere(threadtools.Cause{Code: threadtools.CauseUserStop}, said))
		// An earlier build reads the answer and its kind: what the turn
		// said, as its final message.
		if reported.Cause == nil || reported.Cause.Code != threadtools.CauseUserStop || reported.Cause.Written != said ||
			reported.Answer != said || reported.AnswerKind != store.ThreadAnswerFinal {
			t.Fatalf("the destination reported %q (%s) with cause %+v, want what the turn said beside the user's Stop",
				reported.Answer, reported.AnswerKind, reported.Cause)
		}
		if row.Cause != threadtools.CauseUserStop || string(row.Answer) != said || row.AnswerKind != store.ThreadAnswerFinal {
			t.Fatalf("collected cause %q answer %q (%s), want the user's Stop with what the turn said", row.Cause, row.Answer, row.AnswerKind)
		}
		if got := statusWords(row); got != said {
			t.Errorf("thread_status reads %q, want %q", got, said)
		}
		text, facts := wakeOf(t, row)
		if !strings.HasSuffix(text, "]: "+threadtools.WakePersonStopped+" Its last message:\n"+said) ||
			facts.StoppedBy != "the user" || facts.BodyOf(text) != said {
			t.Errorf("wake %q with facts %+v, want the user named as the stopper and what the turn said as the body", text, facts)
		}
	})

	t.Run("a code this computer does not know, on a final answer", func(t *testing.T) {
		const said = "Half done."
		row := collected(t, func(r *ThreadPeerRequest) {
			// A newer computer's own cause for a turn whose last message
			// it reports as the answer.
			r.Cause = &ThreadPeerCause{Code: "evicted", Written: said}
			r.Answer, r.AnswerKind = said, store.ThreadAnswerFinal
		}, settleThere(threadtools.Cause{Code: threadtools.CauseTurn}, ""))
		if row.Cause != "" || string(row.Answer) != said || row.AnswerKind != store.ThreadAnswerFinal {
			t.Fatalf("collected cause %q answer %q (%s), want the text alone", row.Cause, row.Answer, row.AnswerKind)
		}
		text, facts := wakeOf(t, row)
		if facts.StoppedBy != "" || facts.BodyOf(text) != said || strings.Contains(text, threadtools.WakePersonStopped) {
			t.Errorf("wake %q with facts %+v, want the text whole as the body and no stopper", text, facts)
		}
	})

	t.Run("a code this computer does not know", func(t *testing.T) {
		row := collected(t, func(r *ThreadPeerRequest) {
			if r.Cause != nil {
				r.Cause.Code = "evicted"
			}
		}, settleThere(threadtools.Cause{Code: threadtools.CauseTurn}, ""))
		const words = "The turn was interrupted before it answered."
		if row.Cause != "" || string(row.Answer) != words {
			t.Fatalf("collected cause %q answer %q, want the words alone", row.Cause, row.Answer)
		}
		if _, facts := wakeOf(t, row); facts.StoppedBy != "" {
			t.Errorf("an unknown code names %q as the stopper", facts.StoppedBy)
		}
	})
}

// A turn that wrote nothing but white space wrote nothing: its
// interruption's answer is the cause's sentence alone.
func TestAnInterruptionKeepsNoBlankText(t *testing.T) {
	t.Parallel()
	cause := threadtools.Cause{Code: threadtools.CauseSessionEndedMidTurn}
	if settlement := interruptedSettlement(cause, " \n\t"); len(settlement.Answer) != 0 || settlement.Cause != cause.Code {
		t.Fatalf("settlement = %+v, want the cause and no text", settlement)
	}
	if settlement := interruptedSettlement(cause, "Half done."); string(settlement.Answer) != "Half done." {
		t.Fatalf("settlement = %+v, want what the turn wrote", settlement)
	}
}
