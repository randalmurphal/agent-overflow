package app

import (
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/engine"
)

// threadAgentEmits coalesces what changed into frames, off the goroutines
// that reported it: a store write, an emit, a wait.
type threadAgentEmits struct {
	mu sync.Mutex
	threadAgentChanges
	retryAt  time.Time
	draining bool
	// activity is each running target's latest activity line, while it has
	// a request running or is a worker not at rest; lastActivity and
	// activityDue throttle the frames activity alone emits.
	activity     map[string]string
	lastActivity map[string]time.Time
	activityDue  map[string]bool
	// lastCall is the id of each target's latest tool call, so a new call
	// frames its counts whatever its line says.
	lastCall map[string]string
}

// noteThreadRequestChanged is the store's change hook, called once the
// write committed.
func (a *App) projectPeerRows(thread string) {
	a.queueThreadAgentEmit(func(e *threadAgentEmits) {
		if e.projections == nil {
			e.projections = map[string]struct{}{}
		}
		e.projections[thread] = struct{}{}
	})
}

func (a *App) noteThreadRequestChanged(change store.ThreadRequestChange) {
	if change.Token == "" {
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.callers[change.CallerThreadID] = struct{}{} })
		return
	}
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.tokens[change.Token] = struct{}{} })
}

// noteThreadAgentTarget reports a change to a thread some request runs on,
// or a worker not at rest, that leaves its rest as it was: its title.
func (a *App) noteThreadAgentTarget(threadID string) {
	if threadID == "" || (!a.threadAgentFollows(threadID) && !a.ownerActivity.watching()) {
		return
	}
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.targets[threadID] = struct{}{} })
}

// noteWorkerLiveness reports a change to a source triage holds that may
// move a thread into or out of rest: its turn, its prompts and its
// session's death, from the emit tap (tapThreadAgentRows); its provider
// timers, from the provider event handler (noteProviderWorkerLiveness) and
// the process's end (unregisterSession); and its queue, from triage's
// sends-pending observer (runSendsPendingWorker), which every queue write
// reports to. Each is memory, so a decision on rest under way is told
// (noteThreadRestChange). The thread is reassessed off these paths, where
// the spawn link's lookup runs; its durable sources keep what they last
// said.
func (a *App) noteWorkerLiveness(threadID string) {
	if threadID == "" {
		return
	}
	a.noteThreadRestChange(threadID)
	a.queueThreadAgentEmit(func(e *threadAgentEmits) {
		if _, queued := e.liveness[threadID]; !queued {
			e.liveness[threadID] = false
		}
	})
}

// noteWorkerWaiting reports a change to a thread's durable sources, which
// its reassessment reads again (workerWaits): its background work,
// from the background tray's frames, which also report the work a
// session's end settles; its loops,
// watches and reminders, from their frames; and every source, at boot.
func (a *App) noteWorkerWaiting(threadID string) {
	if threadID == "" {
		return
	}
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.liveness[threadID] = true })
}

// noteWorkflowRunChanged reports a workflow run's state or owed report
// changed: the thread it reports to may have moved into or out of rest.
func (a *App) noteWorkflowRunChanged(itemID string) {
	a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.workflowRuns[itemID] = struct{}{} })
}

// noteWorkflowOriginChange reports a workflow run's binding moved: a run
// that runs or owes a report keeps the thread it reports to from rest, so
// the thread it left and the one it reports to now are both read again.
func (a *App) noteWorkflowOriginChange(change store.WorkItemOriginChange) {
	for _, threadID := range change.Threads() {
		a.noteWorkerWaiting(threadID)
	}
}

// noteBackgroundTrayLiveness reports a background tray frame of a thread
// to its reassessment, unless it can change nothing: a delta whose named
// launches all still run, for a thread already waiting on its background
// work. A delta of a running background agent's latest tool is such a
// frame, so a working agent costs its worker no reads. A decision on rest
// under way is told of every frame (noteThreadRestChange): Codex's
// background work is memory, and a launch that starts as the recorded work
// ends is new work.
func (a *App) noteBackgroundTrayLiveness(event triage.BackgroundTrayEvent) {
	a.noteThreadRestChange(event.ThreadID)
	if !event.Refresh && len(event.Rows) == len(event.LaunchIDs) && a.waitsOnBackground(event.ThreadID) {
		running := true
		for _, row := range event.Rows {
			running = running && row.Status == "running"
		}
		if running {
			return
		}
	}
	a.noteWorkerWaiting(event.ThreadID)
}

// threadAgentFollows reports whether a thread's changes reach an Agents
// row: a request runs on it, or it is a worker not at rest.
func (a *App) threadAgentFollows(threadID string) bool {
	if _, running := a.runningReceiptTokens(threadID); running {
		return true
	}
	return a.liveWorker(threadID)
}

func (e *threadAgentEmits) nextRetry() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.retryAt
}

func (a *App) retryThreadAgentEmits(now time.Time) {
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.retryAt.IsZero() || now.Before(e.retryAt) {
		e.mu.Unlock()
		return
	}
	a.startThreadAgentEmitsLocked()
}

func (a *App) queueThreadAgentEmit(add func(*threadAgentEmits)) {
	if a.store == nil {
		return
	}
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.tokens == nil {
		e.threadAgentChanges = newThreadAgentChanges()
	}
	add(e)
	a.startThreadAgentEmitsLocked()
}

// startThreadAgentEmitsLocked releases the queue lock before starting its worker.
func (a *App) startThreadAgentEmitsLocked() {
	e := &a.threadAgentEmits
	if e.draining || a.lifeCtx().Err() != nil {
		e.mu.Unlock()
		return
	}
	e.draining = true
	e.mu.Unlock()
	if !a.threadRequestsWG.Go(a.drainThreadAgentEmits) {
		e.mu.Lock()
		e.draining = false
		e.mu.Unlock()
	}
}

func (a *App) drainThreadAgentEmits() {
	e := &a.threadAgentEmits
	for {
		if hook := a.threadAgentDrainForTest; hook != nil {
			hook()
		}
		e.mu.Lock()
		batch := e.threadAgentChanges
		if len(batch.tokens)+len(batch.callers)+len(batch.targets)+len(batch.liveness)+len(batch.workflowRuns)+len(batch.projections) == 0 || a.lifeCtx().Err() != nil {
			e.draining = false
			e.mu.Unlock()
			return
		}
		e.threadAgentChanges = newThreadAgentChanges()
		e.retryAt = time.Time{}
		e.mu.Unlock()
		if err := a.publishThreadAgentChanges(batch); err != nil {
			log.Printf("agent rows: projection queued for retry: %v", err)
			e.mu.Lock()
			e.retain(batch)
			e.retryAt = time.Now().Add(restRecoveryRetry)
			e.draining = false
			e.mu.Unlock()
			a.nudgeThreadRequestSweep()
			return
		}
	}
}

// tapThreadAgentRows is the emit tap: any thread's turn, prompt or session
// changed, which may move it into or out of rest (noteWorkerLiveness), or
// its background work, loops, watches or reminders did
// (noteWorkerWaiting); a running target's or a live worker's title
// changed, or it did something new.
func (a *App) tapThreadAgentRows(name eventchan.Channel, data any) {
	// A thread row frame is told by its payload, so this reader names no
	// thread:updated channel: only the announcer emits on it.
	if event, ok := data.(triage.ThreadUpdateEvent); ok {
		if event.Thread != nil {
			a.noteThreadAgentTarget(event.Thread.ID)
		} else if event.Title != nil || event.Action == triage.ThreadActionDeleted {
			a.noteThreadAgentTarget(event.ID)
		}
		return
	}
	switch name {
	case eventchan.ProviderTurnStarted:
		if event, ok := data.(triage.TurnStartedEvent); ok {
			a.noteWorkerLiveness(event.ThreadID)
		}
	case eventchan.ProviderTurnCompleted:
		if event, ok := data.(triage.TurnCompletedEvent); ok {
			a.forgetThreadAgentActivity(event.ThreadID)
			a.noteWorkerLiveness(event.ThreadID)
		}
	case eventchan.ProviderTurnEndUnrecorded:
		// Whether the thread's open turn row is a running turn changed
		// (turnRuns), with no turn frame when a failed boot sweep sets it
		// or a retired session's turn start writes a row and opens no round.
		if event, ok := data.(triage.TurnEndUnrecordedEvent); ok {
			a.noteWorkerLiveness(event.ThreadID)
		}
	case eventchan.ProviderApproval:
		if event, ok := data.(provider.ApprovalEvent); ok {
			threadID := event.ThreadID
			if threadID == "" && event.Request != nil {
				threadID = event.Request.ThreadID
			}
			a.noteWorkerLiveness(threadID)
		}
	case eventchan.ProviderUserInput:
		if event, ok := data.(provider.UserInputEvent); ok {
			threadID := event.ThreadID
			if threadID == "" && event.Request != nil {
				threadID = event.Request.ThreadID
			}
			a.noteWorkerLiveness(threadID)
		}
	case eventchan.ProviderSessionDied:
		if event, ok := data.(triage.SessionDiedEvent); ok {
			a.forgetThreadAgentActivity(event.ThreadID)
			a.noteWorkerLiveness(event.ThreadID)
		}
	case eventchan.ProviderBackgroundTray:
		if event, ok := data.(triage.BackgroundTrayEvent); ok {
			a.noteBackgroundTrayLiveness(event)
		}
	case eventchan.ThreadWakesChanged:
		if change, ok := data.(ThreadWakeChange); ok {
			a.noteWorkerWaiting(change.ThreadID)
		}
	case eventchan.ProviderAsyncQuestionsChanged:
		if change, ok := data.(map[string]string); ok {
			a.noteWorkerWaiting(change["threadId"])
		}
	case eventchan.WorkflowItemState:
		if event, ok := data.(engine.StateEvent); ok && event.ItemID != "" {
			a.noteWorkflowRunChanged(event.ItemID)
		}
	case eventchan.ProviderUsage:
		if event, ok := data.(provider.UsageEvent); ok && (event.Action == "usage" || event.Action == "progress") {
			a.noteThreadAgentCounts(event.ThreadID)
		}
	case eventchan.ProviderItemEvent:
		if event, ok := data.(triage.ItemStreamEvent); ok && event.Item != nil && event.Item.ParentID == "" {
			a.noteThreadAgentActivity(event.ThreadID, *event.Item)
		}
	}
}

// noteThreadAgentActivity records a running target's latest top-level tool
// call as its activity line, as a subagent's row shows its latest tool, and
// frames it at most every threadAgentActivityEvery. A new call frames the
// row even when its line reads as the last one did, since its tool count
// moved; the same call's own updates frame nothing unless its line
// changes. Its messages are not activity: its last one is the row's digest.
func (a *App) noteThreadAgentActivity(threadID string, item store.Item) {
	if item.Kind != "tool_call" || threadID == "" {
		return
	}
	// Every top-level row of every thread passes here: only a thread a
	// request runs on, or a worker not at rest, pays for the line.
	if !a.threadAgentFollows(threadID) {
		return
	}
	line := clipLine(item.Summary)
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.activity == nil {
		e.activity, e.lastActivity, e.activityDue = map[string]string{}, map[string]time.Time{}, map[string]bool{}
		e.lastCall = map[string]string{}
	}
	sameCall := e.lastCall[threadID] == item.ID
	if (line == "" || e.activity[threadID] == line) && sameCall {
		e.mu.Unlock()
		return
	}
	e.lastCall[threadID] = item.ID
	if line != "" {
		e.activity[threadID] = line
	}
	a.frameThreadAgentProgressLocked(threadID)
}

// noteThreadAgentCounts frames a followed thread's row as its spend moves,
// at most every threadAgentActivityEvery.
func (a *App) noteThreadAgentCounts(threadID string) {
	if threadID == "" || !a.threadAgentFollows(threadID) {
		return
	}
	e := &a.threadAgentEmits
	e.mu.Lock()
	if e.activity == nil {
		e.activity, e.lastActivity, e.activityDue = map[string]string{}, map[string]time.Time{}, map[string]bool{}
		e.lastCall = map[string]string{}
	}
	a.frameThreadAgentProgressLocked(threadID)
}

// frameThreadAgentProgressLocked frames threadID as a target now, or once
// threadAgentActivityEvery has passed since its last such frame, one frame
// pending at a time. Caller holds e.mu, which this releases.
func (a *App) frameThreadAgentProgressLocked(threadID string) {
	e := &a.threadAgentEmits
	if e.activityDue[threadID] {
		e.mu.Unlock()
		return
	}
	wait := threadAgentActivityEvery - time.Since(e.lastActivity[threadID])
	e.activityDue[threadID] = true
	e.mu.Unlock()
	emit := func() {
		e.mu.Lock()
		delete(e.activityDue, threadID)
		e.lastActivity[threadID] = time.Now()
		e.mu.Unlock()
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.targets[threadID] = struct{}{} })
	}
	if wait <= 0 {
		emit()
		return
	}
	time.AfterFunc(wait, emit)
}

// threadAgentActivity is a target's latest activity line, while it runs.
func (a *App) threadAgentActivity(threadID string) string {
	e := &a.threadAgentEmits
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activity[threadID]
}

// forgetThreadAgentActivity drops a target's line once its turn ends.
func (a *App) forgetThreadAgentActivity(threadID string) {
	e := &a.threadAgentEmits
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.activity, threadID)
	delete(e.lastActivity, threadID)
	delete(e.lastCall, threadID)
}

// noteThreadAgentWait reports a call threadID parked on, or that left,
// requests: a send or ask is an agent while its caller waits on it, and
// open work of that caller, which a decision on rest under way is told of.
func (a *App) noteThreadAgentWait(threadID string, keys []string) {
	a.noteThreadRestChange(threadID)
	for _, key := range keys {
		if key == "" || strings.HasPrefix(key, threadWatchKey("")) {
			continue
		}
		a.queueThreadAgentEmit(func(e *threadAgentEmits) { e.tokens[key] = struct{}{} })
	}
}
