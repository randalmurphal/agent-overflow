package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// A request another thread queued behind this thread's turn is an incoming
// row: the queue names its origin, and withdrawing it takes it out of the
// queue and tells the sender's agent the user withdrew it.
func TestThreadAgentIncomingRequestWithdrawTellsTheSender(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	target := f.busyThread(t, "busy")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "look at this too", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sendID := threadRequestSendID(ack.Token)
	queued := f.app.queueSnapshotForThread(target.ID)
	if len(queued) != 1 || queued[0].Origin != usermessage.OriginAgentThread || queued[0].OriginThread == nil ||
		queued[0].OriginThread.ThreadID != f.caller.ID {
		t.Fatalf("the queue does not name the message's origin: %+v", queued)
	}
	if got := f.app.incomingAgentSendIDs(target.ID); !slices.Equal(got, []string{sendID}) {
		t.Fatalf("incoming = %v, want %s", got, sendID)
	}

	stop, err := f.app.StopThreadAgentIncoming(t.Context(), target.ID, sendID)
	if err != nil || stop.Outcome != threadAgentStopped || !stop.Incoming || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentIncoming = %+v, %v", stop, err)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("the withdrawn message is still queued: %+v", rows)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestCancelled)
	if !strings.Contains(string(row.Answer), "withdrew") || !row.Notify {
		t.Fatalf("the request = %q notify=%v, want the withdrawal told to an armed sender", row.Answer, row.Notify)
	}
	var wake string
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				wake = queued.Message
				return true
			}
		}
		return false
	})
	if !strings.Contains(wake, "The user withdrew it") {
		t.Fatalf("the sender's wake does not say the user withdrew it: %q", wake)
	}
	if again, err := f.app.StopThreadAgentIncoming(t.Context(), target.ID, sendID); err != nil || again.Outcome != threadAgentEnded {
		t.Fatalf("a second withdraw = %+v, %v; want ended", again, err)
	}
}

// An answer queued for this thread's own request is an incoming row too;
// withdrawing it leaves the answer owed and disarmed, for thread_status.
func TestThreadAgentIncomingReplyWithdrawLeavesTheAnswerOwed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	f.holdCallerTurn(t)
	ack := f.runningRequest(t, "rebuild the index")
	if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	sendID := threadWakeSendID(ack.Token)
	waitUntil(t, 10*time.Second, func() bool {
		return slices.Contains(f.app.incomingAgentSendIDs(f.caller.ID), sendID)
	})

	stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.caller.ID, sendID)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("StopThreadAgentIncoming = %+v, %v", stop, err)
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("the withdrawn answer is still queued: %+v", rows)
	}
	if row := f.request(t, ack.Token); row.Notify || row.DeliveredHow != "" {
		t.Fatalf("the answer = notify %v delivered %q, want owed again and disarmed", row.Notify, row.DeliveredHow)
	}
}

// Stop All withdraws incoming rows with the rest; a person's queued message
// is not one.
func TestThreadAgentStopAllWithdrawsIncomingAndRefusesAPersonsMessage(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	target := f.busyThread(t, "busy")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "and this", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := f.app.StopThreadAgentIncoming(t.Context(), target.ID, "composer-send-1"); err == nil {
		t.Fatal("a person's queued message was withdrawn as an agent's")
	}

	stops, err := f.app.StopThreadAgentRows(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("StopThreadAgentRows: %v", err)
	}
	if len(stops) != 1 || !stops[0].Incoming || stops[0].Token != threadRequestSendID(ack.Token) || stops[0].Outcome != threadAgentStopped {
		t.Fatalf("Stop All = %+v, want the incoming request withdrawn", stops)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestCancelled)
}
