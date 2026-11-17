package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Turning assistants off and on (docs/specs/standing-agents.md, "Turning
// assistants off"). Off is a pause, never a cancel: the threads that live
// under an assistant (its current thread, its past threads and its helpers)
// keep every row they have, and while it lasts:
//
//   - nothing scheduled for them runs: a loop or watch fire, a Wake now and
//     a long-running watch's process start are refused where the row is
//     claimed (fireAgentWake, admitAgentWatchProcess), a reminder where it
//     settles (settleThreadReminder), and the store's due reads leave them
//     out so the sweep neither selects nor wakes for them;
//   - no session starts for them: an app-written message for one waits in
//     its queue (flushStartFenced, startAgentNoticeSession), any other start
//     is refused (startSession), and they admit no new work (admitWork);
//   - a person's send, steer, edit and resend, fork, transfer, shell command
//     or workflow binding is refused before it changes anything
//     (admitPersonOperation); a message queued in the composer waits;
//   - a worker's question goes to the user instead (store.SpawnerOf).
//
// Turning assistants off also brings what they were doing to a stop: their
// watch scripts and processes end with their rows armed again, a request
// open on one settles as a Stop of it does, and a running session ends with
// its turn cut and the messages queued behind it back in the composer.
// Turning them on recovers what a quit left running while the pause still
// holds, then releases it: the long-running watches start again, the sweep
// serves every fire that came due meanwhile once with its count, and a
// thread with messages waiting starts its session.

// assistantsOffMessage is what a person or an agent is told when something
// it asked for needs assistants.
const assistantsOffMessage = "Assistants are turned off in Settings on this computer."

// errAssistantsOff refuses work that needs assistants while they are off.
var errAssistantsOff = errorsx.Public(threadtools.CodeUnsupported, assistantsOffMessage, nil)

// assistantsOffFor is errAssistantsOff as the thread tools' caller reads
// it: a caller on a paired computer is told which computer has them off.
func (a *App) assistantsOffFor(ctx context.Context) error {
	if threadtools.Forwarded(ctx) {
		return errorsx.Public(threadtools.CodeUnsupported, "Assistants are turned off in Settings on "+a.backendDisplayName()+".", nil)
	}
	return errAssistantsOff
}

// agentWakeOutcomePaused is the outcome of a run the pause stopped.
const agentWakeOutcomePaused = "stopped: assistants were turned off; runs again when they are turned on"

// assistantsOffStop is how turning assistants off settles a request open on
// an assistant's thread and cuts its turn.
func assistantsOffStop() restStop {
	return restStop{
		turn: turnStop{
			label: "Stopped: assistants were turned off",
			cause: "Assistants were turned off in Settings before it answered.",
		},
		answer: "Assistants were turned off in Settings before it answered; nobody cancelled it.",
	}
}

// assistantsSwitchState serializes the convergences on the setting.
type assistantsSwitchState struct {
	// pauseMu orders the writes of the store's pause against reads of the
	// setting, so the pause is never cleared after the setting turned off.
	pauseMu sync.Mutex
	mu      sync.Mutex
	// paused is what the last convergence applied, set at boot from the
	// setting before anything runs.
	paused bool
	// convergedForTest, when set, runs after each convergence.
	convergedForTest func(paused bool)
	// resumeReadForTest, when set, runs as turning assistants on reads
	// their loops and watches: before the read with read false, and after
	// it with read true.
	resumeReadForTest func(read bool)
}

// initAssistantsPause sets the pause from the setting at boot, once the
// settings tiers are attached and before the sweep's first pass: a paused
// assistant's loops are not recovered and nothing of it starts.
func (a *App) initAssistantsPause(st *store.Store) {
	paused := !a.currentSettings().AssistantsEnabled
	st.PauseAssistants(paused)
	st.HideAssistants(paused)
	a.assistantsSwitch.mu.Lock()
	a.assistantsSwitch.paused = paused
	a.assistantsSwitch.mu.Unlock()
}

// onAssistantsSettingChanged is the settings observer's hook. Off takes
// effect at once: every admission refuses from here, before the convergence
// stops what runs. On waits for the convergence, which clears the pause only
// once what the pause left is recovered. The convergence runs off the
// observer's goroutine, since ending sessions takes a while.
func (a *App) onAssistantsSettingChanged() {
	if a.store == nil {
		return
	}
	a.holdAssistantsPause()
	if !a.threadRequestsWG.Go(a.convergeAssistants) {
		log.Printf("assistants: shutting down; the setting applies at the next start")
	}
}

// holdAssistantsPause sets the pause if the setting is off.
func (a *App) holdAssistantsPause() {
	a.assistantsSwitch.pauseMu.Lock()
	defer a.assistantsSwitch.pauseMu.Unlock()
	if !a.currentSettings().AssistantsEnabled {
		a.store.PauseAssistants(true)
	}
}

// releaseAssistantsPause clears the pause unless the setting turned off
// meanwhile, and reports whether it did.
func (a *App) releaseAssistantsPause() bool {
	a.assistantsSwitch.pauseMu.Lock()
	defer a.assistantsSwitch.pauseMu.Unlock()
	if !a.currentSettings().AssistantsEnabled {
		return false
	}
	a.store.PauseAssistants(false)
	return true
}

// convergeAssistants brings this computer to the setting as it reads now,
// and again while a change landed during a convergence, so toggling quickly
// ends in the state the setting ends in. A pause the observer set for an off
// this convergence never saw is resumed like any other: an admission it
// refused meanwhile left its row for the resume to start.
func (a *App) convergeAssistants() {
	a.assistantsSwitch.mu.Lock()
	defer a.assistantsSwitch.mu.Unlock()
	for {
		var paused bool
		switch {
		case !a.currentSettings().AssistantsEnabled:
			a.holdAssistantsPause()
			if a.assistantsSwitch.paused {
				return
			}
			a.pauseAssistants()
			paused = true
		case a.assistantsSwitch.paused || a.store.AssistantsPaused():
			if !a.resumeAssistants() {
				// Turned off again before the pause was released.
				continue
			}
		default:
			return
		}
		a.assistantsSwitch.paused = paused
		if hook := a.assistantsSwitch.convergedForTest; hook != nil {
			hook(paused)
		}
	}
}

// assistantsPaused reports whether assistants are off on this computer.
func (a *App) assistantsPaused() bool {
	return a.store != nil && a.store.AssistantsPaused()
}

// pausedAssistantThread reports whether threadID lives under an assistant
// while assistants are off. An error names the read.
func (a *App) pausedAssistantThread(threadID string) (bool, error) {
	if !a.assistantsPaused() {
		return false, nil
	}
	return a.store.ThreadUnderAssistant(threadID)
}

// refusePausedAssistantThread refuses work for a thread that lives under an
// assistant while assistants are off.
func (a *App) refusePausedAssistantThread(threadID string) error {
	paused, err := a.pausedAssistantThread(threadID)
	if err != nil {
		return err
	}
	if paused {
		return errAssistantsOff
	}
	return nil
}

// personAdmissionKey marks a context whose operation admitPersonOperation
// admitted on a thread.
type personAdmissionKey struct{ threadID string }

// admitPersonOperation is the admission of a person's operation that drives
// a thread or changes its conversation: a send, a steer, an edit and resend,
// a fork, a transfer, a shell command, a workflow binding. It refuses a
// paused assistant's thread with the off message. Call it under the thread's
// action lock before anything is staged or cut. The context it returns
// carries the admission, so a session start the operation makes later is
// not refused by a pause that landed meanwhile: the pause stops that session
// when it takes the action lock after the operation.
func (a *App) admitPersonOperation(ctx context.Context, threadID string) (context.Context, error) {
	if personAdmitted(ctx, threadID) {
		return ctx, nil
	}
	if err := a.refusePausedAssistantThread(threadID); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, personAdmissionKey{threadID}, true), nil
}

// personAdmitted reports whether ctx carries admitPersonOperation's
// admission for threadID.
func personAdmitted(ctx context.Context, threadID string) bool {
	admitted, _ := ctx.Value(personAdmissionKey{threadID}).(bool)
	return admitted
}

// refuseAssistantsOff refuses creating or replacing an assistant while
// assistants are off.
func (a *App) refuseAssistantsOff() error {
	if a.assistantsPaused() {
		return errAssistantsOff
	}
	return nil
}

// pauseAssistants stops what this computer's assistants are doing.
func (a *App) pauseAssistants() {
	threads, err := a.store.ListThreadsUnderAssistants()
	if err != nil {
		log.Printf("assistants: turning them off: %v", err)
		return
	}
	for _, threadID := range threads {
		if failures := a.pauseAssistantThread(threadID); len(failures) > 0 {
			log.Printf("assistants: turning off %s: %v", threadID, failures)
			a.emitErrorToThread(threadID, "Assistants were turned off, but this thread could not be stopped completely: "+strings.Join(failures, "; "))
		}
	}
}

// pauseAssistantThread stops one thread under an assistant. Its admission is
// closed while it runs, so the work already being admitted is recorded
// before it is stopped and anything admitted later sees the pause.
func (a *App) pauseAssistantThread(threadID string) []string {
	reopen := a.resting.close(threadID)
	defer reopen()
	result := &ThreadAgentStop{}
	if err := a.pauseAgentWakeRuns(threadID); err != nil {
		result.Failures = append(result.Failures, "stop its loops and watches: "+err.Error())
	}
	a.stopRequestsOnRestingThread(a.lifeCtx(), threadID, assistantsOffStop(), result)
	if err := a.endPausedAssistantSession(threadID); err != nil {
		result.Failures = append(result.Failures, "end its session: "+err.Error())
	}
	// A prompt its workers routed to it goes to the user now.
	a.reroutePromptsFrom(threadID)
	return result.Failures
}

// pauseAgentWakeRuns stops a thread's watch scripts and processes and its
// loops between fire and delivery, waits a bounded time for each to exit,
// and arms their rows again with the pause's outcome, keeping their
// schedule (store.PauseAgentWakeRun).
func (a *App) pauseAgentWakeRuns(threadID string) error {
	a.agentWakes.lifecycle.Lock()
	a.agentWakes.mu.Lock()
	var ids []string
	for id, run := range a.agentWakes.runs {
		if run.threadID == threadID {
			ids = append(ids, id)
		}
	}
	a.agentWakes.mu.Unlock()
	stopped := make([]*agentWakeRun, 0, len(ids))
	for _, id := range ids {
		if run := a.stopAgentWakeRun(id); run != nil {
			stopped = append(stopped, run)
		}
	}
	a.agentWakes.lifecycle.Unlock()
	for _, run := range stopped {
		select {
		case <-run.done:
		case <-time.After(agentWakeStopWait):
			log.Printf("assistants: a loop or watch of %s did not stop within %s", threadID, agentWakeStopWait)
		}
	}
	rows, err := a.store.ListAgentWakes(threadID)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if row.State != store.AgentWakeRunning {
			continue
		}
		if _, err := a.store.PauseAgentWakeRun(row.ID, agentWakeOutcomePaused); err != nil {
			errs = append(errs, err)
			continue
		}
		a.emitAgentWakeChanged(threadID, row.ID)
	}
	return errors.Join(errs...)
}

// endPausedAssistantSession cuts the thread's turn with the pause's stop row
// and ends its session with everything it runs, under the action lock every
// send and start takes. The messages queued behind it go back to the
// composer, as when a session ends on its own, and an app-written one to
// its owner. A thread with no session keeps its queue: the messages wait
// for assistants to be turned on.
func (a *App) endPausedAssistantSession(threadID string) error {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if _, live := a.sessionManager().get(threadID); !live {
		return nil
	}
	var errs []error
	if a.triage != nil && a.triage.SampleInterrupt(threadID).CutTurn() >= 0 {
		if _, err := a.interruptTurnHoldingLock(threadID, anyOpenTurn, assistantsOffStop().turn, a.observeInterruptTarget(threadID)); err != nil {
			errs = append(errs, fmt.Errorf("interrupt its turn: %w", err))
		}
	}
	sess, _ := a.sessionManager().take(threadID)
	if err := a.teardownAndCloseSessionRestoringQueue(threadID, sess); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resumeAssistants lets this computer's assistants run again, and reports
// false, having started nothing, when the setting turned off before the pause
// was released. While the pause still holds, so nothing of theirs is
// admitted, a run a quit cut while they were off is recorded; then the pause
// is released, their long-running watches start, the cut runs run again, and
// the sweep serves every interval fire that came due meanwhile once, with
// how many it missed. A thread with messages waiting in its queue starts its
// session for them.
func (a *App) resumeAssistants() bool {
	if hook := a.assistantsSwitch.resumeReadForTest; hook != nil {
		hook(false)
	}
	rows, err := a.store.ListLiveAssistantAgentWakes()
	if hook := a.assistantsSwitch.resumeReadForTest; hook != nil {
		hook(true)
	}
	if err != nil {
		log.Printf("assistants: turning them on: %v", err)
	}
	recovered := a.recoverOrphanedAgentWakes(rows)
	if !a.releaseAssistantsPause() {
		return false
	}
	a.restartAgentWakes(recovered, false)
	a.nudgeThreadRequestSweep()
	threads, err := a.store.ListThreadsUnderAssistants()
	if err != nil {
		log.Printf("assistants: turning them on: %v", err)
		return true
	}
	for _, threadID := range threads {
		a.startWaitingAssistantSession(threadID)
	}
	return true
}

// startWaitingAssistantSession starts the session of a thread that has
// messages waiting in its queue, as the queue's own start would have.
func (a *App) startWaitingAssistantSession(threadID string) {
	queued, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		a.emitErrorToThread(threadID, "Assistants were turned on, but the app could not read the messages waiting for this thread: "+err.Error())
		return
	}
	if len(queued) == 0 {
		return
	}
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		return
	}
	defer unlock()
	if _, live := a.sessionManager().get(threadID); live {
		return
	}
	if held, err := a.flushStartFenced(threadID); err != nil || held {
		return
	}
	if err := a.startSession(a.lifeCtx(), threadID); err != nil && !errors.Is(err, ErrShuttingDown) {
		a.noteAgentNoticeStartFailed(threadID)
		a.emitErrorToThread(threadID, "Messages are waiting for this thread, but the agent could not start after assistants were turned on: "+err.Error())
	}
}
