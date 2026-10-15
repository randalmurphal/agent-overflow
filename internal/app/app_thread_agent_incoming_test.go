package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// A request another thread queued for this thread is an incoming
// row: the queue names its origin, and withdrawing it takes it out of the
// queue and tells the sender's agent the user withdrew it.
func TestThreadAgentIncomingRequestWithdrawTellsTheSender(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	// The sender's wake starts its session once queued.
	f.mockClaudeHoldingTheTurn(t, "working")
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
	f.app.waitAgentNoticeWork()
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
	// The sender's wake starts its session once queued.
	f.mockClaudeHoldingTheTurn(t, "working")
	f.holdCallerTurn(t)
	target := f.busyThread(t, "busy")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "and this", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := f.app.registerQueueItem(target.ID, "mine too", SendMessageOptions{SendID: "composer-send-1"}, injectedQueueOptions{preserveDraft: true}); err != nil {
		t.Fatalf("queue the person's message: %v", err)
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
	f.app.waitAgentNoticeWork()
}

// A loop's wake, a script's outside send and a worker's question are
// incoming rows too, each withdrawn the way a queue drop gives it back: the
// wake's row records it, the send leaves a notice, the question goes back to
// the user in the worker.
func TestThreadAgentIncomingWakesSendsAndQuestionsWithdrawToTheirOwners(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "incoming-owners")
	board := holdAgentWake(t, a, thread, store.AgentWakeLoop, "board")
	sendID := sendWithin(t, a, thread.ID, "deploy 13 is done", 5*time.Second)
	a.waitAgentNoticeWork()
	prompt := a.workerPrompts.add(workerPrompt{
		workerID: "worker-thread", agentID: thread.ID, requestID: "question-1",
	}, func(string, string) bool { return true })
	if err := a.queueAgentNotice(agentNotice{
		threadID: thread.ID, sendID: prompt.sendID(),
		origin:     usermessage.OriginAgentWake,
		originWake: &usermessage.OriginWake{Kind: "question", Label: "worker", ThreadID: "worker-thread"},
		prepare:    func() (string, bool, error) { return `Thread "worker" asks`, true, nil },
	}); err != nil {
		t.Fatalf("queue the worker's question: %v", err)
	}
	a.waitAgentNoticeWork()
	wakeID := agentWakeSendID(board.ID, 1)
	if got := a.incomingAgentSendIDs(thread.ID); !slices.Equal(got, []string{wakeID, sendID, prompt.sendID()}) {
		t.Fatalf("incoming = %q", got)
	}

	for _, id := range []string{wakeID, sendID, prompt.sendID()} {
		stop, err := a.StopThreadAgentIncoming(t.Context(), thread.ID, id)
		if err != nil || stop.Outcome != threadAgentStopped {
			t.Fatalf("withdraw %s = %+v, %v", id, stop, err)
		}
	}
	if durable, memory := queuedSendIDs(t, a, thread.ID); len(durable)+len(memory) != 0 {
		t.Fatalf("still queued: durable %q, memory %q", durable, memory)
	}
	saved, found, err := a.store.GetAgentWake(board.ID)
	if err != nil || !found || saved.Undelivered != agentWakeOutcomeWithdrawn {
		t.Fatalf("the loop records %q (found=%v err=%v), want %q", saved.Undelivered, found, err, agentWakeOutcomeWithdrawn)
	}
	want := "A message sent with agent-overflow thread send was not delivered: the user withdrew it from the queue."
	if rows := noticeRows(t, a, thread.ID, want); len(rows) != 1 {
		t.Fatalf("notices %q, want one %q", rows, want)
	}
	if a.workerPrompts.isRouted(prompt.token) {
		t.Fatal("the worker's question is still routed to the agent; it should be back with the user")
	}
}

// A remote job's completion and a workflow run's wake are incoming rows: the
// queue marks them an agent's. Withdrawn, the completion goes back to its
// watch as a queue drop sends it, and the workflow's wake is dropped. A
// person's queued message is not an incoming row and cannot be withdrawn as
// one.
func TestThreadAgentIncomingCompletionsAndWorkflowWakes(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	thread := remoteWatchThread(t, a, string(provider.Claude))
	w := completedRemoteWatch(t, a, thread)
	if err := a.queueAgentNotice(a.remoteCompletionNotice(w, remoteCompletionOutput{Tail: "done"})); err != nil {
		t.Fatalf("queue the completion: %v", err)
	}
	a.waitAgentNoticeWork()
	if err := a.queueWorkflowWake(thread.ID, "wf-1", "Phase build finished.", nil); err != nil {
		t.Fatalf("queue the workflow wake: %v", err)
	}
	person, err := a.registerQueueItem(thread.ID, "and mine", SendMessageOptions{SendID: "composer-1"}, injectedQueueOptions{preserveDraft: true})
	if err != nil {
		t.Fatalf("queue the person's message: %v", err)
	}
	completion := remoteCompletionSendID(w)
	incoming := a.incomingAgentSendIDs(thread.ID)
	if len(incoming) != 2 || incoming[0] != completion || !strings.HasPrefix(incoming[1], workflowWakeSendIDRoot) {
		t.Fatalf("incoming = %q, want the completion and the workflow wake", incoming)
	}
	if _, err := a.StopThreadAgentIncoming(t.Context(), thread.ID, person.SendID); err == nil {
		t.Fatal("withdrew the person's message as an incoming one")
	}

	for _, id := range incoming {
		stop, err := a.StopThreadAgentIncoming(t.Context(), thread.ID, id)
		if err != nil || stop.Outcome != threadAgentStopped {
			t.Fatalf("withdraw %s = %+v, %v", id, stop, err)
		}
	}
	watch, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || watch.Notification != "pending" {
		t.Fatalf("watch = %+v, %v; want its completion pending again", watch, err)
	}
	if durable, memory := queuedSendIDs(t, a, thread.ID); !slices.Equal(durable, []string{person.SendID}) || !slices.Equal(memory, []string{person.SendID}) {
		t.Fatalf("queue = %q / %q, want the person's message alone", durable, memory)
	}
}
