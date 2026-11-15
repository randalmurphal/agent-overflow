package app

import (
	"sync"

	"agent-overflow/internal/store"
)

// A spawn's Agents row is open while the thread it made is not at rest
// (docs/specs/standing-agents.md, "Owned workers"), whatever its request's
// state: a worker that replied and keeps working, one a person continued
// from its own pane, one blocked on a prompt or holding queued messages.
// liveWorkers is the one record of which threads those are. It is memory,
// as parked calls are: after a restart nothing is live until a turn starts
// or a message is queued, and each of those reports itself.
//
// The record has one writer, reassessWorker, which reads why a thread is
// not at rest (workerUnrest) and stores it. noteWorkerLiveness is the one
// way a source's change asks for that: the Agents rows' emit tap calls it
// for turns, prompts and session deaths (tapThreadAgentRows), the
// sends-pending observer for queue changes (runSendsPendingWorker). Every
// reader, the listing's reads, the row view, the change frames' gates and
// Past, consumes only what it stored.

// Why a worker is not at rest, in the order a row shows them.
const (
	workerBlocked = "blocked"
	workerRunning = "running"
	workerQueued  = "queued"
)

// liveWorkers is the threads on this computer last found not at rest.
type liveWorkers struct {
	// reassess serializes reassessWorker, so a read of a thread's state
	// and its record are never overtaken by an older read's. It is held
	// across the spawn link's lookup; mu, which readers take, is not.
	reassess sync.Mutex
	mu       sync.Mutex
	threads  map[string]liveWorker
}

// liveWorker is one thread not at rest: why, and the token of the local
// spawn that reaches it (its link), empty for a thread no local spawn made.
// Such a thread is kept too, so it costs one lookup each time it leaves
// rest, not one per change.
type liveWorker struct {
	unrest string
	link   string
}

// workerUnrest says why threadID is not at rest, empty when it is: waiting
// on a person for a prompt, in a turn, or holding messages queued for one.
// Every reason a worker counts as not at rest is answered here; a source
// added later (its running background work, its armed loops, watches and
// reminders) is one more case, and the event that changes it one more call
// of noteWorkerLiveness.
func (a *App) workerUnrest(threadID string) string {
	if a.triage == nil {
		return ""
	}
	live := threadToolsLiveState(a.triage.LiveStateSnapshotForThread(threadID))
	switch {
	case live.Blocked():
		return workerBlocked
	case live.ActiveTurn:
		return workerRunning
	case live.PendingSends:
		return workerQueued
	}
	return ""
}

// reassessWorker reads why threadID is not at rest and records it, the
// set's one write. A thread at rest is forgotten. One newly not at rest
// costs one search of its spawn link; one already recorded keeps its link.
// It reports whether the thread is a worker, one a local spawn made, now
// or as last recorded, so a change on either side is framed. A failed
// lookup records nothing, so the next change looks again, and is returned
// with worker true.
func (a *App) reassessWorker(threadID string) (bool, error) {
	w := &a.liveWorkers
	w.reassess.Lock()
	defer w.reassess.Unlock()
	unrest := a.workerUnrest(threadID)
	w.mu.Lock()
	before, known := w.threads[threadID]
	switch {
	case unrest == "":
		delete(w.threads, threadID)
		w.mu.Unlock()
		return before.link != "", nil
	case known:
		before.unrest = unrest
		w.threads[threadID] = before
		w.mu.Unlock()
		return before.link != "", nil
	}
	w.mu.Unlock()
	link, _, err := a.store.SpawnLinkOf(threadID)
	if err != nil {
		return true, err
	}
	w.mu.Lock()
	if w.threads == nil {
		w.threads = map[string]liveWorker{}
	}
	w.threads[threadID] = liveWorker{unrest: unrest, link: link}
	w.mu.Unlock()
	return link != "", nil
}

// forgetLiveWorker records a thread's delete: the thread is forgotten, and
// every other recorded worker's spawn link is looked up again, since the
// delete takes the spawns the thread made with it. Bounded by the threads
// not at rest; a failed lookup keeps that worker's last link.
func (a *App) forgetLiveWorker(deleted string) error {
	w := &a.liveWorkers
	w.reassess.Lock()
	defer w.reassess.Unlock()
	w.mu.Lock()
	delete(w.threads, deleted)
	ids := make([]string, 0, len(w.threads))
	for id, worker := range w.threads {
		if worker.link != "" {
			ids = append(ids, id)
		}
	}
	w.mu.Unlock()
	var firstErr error
	for _, id := range ids {
		link, _, err := a.store.SpawnLinkOf(id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		w.mu.Lock()
		if worker, ok := w.threads[id]; ok {
			worker.link = link
			w.threads[id] = worker
		}
		w.mu.Unlock()
	}
	return firstErr
}

// liveWorkerUnrest is why the thread spawn reaches is not at rest, as last
// recorded, while spawn is that thread's link: empty when it is at rest,
// and for any other spawn of it.
func (a *App) liveWorkerUnrest(spawn store.ThreadRequest) string {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	if worker := w.threads[spawn.TargetThreadID]; worker.link != "" && worker.link == spawn.Token {
		return worker.unrest
	}
	return ""
}

// liveWorker reports whether threadID is a worker not at rest, as last
// recorded.
func (a *App) liveWorker(threadID string) bool {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threads[threadID].link != ""
}

// liveWorkerThreads lists the workers not at rest, by thread id, for the
// listing's reads, which join each to its spawn link.
func (a *App) liveWorkerThreads() []string {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.threads))
	for id, worker := range w.threads {
		if worker.link != "" {
			out = append(out, id)
		}
	}
	return out
}

// liveWorkerLinks lists the spawn links of the workers not at rest, by
// token.
func (a *App) liveWorkerLinks() []string {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.threads))
	for _, worker := range w.threads {
		if worker.link != "" {
			out = append(out, worker.link)
		}
	}
	return out
}

// liveSpawnTokens lists the spawns, by token, whose thread is not at rest:
// each local worker's link, and each spawn on a paired computer whose last
// agent_rows answer said its thread is not. Past leaves them out: each is
// still open.
func (a *App) liveSpawnTokens() []string {
	out := a.liveWorkerLinks()
	c := &a.threadAgentPeers
	c.mu.Lock()
	defer c.mu.Unlock()
	for token, entry := range c.entries {
		if entry.listing.Worker != "" {
			out = append(out, token)
		}
	}
	return out
}
