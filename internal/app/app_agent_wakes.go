package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// thread_loop and thread_watch: a thread's own schedule
// (docs/specs/standing-agents.md, "Wake supervisor"). The rows live in
// `agent_wakes`; the thread request sweep fires them (app_agent_wake_runs.go)
// and delivers each wake through the queue reminders use
// (app_agent_wake_delivery.go).
//
// A wake's `running` state is the supervisor's own execution, never the
// agent's turn: a watch script in flight, a long-running watch's process
// alive, or a loop between its fire and its queued message.

const (
	// agentWakeFloor is the shortest interval a loop or watch may register.
	// A fire is a turn, so a shorter one spends the user's usage on a
	// clock; a harness boot lowers it (IsolationConfig.AgentWakeFloor).
	agentWakeFloor = threadtools.WakeFloorSeconds * time.Second
)

// agentWakeState is the supervisor's in-process state. Zero value ready.
type agentWakeState struct {
	mu sync.Mutex
	// floorOverride replaces agentWakeFloor on a harness boot.
	floorOverride time.Duration
	// runs is every run in flight by wake id: an interval run between its
	// claim and its finish, or a long-running watch's supervisor.
	runs map[string]*agentWakeRun
	// slots bounds the interval runs that execute at once. Built on first
	// use.
	slots chan struct{}
	// ticks marks the threads whose current turn only wakes started (see
	// noteAgentWakeDispatch).
	ticks map[string]bool
	// queued is, per thread, what Claude was handed while a turn ran, and
	// pickups the turn that took it up after that turn ended (see
	// noteAgentWakeQueued).
	queued  map[string]*agentWakeQueued
	pickups map[string]agentWakePickup
}

// agentWakeQueued is the rows Claude was handed while a turn ran.
type agentWakeQueued struct {
	// turn is the running turn the rows show in.
	turn int
	// ended is set when that turn ended with rows still unconfirmed: a row
	// confirmed since is taken up by the turn after it.
	ended bool
	// rows maps each row id to whether only wakes are in it.
	rows map[string]bool
}

// agentWakePickup is the turn that took up a thread's queued rows.
type agentWakePickup struct {
	turn int
	tick bool
}

// agentWakeRun is one run in flight.
type agentWakeRun struct {
	threadID string
	cancel   context.CancelFunc
	done     chan struct{}
	// stopped is set before cancel when the row was cancelled or replaced:
	// the run then writes nothing, because the row is no longer its own.
	// A cancel with stopped unset is the app shutting down.
	mu      sync.Mutex
	stopped bool
}

func (r *agentWakeRun) wasStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (a *App) agentWakeFloor() time.Duration {
	if a.agentWakes.floorOverride > 0 {
		return a.agentWakes.floorOverride
	}
	return agentWakeFloor
}

func newAgentWakeID() string { return entityid.New() }

// Loop registers or replaces one loop of the caller's thread.
func (t threadToolsApp) Loop(_ context.Context, caller threadtools.Caller, call threadtools.LoopCall) (threadtools.WakeState, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	}
	interval, err := t.app.checkAgentWakeInterval(call.EverySeconds)
	if err != nil {
		return threadtools.WakeState{}, err
	}
	row, err := t.app.registerAgentWake(store.AgentWake{
		ThreadID: caller.ThreadID, Kind: store.AgentWakeLoop, Label: call.Label, Note: call.Note,
		IntervalSeconds: int64(call.EverySeconds), NextAt: time.Now().Add(interval).UnixMilli(),
	})
	if err != nil {
		return threadtools.WakeState{}, err
	}
	return agentWakeToolState(row), nil
}

// Watch registers or replaces one watch of the caller's thread. Only an
// assistant's current thread may: a watch runs a script with the
// assistant's home, and no other thread has one.
func (t threadToolsApp) Watch(_ context.Context, caller threadtools.Caller, call threadtools.WatchCall) (threadtools.WakeState, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	}
	if _, standing, err := t.app.store.GetStandingAgent(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	} else if !standing {
		return threadtools.WakeState{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"thread_watch is for assistants, and this thread is not an assistant's current thread: a watch runs a script with the assistant's own home directory. Use thread_loop, which any thread may register, or ask the user to create an assistant for this work.", nil)
	}
	row := store.AgentWake{
		ThreadID: caller.ThreadID, Kind: store.AgentWakeWatch, Label: call.Label,
		Command: call.Command, Cwd: call.Cwd, LongRunning: call.LongRunning,
	}
	if !call.LongRunning {
		interval, err := t.app.checkAgentWakeInterval(call.EverySeconds)
		if err != nil {
			return threadtools.WakeState{}, err
		}
		switch {
		case call.TimeoutSeconds == 0:
			row.TimeoutSeconds = threadtools.DefaultWakeTimeoutSeconds
		case call.TimeoutSeconds < 1 || call.TimeoutSeconds > threadtools.MaxWakeTimeoutSeconds:
			return threadtools.WakeState{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
				"timeout_seconds must be between 1 and %d.", threadtools.MaxWakeTimeoutSeconds), nil)
		default:
			row.TimeoutSeconds = int64(call.TimeoutSeconds)
		}
		// The first run is one interval out, as a loop's first fire is, so
		// it never lands inside the turn that registered it.
		row.IntervalSeconds, row.NextAt = int64(call.EverySeconds), time.Now().Add(interval).UnixMilli()
	}
	stored, err := t.app.registerAgentWake(row)
	if err != nil {
		return threadtools.WakeState{}, err
	}
	return agentWakeToolState(stored), nil
}

// ListWakes lists the caller's live loops and watches.
func (t threadToolsApp) ListWakes(_ context.Context, caller threadtools.Caller) ([]threadtools.WakeState, error) {
	rows, err := t.app.store.ListAgentWakes(caller.ThreadID)
	if err != nil {
		return nil, err
	}
	out := make([]threadtools.WakeState, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentWakeToolState(row))
	}
	return out, nil
}

// CancelWake cancels the caller's live loop or watch with this label, stops
// what it has running and withdraws its wake still queued.
func (t threadToolsApp) CancelWake(_ context.Context, caller threadtools.Caller, label, kind string) (threadtools.WakeState, bool, error) {
	row, cancelled, err := t.app.cancelAgentWake(caller.ThreadID, label, kind)
	if err != nil || !cancelled {
		return threadtools.WakeState{}, false, err
	}
	return agentWakeToolState(row), true, nil
}

// cancelAgentWake cancels a thread's live loop or watch by label, stops what
// it has running and withdraws its wake still queued. It is the one cancel of
// a single row: thread_cancel and the activity rail's Cancel both take it.
// Not cancelled is no live row of that kind under that label. A wake that
// could not be taken back is an error, with the row already cancelled.
func (a *App) cancelAgentWake(threadID, label, kind string) (store.AgentWake, bool, error) {
	row, cancelled, err := a.store.CancelAgentWake(threadID, label, kind, agentWakeOutcomeCancelled)
	if err != nil || !cancelled {
		return store.AgentWake{}, false, err
	}
	a.emitAgentWakeChanged(threadID, row.ID)
	a.stopAgentWakeRun(row.ID)
	if err := a.withdrawCancelledAgentWakes(row.ThreadID, row.ID); err != nil {
		log.Printf("agent wakes: %v", err)
		return store.AgentWake{}, false, fmt.Errorf("the %s %q is cancelled, but its wake already queued could not be taken back and may still arrive", row.Kind, row.Label)
	}
	row.State, row.NextAt = store.AgentWakeCancelled, 0
	return row, true, nil
}

// checkAgentWakeInterval refuses an interval below the computer's floor.
func (a *App) checkAgentWakeInterval(seconds int) (time.Duration, error) {
	interval := time.Duration(seconds) * time.Second
	if floor := a.agentWakeFloor(); interval < floor {
		return 0, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"every_seconds must be at least %d on this computer: each fire is a turn.", int(floor/time.Second)), nil)
	}
	return interval, nil
}

// registerAgentWake writes one registration and brings the supervisor in
// line with it: a long-running watch's process is restarted with the new
// command, a row that stopped being one has its process stopped, and the
// sweep is woken for an interval row whose first fire may be sooner than its
// sleep.
func (a *App) registerAgentWake(row store.AgentWake) (store.AgentWake, error) {
	row.ID = newAgentWakeID()
	stored, err := a.store.RegisterAgentWake(row)
	switch {
	case errors.Is(err, store.ErrAgentWakeLabelTaken):
		return store.AgentWake{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"Another of this thread's loops or watches is already named %q. Choose another label, or stop that one first with thread_cancel label.", row.Label), err)
	case errors.Is(err, store.ErrAgentWakeLimit):
		return store.AgentWake{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"This thread already has %d loops and watches, the most one thread keeps. Stop one with thread_cancel label first.", store.MaxAgentWakesPerThread), err)
	case err != nil:
		return store.AgentWake{}, err
	}
	a.emitAgentWakeChanged(stored.ThreadID, stored.ID)
	if stored.Kind == store.AgentWakeWatch {
		a.stopLongRunningAgentWatch(stored.ID)
		if stored.LongRunning {
			a.startAgentWatchProcess(stored)
		}
	}
	a.nudgeThreadRequestSweep()
	return stored, nil
}

// agentWakeToolState is one row as thread_status lists it.
func agentWakeToolState(row store.AgentWake) threadtools.WakeState {
	return threadtools.WakeState{
		Kind: row.Kind, Label: row.Label, State: row.State, Note: row.Note,
		Command: row.Command, Cwd: row.Cwd, EverySeconds: row.IntervalSeconds,
		TimeoutSeconds: row.TimeoutSeconds, LongRunning: row.LongRunning, NextFireAt: row.NextAt,
		LastRunAt: row.LastRunAt, LastOutcome: row.LastOutcome, LastOutput: row.LastOutput,
		Undelivered: row.Undelivered, Missed: row.Missed,
	}
}

// cancelAgentWakesForThread cancels every loop and watch of a thread that is
// being archived, deleted or moved, and stops what they have running. Rows a
// delete has already cascaded away still have their runs stopped by thread.
// It may run under the thread's action lock, as a delete's does, so the
// callers that keep the thread withdraw its queued wakes themselves
// (withdrawCancelledAgentWakes); a deleted thread's queue goes with it.
func (a *App) cancelAgentWakesForThread(threadID, outcome string) {
	if ids, err := a.store.CancelAgentWakesForThread(threadID, "", outcome); err != nil {
		log.Printf("agent wakes: cancel the loops and watches of %s: %v", threadID, err)
	} else if len(ids) > 0 {
		a.emitAgentWakesRemoved(threadID, ids)
	}
	a.stopAgentWakeRunsForThread(threadID)
}

// noteAgentWakeDispatch records whether the turn a flush dispatch starts is a
// tick: one only loop and watch wakes started. It decides only whether that
// turn's start retracts a notification; whether its end is quiet is read
// from its rows when triage asks (agentTurnIsTick, through
// SetQuietTurnClassifier). A dispatch into a running turn
// starts nothing, but a person's message joining a tick makes the turn
// theirs. A new turn also ends any wait for a pickup: its own row says who
// it is for.
func (a *App) noteAgentWakeDispatch(threadID string, allWakes, newTurn bool) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if newTurn {
		delete(a.agentWakes.queued, threadID)
		delete(a.agentWakes.pickups, threadID)
	}
	switch {
	case newTurn && allWakes:
		if a.agentWakes.ticks == nil {
			a.agentWakes.ticks = make(map[string]bool)
		}
		a.agentWakes.ticks[threadID] = true
	case newTurn || !allWakes:
		delete(a.agentWakes.ticks, threadID)
	}
}

// noteAgentWakeQueued records a row Claude was handed while turn `turn` ran.
// The row shows in that turn either way, but Claude takes it in at one of
// two points: at the turn's next model call, so the turn answers it, or
// after the turn ends, in a turn of its own that holds no row. The echo
// tells them apart (agentWakeQueuedConfirmed): one that arrives before the
// turn ended is the first.
func (a *App) noteAgentWakeQueued(threadID, rowID string, turn int, allWakes bool) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	q := a.agentWakes.queued[threadID]
	if q == nil || q.turn != turn {
		if a.agentWakes.queued == nil {
			a.agentWakes.queued = make(map[string]*agentWakeQueued)
		}
		q = &agentWakeQueued{turn: turn, rows: make(map[string]bool)}
		a.agentWakes.queued[threadID] = q
	}
	q.rows[rowID] = allWakes
}

// forgetAgentWakeQueued drops a row whose dispatch failed: Claude never got
// it, so no turn takes it up.
func (a *App) forgetAgentWakeQueued(threadID, rowID string) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if q := a.agentWakes.queued[threadID]; q != nil && !q.ended {
		delete(q.rows, rowID)
	}
}

// agentWakeQueuedConfirmed is a queued row's echo. Before the turn it joined
// ended, that turn took it in and the row is no longer waiting.
func (a *App) agentWakeQueuedConfirmed(threadID, rowID string) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if q := a.agentWakes.queued[threadID]; q != nil && !q.ended {
		delete(q.rows, rowID)
	}
}

// tapAgentWakeTurns keeps the marks that say which of a thread's turns are
// ticks, from the turn starts and ends on the emit funnel. It runs before
// the notification tap, which reads a start's mark (agentWakeTickStarted);
// triage's quiet classifier reads the pickup a start leaves
// (agentTurnIsTick).
func (a *App) tapAgentWakeTurns(name eventchan.Channel, data any) {
	switch name {
	case eventchan.ProviderTurnStarted:
		if event, ok := data.(triage.TurnStartedEvent); ok {
			a.noteAgentWakeTurnStart(event.ThreadID, event.TurnIndex)
		}
	case eventchan.ProviderTurnCompleted:
		// CountsAsActivity is a top-level turn's end, not a subagent's.
		if event, ok := data.(triage.TurnCompletedEvent); ok && event.CountsAsActivity {
			a.endAgentWakeTickTurn(event.ThreadID, event.TurnIndex)
		}
	}
}

// agentWakeTickStarted reports whether the turn threadID started last is a
// tick, by the mark its start left (noteAgentWakeTurnStart).
func (a *App) agentWakeTickStarted(threadID string) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	return a.agentWakes.ticks[threadID]
}

// noteAgentWakeTurnStart marks a turn that starts after the one Claude's
// queued rows joined as the one that takes them up: a tick when only wakes
// are in them. A new round of the turn they joined took them in itself, and
// its rows decide.
func (a *App) noteAgentWakeTurnStart(threadID string, turnIndex int) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if q := a.agentWakes.queued[threadID]; q != nil && q.ended && turnIndex >= q.turn {
		delete(a.agentWakes.queued, threadID)
		tick := turnIndex > q.turn
		for _, wakes := range q.rows {
			tick = tick && wakes
		}
		if tick {
			if a.agentWakes.ticks == nil {
				a.agentWakes.ticks = make(map[string]bool)
			}
			if a.agentWakes.pickups == nil {
				a.agentWakes.pickups = make(map[string]agentWakePickup)
			}
			a.agentWakes.ticks[threadID] = true
			a.agentWakes.pickups[threadID] = agentWakePickup{turn: turnIndex, tick: true}
		}
	}
}

// endAgentWakeTickTurn runs when a top-level turn ends. It forgets the
// thread's tick mark, so a turn the provider starts on its own is not one;
// leaves rows Claude has not confirmed waiting for the turn after this one;
// and forgets a pickup of an earlier turn. The pickup of this turn stays
// for a later round of it (agentTurnIsTick), until a later turn ends or
// starts for a dispatch.
func (a *App) endAgentWakeTickTurn(threadID string, turnIndex int) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	delete(a.agentWakes.ticks, threadID)
	if q := a.agentWakes.queued[threadID]; q != nil && q.turn == turnIndex {
		if len(q.rows) == 0 {
			delete(a.agentWakes.queued, threadID)
		} else {
			q.ended = true
		}
	}
	if p, ok := a.agentWakes.pickups[threadID]; ok && p.turn < turnIndex {
		delete(a.agentWakes.pickups, threadID)
	}
}

// agentTurnIsTick reports whether a top-level turn is a tick, which nobody
// needs to read when it ends cleanly (triage.SetQuietTurnClassifier): every
// message it holds is a loop or watch wake, or it holds none and is the
// turn Claude took up queued rows that were only wakes in.
func (a *App) agentTurnIsTick(threadID string, turnIndex int) bool {
	tick, empty := a.agentWakeTurnMessages(threadID, turnIndex)
	if tick || !empty {
		return tick
	}
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	p, ok := a.agentWakes.pickups[threadID]
	return ok && p.turn == turnIndex && p.tick
}

// forgetAgentWakeTurns drops a thread's turn marks when its session ends:
// the next session's turns are its own.
func (a *App) forgetAgentWakeTurns(threadID string) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	delete(a.agentWakes.ticks, threadID)
	delete(a.agentWakes.queued, threadID)
	delete(a.agentWakes.pickups, threadID)
}

// agentWakeTurnMessages reads who a turn that ended was for. tick is every
// message it holds being a loop or watch wake; its rows are what both
// providers delivered into it, including a wake Codex steered into a
// person's turn and a person's message joined to a tick, which the dispatch
// cannot know. empty is a turn holding no message: the provider's own, or
// the turn Claude took queued rows up in, which show in the turn before it.
// A read error reports neither: a person's turn going silent is the worse
// mistake.
func (a *App) agentWakeTurnMessages(threadID string, turnIndex int) (tick, empty bool) {
	rows, err := a.store.ListTurnItemsOfKind(threadID, turnIndex, "user_text")
	if err != nil {
		log.Printf("agent wakes: read the messages of %s turn %d: %v", threadID, turnIndex, err)
		return false, false
	}
	empty = true
	for _, row := range rows {
		if row.ParentID != "" {
			continue
		}
		empty = false
		meta, err := usermessage.FromItem(row)
		if err != nil {
			log.Printf("agent wakes: read the meta of %s: %v", row.ID, err)
			return false, false
		}
		if !usermessage.IsTick(meta) {
			return false, false
		}
	}
	return !empty, empty
}

// flushGroupIsAgentWakes reports whether every message of a dispatch is a
// loop or watch wake.
func flushGroupIsAgentWakes(members []flushMember) bool {
	for _, member := range members {
		if !usermessage.IsTickSendID(member.payload.SendID) {
			return false
		}
	}
	return len(members) > 0
}
