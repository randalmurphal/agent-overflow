package app

import (
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A spawn's Agents row is open while the thread it made is not at rest
// (docs/specs/standing-agents.md, "Owned workers"), whatever its request's
// state. A thread is at rest when it has no turn, no queued message, no
// open prompt, no running background work, no provider timer, no armed
// loop, watch or reminder, and no open work below it at any depth.
// liveWorkers is the one record of why the spawned threads are not at rest
// by their own state. It is memory, so the boot seeds it from the durable
// sources (seedLiveWorkers); the memory sources report themselves once a
// turn starts or a message is queued. The work below a thread is the
// listing's reads' to answer: they climb from each live worker and each
// piece of open work to every spawn above it, up to the root
// (store.ThreadAgentRequest.TargetOpen), and the row of a spawn whose
// thread has nothing of its own shows it waiting.
//
// The record has one writer, reassessWorker, which reads why a thread is
// not at rest and stores it. Two calls ask for that, each from the events
// that change its sources: noteWorkerLiveness for the sources triage holds
// (a turn, a prompt, a queue, a provider timer), from the Agents rows'
// emit tap (tapThreadAgentRows), the provider event handler
// (noteProviderWorkerLiveness), the process's end (unregisterSession) and
// the sends-pending observer (runSendsPendingWorker); noteWorkerWaiting for
// the durable ones (background work, loops, watches and reminders), from
// the background tray and wake frames and the boot (seedLiveWorkers).
// Every reader, the listing's reads, the row view, the change frames'
// gates and Past, consumes only what it stored.

// Why a worker is not at rest, in the order a row shows them. Waiting is
// below the rest: background work, a loop, watch or reminder, or a timer.
const (
	workerBlocked = "blocked"
	workerRunning = "running"
	workerQueued  = "queued"
	workerWaiting = "waiting"
)

// liveWorkers is the threads on this computer last found not at rest.
type liveWorkers struct {
	// reassess serializes reassessWorker, so a read of a thread's state
	// and its record are never overtaken by an older read's. It is held
	// across the store reads; mu, which readers take, is not.
	reassess sync.Mutex
	mu       sync.Mutex
	threads  map[string]liveWorker
}

// liveWorker is one thread not at rest: why, as a row shows it; the token
// of the local spawn that reaches it (its link), empty for a thread no
// local spawn made; and for a spawned thread, what its durable sources
// said when they last changed (waits), background when that is background
// work. A thread no spawn made is kept while triage holds it, so it costs
// one lookup each time it leaves rest, not one per change.
type liveWorker struct {
	workerRest
	link       string
	waits      workerRest
	background bool
}

// workerRest is why a thread is not at rest, and for a waiting one what it
// waits on, in the words of the row's activity line.
type workerRest struct {
	unrest string
	detail string
}

// workerUnrest says why threadID is not at rest, the zero value when it
// is: the one answer to that question, from every source in the order a
// row shows them. workerHeldUnrest asks what triage holds,
// workerDurableUnrest the store, workerTimerUnrest the provider's timers;
// a source added later is one more case in one of them, and the event that
// changes it one more call of noteWorkerLiveness or noteWorkerWaiting.
func (a *App) workerUnrest(threadID string) (workerRest, error) {
	if unrest := a.workerHeldUnrest(threadID); unrest != "" {
		return workerRest{unrest: unrest}, nil
	}
	waits, _, err := a.workerDurableUnrest(threadID)
	if err != nil || waits.unrest != "" {
		return waits, err
	}
	return a.workerTimerUnrest(threadID), nil
}

// workerHeldUnrest is why threadID is not at rest from what triage holds,
// no store read: waiting on a person for a prompt, in a turn, or holding
// messages queued for one.
func (a *App) workerHeldUnrest(threadID string) string {
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

// workerDurableUnrest is whether threadID waits on something it started
// that the store records: running background work (what
// runningBackgroundWorkForThread reports: Claude tasks and background
// agents, Codex subagents and terminals, remote commands), an armed loop
// or watch, or a reminder that has not fired. The detail is the background
// work's summary, or the loop, watch or reminder named as its wake rows name
// it (wakeWaitingLine); background says
// the wait is background work. It costs the background inventory's reads,
// then one read of the loops and watches and one of the reminders while
// nothing earlier answered.
func (a *App) workerDurableUnrest(threadID string) (workerRest, bool, error) {
	work, err := a.runningBackgroundWorkForThread(threadID, "")
	if err != nil {
		return workerRest{}, false, fmt.Errorf("read the running background work: %w", err)
	}
	if len(work) > 0 {
		detail := work[0].Summary
		if detail == "" {
			detail = work[0].ToolName
		}
		return workerRest{unrest: workerWaiting, detail: detail}, true, nil
	}
	wakes, err := a.store.ListAgentWakes(threadID)
	if err != nil {
		return workerRest{}, false, err
	}
	if len(wakes) > 0 {
		return workerRest{unrest: workerWaiting, detail: wakeWaitingLine(wakes[0].Kind, wakes[0].Label)}, false, nil
	}
	reminders, _, err := a.store.ListOpenThreadReminders(threadID, 1)
	if err != nil {
		return workerRest{}, false, err
	}
	if len(reminders) > 0 {
		return workerRest{unrest: workerWaiting, detail: wakeWaitingLine(threadWakeKindReminder, reminderLabel(string(reminders[0].Answer)))}, false, nil
	}
	return workerRest{}, false, nil
}

// wakeWaitingLine names a loop, watch or reminder a worker waits on as the
// timeline's wake rows name it (agentWakeOriginLabel in userMessageMeta.ts):
// "Loop · <label>", or the kind alone with no label.
func wakeWaitingLine(kind, label string) string {
	var name string
	switch kind {
	case store.AgentWakeLoop:
		name = "Loop"
	case store.AgentWakeWatch:
		name = "Watch"
	case threadWakeKindReminder:
		name = "Reminder"
	default:
		return label
	}
	if label == "" {
		return name
	}
	return name + " · " + label
}

// workerTimerUnrest is whether threadID's provider process will resume
// itself: a scheduled wakeup or an armed cron job, read as the idle reaper
// reads them (PendingWakeupAt within its grace), from memory.
func (a *App) workerTimerUnrest(threadID string) workerRest {
	if a.triage == nil {
		return workerRest{}
	}
	if at, ok := a.triage.PendingWakeupAt(threadID); ok && time.Now().Before(at.Add(a.wakeupReapGrace())) {
		return workerRest{unrest: workerWaiting}
	}
	return workerRest{}
}

// reassessWorker reads why threadID is not at rest and records it, the
// set's one write; a thread at rest is forgotten. durable says the
// thread's durable sources changed and are read again; otherwise the
// record keeps what they last said, which their own changes keep current.
// What it costs:
//
//   - triage's sources and the timers are memory;
//   - the spawn link is recorded, or one search for a thread not recorded;
//   - with durable, for a spawned thread only, workerDurableUnrest's reads.
//
// It reports whether the thread is a worker, one a local spawn made, now
// or as last recorded, so a change on either side is framed. A failed read
// leaves the record as it was, so the next change reads again, and is
// returned with worker true.
func (a *App) reassessWorker(threadID string, durable bool) (bool, error) {
	if a.store == nil {
		// No store, no spawns: nothing here is a worker.
		return false, nil
	}
	w := &a.liveWorkers
	w.reassess.Lock()
	defer w.reassess.Unlock()
	w.mu.Lock()
	before, known := w.threads[threadID]
	w.mu.Unlock()
	next := liveWorker{link: before.link, waits: before.waits, background: before.background}
	if !known {
		link, _, err := a.store.SpawnLinkOf(threadID)
		if err != nil {
			return true, err
		}
		next.link = link
	}
	if durable && next.link != "" {
		waits, background, err := a.workerDurableUnrest(threadID)
		if err != nil {
			return true, err
		}
		next.waits, next.background = waits, background
	}
	if held := a.workerHeldUnrest(threadID); held != "" {
		next.workerRest = workerRest{unrest: held}
	} else if next.waits.unrest != "" {
		next.workerRest = next.waits
	} else if next.link != "" {
		next.workerRest = a.workerTimerUnrest(threadID)
	}
	w.mu.Lock()
	if next.unrest == "" {
		delete(w.threads, threadID)
	} else {
		if w.threads == nil {
			w.threads = map[string]liveWorker{}
		}
		w.threads[threadID] = next
	}
	w.mu.Unlock()
	return next.link != "", nil
}

// waitsOnBackground reports whether threadID's durable sources last said
// it waits on background work.
func (a *App) waitsOnBackground(threadID string) bool {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threads[threadID].background
}

// noteProviderWorkerLiveness reports a provider's own timers changing: a
// scheduled wakeup set or cleared, or its armed cron jobs listed again
// (workerTimerUnrest). Triage records both before this runs.
func (a *App) noteProviderWorkerLiveness(threadID string, evt provider.ProviderEvent) {
	if evt.Kind == provider.EventSessionWakeup || evt.Kind == provider.EventScheduledJobs {
		a.noteWorkerLiveness(threadID)
	}
}

// seedLiveWorkers asks, at boot, about each thread a durable source says
// may wait: one with a loop or watch armed, a reminder that has not fired,
// or a remote command pending. Each source is one read; each thread is then
// reassessed off the boot's path. A source that cannot be read is logged,
// and the others still seed.
func (a *App) seedLiveWorkers() {
	for _, source := range []struct {
		what string
		read func() ([]string, error)
	}{
		{"loops and watches", a.store.ThreadsWithLiveAgentWakes},
		{"reminders", a.store.ThreadsWithOpenReminders},
		{"remote commands", a.store.RemoteWatchThreadIDs},
	} {
		ids, err := source.read()
		if err != nil {
			log.Printf("agent rows: seed the workers waiting on %s: %v", source.what, err)
			continue
		}
		for _, id := range ids {
			a.noteWorkerWaiting(id)
		}
	}
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

// liveWorkerUnrest is why the thread spawn reaches is not at rest, and what
// it waits on, as last recorded, while spawn is that thread's link: the
// zero value when it is at rest, and for any other spawn of it.
func (a *App) liveWorkerUnrest(spawn store.ThreadRequest) workerRest {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	if worker := w.threads[spawn.TargetThreadID]; worker.link != "" && worker.link == spawn.Token {
		return worker.workerRest
	}
	return workerRest{}
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
