package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// storeCost is what a path cost the store: read statements, row writes and
// writer transactions.
type storeCost struct{ reads, writes, commits uint64 }

func storeCostOf(s *store.Store) storeCost {
	return storeCost{s.ReadCount(), s.WriteCount(), s.CommitCount()}
}

func (c storeCost) since(before storeCost) storeCost {
	return storeCost{c.reads - before.reads, c.writes - before.writes, c.commits - before.commits}
}

// A send into a busy thread on this computer writes its request and its
// receipt in one transaction and resolves its target once, and its
// settlement writes the receipt, the request and the acknowledgement in
// one transaction more than the turn end it rides on.
func TestALocalSendAndItsSettlementTakeOneTransactionEach(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)

	// A turn end with no request on it, for the settlement's measure.
	h.text(t, "a-plain", "warming up")
	before := storeCostOf(f.app.store)
	h.turnComplete(t)
	plainEnd := storeCostOf(f.app.store).since(before)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})

	before = storeCostOf(f.app.store)
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: h.thread.ID, Message: "check the changelog"})
	if err != nil {
		t.Fatal(err)
	}
	send := storeCostOf(f.app.store).since(before)
	// The request and receipt (one), and the queue's row with its draft
	// guard and the thread's stamps. Before the request and receipt were one
	// write, they were four and the target was resolved twice: 9 commits,
	// 12 writes.
	if send.commits > 5 || send.writes > 10 {
		t.Errorf("the send cost %+v, want at most 5 commits and 10 writes", send)
	}
	if row := f.request(t, ack.Token); row.State == store.ThreadRequestUnconfirmed || row.TargetThreadID != h.thread.ID {
		t.Fatalf("request after the send = %+v", row)
	}

	queued := f.app.triage.QueuedFlushItems(h.thread.ID)
	f.app.dispatchFlush(h.thread.ID, queued)
	head, ok := f.app.triage.PeekPendingSendHeadForTest(h.thread.ID)
	if !ok {
		t.Fatal("no pending send")
	}
	h.lifecycle(t, head.ExpectedProviderItemID, provider.CommandStarted)
	h.echo(t, head.ExpectedProviderItemID, "check the changelog")
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after its delivery = %q, want running", row.State)
	}
	h.text(t, "a-answer", "The changelog lists the fix.")
	before = storeCostOf(f.app.store)
	h.turnComplete(t)
	settled := storeCostOf(f.app.store).since(before)
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestFinished {
		t.Fatalf("request after its turn = %q, want finished", row.State)
	}
	if receipt := f.receipt(t, ack.Token); receipt.CollectedRevision != receipt.Revision || len(receipt.Answer) == 0 {
		t.Fatalf("receipt after the settlement = %+v, want it acknowledged and keeping its answer", receipt)
	}
	// One transaction for the receipt, the request and the acknowledgement,
	// on top of the turn end's own; the receipt read once. Before: three
	// transactions, and the receipt read three times and the request twice.
	if extra := settled.commits - plainEnd.commits; extra != 1 {
		t.Errorf("the settlement added %d transactions to the turn end (%+v against %+v), want 1", extra, settled, plainEnd)
	}
	if extra := settled.reads - plainEnd.reads; extra > 10 {
		t.Errorf("the settlement added %d reads to the turn end, want at most 10", extra)
	}
}

// A watch fire reads its row once after its run, for the wake it renders
// and queues, and once more under the queue's lock for a cancel that landed
// meanwhile; the claim and the finish tell the tray from the rows they
// wrote.
func TestAWatchFireReadsItsRowOnce(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := testThread("costs-fire")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	testAssistant(t, a, thread.ID)
	// A live session mid-turn, so the wake waits in the queue and nothing
	// starts a provider.
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, filepath.Join(t.TempDir(), "stdin.jsonl")), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	a.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "costs-tok", Claude: sess, Liveness: newSessionLiveness(time.Now())})
	handle := startedSessionEventHandler(a, thread.ID, "costs-tok", string(provider.Claude))
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, a, thread.ID)

	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "news",
		Command: "echo news", IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	framesBefore := wakeRowsChanged(rec, thread.ID)
	reads := a.store.ReadCount()
	if !a.fireAgentWake(watch, time.Now(), agentWakeFire{at: time.Now().UnixMilli()}) {
		t.Fatal("the watch did not fire")
	}
	waitUntil(t, 10*time.Second, func() bool {
		changes := wakeChanges(rec, thread.ID)[framesBefore:]
		last := len(changes) - 1
		return last >= 0 && changes[last].Row != nil && changes[last].Row.State == store.AgentWakeArmed && changes[last].Row.LastOutcome != ""
	})
	reads = a.store.ReadCount() - reads
	changes := wakeChanges(rec, thread.ID)[framesBefore:]
	if first := changes[0].Row; first == nil || first.State != store.AgentWakeRunning {
		t.Fatalf("the claim's frame = %+v, want the running row", changes[0])
	}
	if last := changes[len(changes)-1].Row; last.LastOutcome != agentWakeOutcomeDelivered {
		t.Fatalf("the finish's frame = %+v, want the delivered row", last)
	}
	// Before: the claim's and the finish's frames each read the row, the
	// delivery read it again after the render's read, and read the thread
	// before the queue's own read did: 23.
	if reads > 19 {
		t.Errorf("the fire read the store %d times, want at most 19", reads)
	}
}
