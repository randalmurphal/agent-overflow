package app

import (
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
//   - nothing scheduled for them comes due: the store's due reads leave them
//     out (store.PauseAssistants), which is where every loop, watch and
//     reminder fire is decided;
//   - no session starts for them: an app-written message for one waits in
//     its queue (flushStartFenced, startAgentNoticeSession), any other start
//     is refused (startSession), and they admit no new work (admitWork);
//   - a worker's question goes to the user instead (store.SpawnerOf).
//
// Turning assistants off also brings what they were doing to a stop: their
// watch scripts and processes end with their rows armed again, a request
// open on one settles as a Stop of it does, and a running session ends with
// its turn cut and the messages queued behind it back in the composer.
// Turning them on starts the long-running watches again, lets the sweep
// serve every fire that came due meanwhile once with its count, and starts
// the session of a thread with messages waiting.

// assistantsOffMessage is what a person or an agent is told when something
// it asked for needs assistants.
const assistantsOffMessage = "Assistants are turned off in Settings on this computer."

// errAssistantsOff refuses work that needs assistants while they are off.
var errAssistantsOff = errorsx.Public(threadtools.CodeUnsupported, assistantsOffMessage, nil)

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
	mu sync.Mutex
	// paused is what the last convergence applied, set at boot from the
	// setting before anything runs.
	paused bool
	// convergedForTest, when set, runs after each convergence.
	convergedForTest func(paused bool)
}

// initAssistantsPause sets the pause from the setting at boot, once the
// settings tiers are attached and before the sweep's first pass: a paused
// assistant's loops are not recovered and nothing of it starts.
func (a *App) initAssistantsPause(st *store.Store) {
	paused := !a.currentSettings().AssistantsEnabled
	st.PauseAssistants(paused)
	a.assistantsSwitch.mu.Lock()
	a.assistantsSwitch.paused = paused
	a.assistantsSwitch.mu.Unlock()
}

// onAssistantsSettingChanged is the settings observer's hook. The pause of
// the due reads follows the setting at once; the rest converges off the
// observer's goroutine, since ending sessions takes a while.
func (a *App) onAssistantsSettingChanged() {
	if a.store == nil {
		return
	}
	a.store.PauseAssistants(!a.currentSettings().AssistantsEnabled)
	a.nudgeThreadRequestSweep()
	if !a.threadRequestsWG.Go(a.convergeAssistants) {
		log.Printf("assistants: shutting down; the setting applies at the next start")
	}
}

// convergeAssistants brings this computer to the setting as it reads now,
// and again while a change landed during a convergence, so toggling twice
// quickly ends in the state the setting ends in.
func (a *App) convergeAssistants() {
	a.assistantsSwitch.mu.Lock()
	defer a.assistantsSwitch.mu.Unlock()
	for {
		paused := !a.currentSettings().AssistantsEnabled
		a.store.PauseAssistants(paused)
		if paused == a.assistantsSwitch.paused {
			return
		}
		if paused {
			a.pauseAssistants()
		} else {
			a.resumeAssistants()
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

// resumeAssistants lets this computer's assistants run again: their
// long-running watches start, a run a quit cut while they were off runs
// again, and the sweep serves every interval fire that came due meanwhile
// once, with how many it missed. A thread with messages waiting in its
// queue starts its session for them.
func (a *App) resumeAssistants() {
	rows, err := a.store.ListLiveAssistantAgentWakes()
	if err != nil {
		log.Printf("assistants: turning them on: %v", err)
	} else {
		a.recoverAgentWakes(rows, false)
	}
	a.nudgeThreadRequestSweep()
	threads, err := a.store.ListThreadsUnderAssistants()
	if err != nil {
		log.Printf("assistants: turning them on: %v", err)
		return
	}
	for _, threadID := range threads {
		a.startWaitingAssistantSession(threadID)
	}
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
