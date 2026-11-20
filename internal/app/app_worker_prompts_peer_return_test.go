package app

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A spawner's computer from before this release gives back a prompt its
// thread did not take now, not only one no thread there can hold, and says
// neither. While the worker's prompts are that thread's, such a return
// keeps the prompt the spawner's here and offers it again later.

// offerNow makes the hold of worker i's prompt due at once, as the wait
// before it is offered again running out does.
func (f *linkedWorkers) offerNow(t *testing.T, i int) {
	t.Helper()
	w := &f.app.workerPrompts
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[w.current[workerPromptKey{workerID: f.workers[i].ID, requestID: "approval-1"}]]
	if prompt == nil || prompt.remote == nil {
		t.Fatal("worker's prompt has no hold")
	}
	prompt.remote.offerAt = time.Time{}
}

// offerIn is how long from now the hold of worker i's prompt is offered
// again.
func (f *linkedWorkers) offerIn(t *testing.T, i int) time.Duration {
	t.Helper()
	w := &f.app.workerPrompts
	w.mu.Lock()
	defer w.mu.Unlock()
	prompt := w.byToken[w.current[workerPromptKey{workerID: f.workers[i].ID, requestID: "approval-1"}]]
	if prompt == nil || prompt.remote == nil {
		t.Fatal("worker's prompt has no hold")
	}
	return time.Until(prompt.remote.offerAt)
}

// An older spawner's return keeps the prompt its spawner's: the worker
// reads waiting on it, the person is not asked, and the prompt is listed
// again under a new token once the wait runs out, each wait twice the
// last up to the bound. Returned while it is no longer the spawner's, it
// goes to the person.
func TestAnOlderSpawnersReturnOffersThePromptAgainLater(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	worker := f.workers[0].ID
	token := f.ask(t, 0)
	waits := []time.Duration{}
	for range 7 {
		if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{Spawn: f.spawns[0], Token: token}); err != nil {
			t.Fatalf("the older spawner's return: %v", err)
		}
		f.drain(t)
		if route := f.route(t, worker); route == nil || route.ThreadID != f.lead || f.handedToPerson(worker) {
			t.Fatalf("after a return the prompt is routed to %+v, want it still the lead's on Desk", route)
		}
		if waiting := f.app.threadWaitingOn(worker); waiting.routedTo() != "the lead on Desk" {
			t.Fatalf("after a return the worker reads routed to %q, want the lead on Desk", waiting.routedTo())
		}
		if listed := f.list(t, 0); len(listed) != 0 {
			t.Fatalf("right after a return the spawner's listing = %+v, want nothing until the wait runs out", listed)
		}
		waits = append(waits, f.offerIn(t, 0))
		f.offerNow(t, 0)
		listed := f.list(t, 0)
		if len(listed) != 1 || listed[0].Token == token {
			t.Fatalf("once the wait ran out the spawner's listing = %+v, want the prompt under a new token", listed)
		}
		token = listed[0].Token
	}
	for i, wait := range waits {
		want := min(peerPromptOfferFirst<<i, peerPromptOfferMost)
		if wait > want || wait < want-5*time.Second {
			t.Fatalf("the waits = %v; want each twice the last from %v up to %v", waits, peerPromptOfferFirst, peerPromptOfferMost)
		}
	}

	// With a sidebar row and no run of the spawner's open, a return gives
	// it to the person here.
	execOnStore(t, f.dbPath, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20,
		run_open = 0, run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = ?`, f.spawns[0])
	if _, err := f.app.store.SetThreadPlacement([]string{worker}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if err := f.app.offerPeerPromptAgain(token); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if route := f.route(t, worker); route != nil || !f.handedToPerson(worker) {
		t.Fatalf("returned once it is no longer the spawner's, the prompt is routed to %+v, want the person here", route)
	}
}

// Across two computers, a spawner's computer whose returns carry no reason,
// as one before this release sends them, gives back the prompt of a worker
// whose spawner it can no longer hold it for: the worker's computer keeps
// it the spawner's, offers it again once the wait runs out, and that
// computer, taking the new token as a new prompt, gives it back again. The
// person there is never asked.
func TestAPromptAnOlderSpawnerKeepsGivingBackIsOfferedAgainAndStaysItsSpawners(t *testing.T) {
	t.Parallel()
	p, peer := promptOwnerOpPair(t)
	peer.reasonless.Store(true)
	p.openThere(t, 1)
	if _, _, err := p.source.store.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	offered := func() workerPrompt {
		w := &p.dest.workerPrompts
		w.mu.Lock()
		defer w.mu.Unlock()
		prompt := w.byToken[w.current[workerPromptKey{workerID: p.worker, requestID: "req-allow"}]]
		if prompt == nil || prompt.remote == nil {
			return workerPrompt{}
		}
		return *prompt
	}
	for returns := int32(1); returns <= 2; returns++ {
		waitUntil(t, 15*time.Second, func() bool {
			p.readOwnerState(t, p.source)
			p.drain(t)
			return peer.returns.Load() >= returns && offered().remote != nil && offered().remote.returns >= int(returns)
		})
		if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID {
			t.Fatalf("after %d returns the prompt there is routed to %+v (open %v), want its spawner here", returns, route, open)
		}
		if p.announcedToPerson("req-allow") {
			t.Fatalf("after %d returns the person there was asked", returns)
		}
		// The wait runs out.
		w := &p.dest.workerPrompts
		w.mu.Lock()
		w.byToken[w.current[workerPromptKey{workerID: p.worker, requestID: "req-allow"}]].remote.offerAt = time.Time{}
		w.mu.Unlock()
	}
}
