package app

import (
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A spawn's Agents row is open while the thread it made is not at rest
// (docs/specs/standing-agents.md, "Owned workers"), whatever its request's
// state. A thread is at rest when it has no turn, no queued message, no
// open prompt, no running background work, no provider timer, no armed
// loop, watch or reminder, and no open work below it at any depth that its
// own runs started (docs/specs/standing-agents.md, "At rest").
// liveWorkers is the one record of why the spawned threads are not at rest
// by their own state. It is memory, so the boot records every thread a
// recovered source names before any request settles on rest
// (recoverRestAtBoot); the memory sources report themselves once a turn
// starts or a message is queued. The work below a thread is the
// listing's reads' to answer: they climb from each live worker and each
// piece of open work to every spawn above it, up to the root, through a
// spawn only while a run of its caller's is open on its thread
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
// the durable ones (background work, loops, watches, reminders, answers
// on their way, workflow runs and unanswered questions), from the
// background tray, wake, request, workflow and question frames and the
// boot (recoverRestAtBoot). Its readers, the listing's reads, the row
// view, the change frames' gates, Past and Stop All's selection, consume
// only what it stored. A decision that cannot be taken back, a held request
// settling or the retention sweep clearing a spawn, reads neither it nor
// the marks: it reads the threads' own sources (readRest).

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

// liveWorker is one thread not at rest: why, as a row shows it; the spawn
// that made it (its link, workerLinkOf), empty for a thread no spawn made;
// and for a spawned thread, what its durable sources
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
// waits on, in the words of the row's activity line; below marks a thread
// waiting only on open work below it (threadRest).
type workerRest struct {
	unrest string
	detail string
	below  bool
}

// threadRest says why threadID is not at rest, the zero value when it is:
// its own state (workerUnrest) or, with nothing of its own, open work at
// any depth below it (store.ThreadAgentState.OpenBelow), which reads
// waiting with no detail: its own state read now, the work below as the
// listing records it. thread_status with thread_ids waits for it
// (threadStateNow), and a paired computer answers with it, for its
// agent_rows (answerThreadAgentRows) and for a request's poll
// (threadPeerRequestState). The Agents rows read the same two halves from
// what was recorded: liveWorkers for the thread's own state, the listing's
// TargetOpen for the work below it. A decision that cannot be taken back
// reads neither record (readRest).
func (a *App) threadRest(threadID string) (workerRest, error) {
	rest, err := a.workerUnrest(threadID)
	if err != nil || rest.unrest != "" {
		return rest, err
	}
	state, err := a.threadAgentState(threadID)
	if err != nil {
		return workerRest{}, err
	}
	if state.OpenBelow {
		return workerRest{unrest: workerWaiting, below: true}, nil
	}
	return workerRest{}, nil
}

// threadAgentState reads what threadID's Agents listing says of it, with
// the parked waits and the live workers as a listing reads them.
func (a *App) threadAgentState(threadID string) (store.ThreadAgentState, error) {
	view := newThreadAgentView(a)
	state, err := a.store.ReadThreadAgentState(store.ThreadAgentStateRead{
		Thread: threadID, Waiting: view.waiting, Live: view.live, SettledSince: view.since,
	})
	if err != nil {
		return store.ThreadAgentState{}, fmt.Errorf("read whether thread %s has open work: %w", threadID, err)
	}
	return state, nil
}

// waitingLine says what a thread not at rest waits on, for an agent reading
// thread_status: the row's activity line, or what a waiting thread with no
// line waits on. Empty while it runs or is blocked, which its state says.
func (rest workerRest) waitingLine() string {
	switch {
	case rest.unrest == workerQueued:
		return "messages queued for it"
	case rest.unrest != workerWaiting:
		return ""
	case rest.detail != "":
		return rest.detail
	case rest.below:
		return "threads it started"
	default:
		return "a scheduled wakeup of its own"
	}
}

// workerUnrest says why threadID is not at rest, the zero value when it
// is: its own sources read now, the store's first (ownRest).
func (a *App) workerUnrest(threadID string) (workerRest, error) {
	src, err := a.store.ReadThreadRestSources(threadID)
	if err != nil {
		return workerRest{}, err
	}
	return a.ownRest(threadID, src), nil
}

// ownRest is the one answer to why threadID is not at rest by its own
// state, the zero value when it is, from every source in the order a row
// shows them: what triage holds (workerHeldUnrest), then a turn whose row
// is open (src.TurnOpen) as running; what it waits on (threadWaits: src,
// what the store holds, with what memory holds of its background work);
// the provider's timers (workerTimerUnrest). A source added later is one
// more case in one of them, and the event that changes it one more call
// of noteWorkerLiveness or noteWorkerWaiting, and for one memory holds, of
// noteThreadRestChange.
//
// The store is read before triage, as reassessWorker reads them: an answer
// on its way stops being one when the turn it starts echoes it, so a read
// that finds it delivered then finds that turn, or that turn's end.
func (a *App) ownRest(threadID string, src store.ThreadRestSources) workerRest {
	waits, _ := a.threadWaits(threadID, src)
	if unrest := a.workerHeldUnrest(threadID); unrest != "" {
		return workerRest{unrest: unrest}
	}
	if src.TurnOpen {
		return workerRest{unrest: workerRunning}
	}
	if waits.unrest != "" {
		return waits
	}
	return a.workerTimerUnrest(threadID)
}

// workerHeldUnrest is why threadID is not at rest from what the app holds
// in memory, no store read (threadWaitingOn): waiting on a prompt or an
// async question, in a turn, or holding messages queued for one.
func (a *App) workerHeldUnrest(threadID string) string {
	live := a.threadWaitingOn(threadID).live
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

// workerWaits is threadWaits over threadID's sources read now.
func (a *App) workerWaits(threadID string) (workerRest, bool, error) {
	src, err := a.store.ReadThreadRestSources(threadID)
	if err != nil {
		return workerRest{}, false, err
	}
	waits, background := a.threadWaits(threadID, src)
	return waits, background, nil
}

// threadWaits is restWaits over src and what memory holds of threadID now.
func (a *App) threadWaits(threadID string, src store.ThreadRestSources) (workerRest, bool) {
	return restWaits(src, a.memoryWaits(threadID), a.remoteComputerNames)
}

// memoryWaits is what memory holds of a thread that it waits on: Codex's
// background terminals and subagents, which triage holds and whose frames
// bump the thread's revision (noteBackgroundTrayLiveness).
type memoryWaits struct {
	background       bool
	backgroundDetail string
}

func (a *App) memoryWaits(threadID string) memoryWaits {
	var mem memoryWaits
	if a.triage != nil {
		live := a.triage.ListLiveCodexBackgroundTasks(threadID, 0, 0)
		if len(live) == 0 {
			live = a.triage.ListLiveCodexAgentTasks(threadID)
		}
		if len(live) > 0 {
			mem.background = true
			if mem.backgroundDetail = live[0].Summary; mem.backgroundDetail == "" {
				mem.backgroundDetail = live[0].ToolName
			}
		}
	}
	return mem
}

// restWaits is the one rule for whether a thread waits on something that
// starts a turn there without a person, read from what the store holds of
// it (src) and what memory does (mem), no read of its own: a question it
// asked that nobody answered yet (blocked, as its prompt reads); an answer
// to a request it made on its way to it (queued, store.ThreadAnswerDue);
// or, waiting, running background work (Claude tasks and background agents
// and remote commands, which the store holds; Codex subagents and
// terminals, which memory does), a workflow run bound to it that runs or
// owes it its report (the store's), an armed loop or watch, or a reminder that has
// not fired. The detail is the background work's summary, a remote
// command's computer (named by names, when given) and label, the run's
// goal, or the loop, watch or reminder named as its wake rows name it
// (wakeWaitingLine); background says the wait is background work. A
// write's own check applies it to the sources its transaction read, with
// nothing from memory (restDecision.stillAtRest).
func restWaits(src store.ThreadRestSources, mem memoryWaits, names func() map[string]string) (workerRest, bool) {
	switch {
	case src.Asking:
		return workerRest{unrest: workerBlocked}, false
	case src.AnswerDue:
		return workerRest{unrest: workerQueued}, false
	case src.Background && (src.BackgroundComputer == "" || !mem.background):
		detail := src.BackgroundDetail
		if src.BackgroundComputer != "" && names != nil {
			if name := names()[src.BackgroundComputer]; name != "" {
				detail = name + " · " + detail
			}
		}
		return workerRest{unrest: workerWaiting, detail: detail}, true
	case mem.background:
		return workerRest{unrest: workerWaiting, detail: mem.backgroundDetail}, true
	case src.Workflow:
		return workerRest{unrest: workerWaiting, detail: workflowWaitingLine(src.WorkflowGoal)}, false
	case src.Wake:
		return workerRest{unrest: workerWaiting, detail: wakeWaitingLine(src.WakeKind, src.WakeLabel)}, false
	case src.Reminder:
		return workerRest{unrest: workerWaiting, detail: wakeWaitingLine(threadWakeKindReminder, reminderLabel(src.ReminderNote))}, false
	}
	return workerRest{}, false
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

// workflowWaitingLine names a workflow run a worker waits on by its goal.
func workflowWaitingLine(goal string) string {
	if goal = clipLine(goal); goal == "" {
		return "Workflow"
	}
	return "Workflow · " + goal
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
//   - the spawn link is recorded, or up to two searches for a thread not
//     recorded, or recorded with none and read with durable (workerLinkOf);
//   - with durable, for a spawned thread only, one read of its sources
//     (workerWaits).
//
// It reports whether the thread is a worker, one a spawn made, now
// or as last recorded, so a change on either side is framed. A failed read
// leaves the record as it was, so the next change reads again, and is
// returned with worker true.
func (a *App) reassessWorker(threadID string, durable bool) (bool, error) {
	if a.store == nil {
		// No store, no spawns: nothing here is a worker.
		return false, nil
	}
	w := &a.liveWorkers
	if hook := a.beforeWorkerReassessForTest; hook != nil {
		hook(threadID)
	}
	w.reassess.Lock()
	defer w.reassess.Unlock()
	w.mu.Lock()
	before, known := w.threads[threadID]
	w.mu.Unlock()
	next := liveWorker{link: before.link, waits: before.waits, background: before.background}
	// A thread recorded with no link may have been recorded before the
	// spawn that names it did, as a fork with a question is.
	if !known || (durable && before.link == "") {
		link, err := a.workerLinkOf(threadID)
		if err != nil {
			return true, err
		}
		next.link = link
	}
	if durable && next.link != "" {
		waits, background, err := a.workerWaits(threadID)
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

// workerLinkOf is the spawn that makes threadID a worker: its local spawn
// link or, for a thread a paired computer's spawn made here, that spawn
// (peerSpawnLink), so this computer follows it as a worker for that
// computer's rows (answerThreadAgentRows). Empty for any other thread.
func (a *App) workerLinkOf(threadID string) (string, error) {
	link, found, err := a.store.SpawnLinkOf(threadID)
	if err != nil || found {
		return link, err
	}
	token, found, err := a.store.PeerSpawnReceiptOf(threadID)
	if err != nil || !found {
		return "", err
	}
	return peerSpawnLink(token), nil
}

// peerSpawnLink is the link of a thread a paired computer's spawn made
// here. No local request carries it, so no row here reads that thread's
// record as its worker's (liveWorkerUnrest).
func peerSpawnLink(token string) string { return "peer:" + token }

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
		link, err := a.workerLinkOf(id)
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

// liveThreadRest is why threadID is not at rest by its own state, as last
// recorded, whichever spawn made it.
func (a *App) liveThreadRest(threadID string) workerRest {
	w := &a.liveWorkers
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threads[threadID].workerRest
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
