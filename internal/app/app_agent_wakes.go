package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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

// Watch registers or replaces one watch of the caller's thread. Only a
// standing agent may: a watch runs a script with the agent's home, and a
// thread the user has not promoted has none.
func (t threadToolsApp) Watch(_ context.Context, caller threadtools.Caller, call threadtools.WatchCall) (threadtools.WakeState, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	}
	if _, standing, err := t.app.store.GetStandingAgent(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	} else if !standing {
		return threadtools.WakeState{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"thread_watch is for standing agents, and this thread is not one: a watch runs a script with the agent's own home directory, which only a thread the user promoted to a standing agent has. Use thread_loop, which any thread may register, or ask the user to promote this thread.", nil)
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

// CancelWake cancels the caller's live loop or watch with this label and
// stops what it has running.
func (t threadToolsApp) CancelWake(_ context.Context, caller threadtools.Caller, label, kind string) (threadtools.WakeState, bool, error) {
	row, cancelled, err := t.app.store.CancelAgentWake(caller.ThreadID, label, kind, agentWakeOutcomeCancelled)
	if err != nil || !cancelled {
		return threadtools.WakeState{}, false, err
	}
	t.app.stopAgentWakeRun(row.ID)
	row.State, row.NextAt = store.AgentWakeCancelled, 0
	return agentWakeToolState(row), true, nil
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
		LastRunAt: row.LastRunAt, LastOutcome: row.LastOutcome, LastOutput: row.LastOutput, Missed: row.Missed,
	}
}

// cancelAgentWakesForThread cancels every loop and watch of a thread that is
// being archived, deleted or moved, and stops what they have running. Rows a
// delete has already cascaded away still have their runs stopped by thread.
func (a *App) cancelAgentWakesForThread(threadID, outcome string) {
	if _, err := a.store.CancelAgentWakesForThread(threadID, "", outcome); err != nil {
		log.Printf("agent wakes: cancel the loops and watches of %s: %v", threadID, err)
	}
	a.stopAgentWakeRunsForThread(threadID)
}

// noteAgentWakeDispatch records whether the turn a flush dispatch starts is a
// tick: one only loop and watch wakes started. It decides only whether that
// turn's start retracts a notification; whether its end raises one is read
// from its rows (agentWakeTurnMessages). A dispatch into a running turn
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

// agentWakeTickTurn reports, at a turn's start, whether it is a tick. A
// turn that starts after the one Claude's queued rows joined is the one
// that takes them up: a tick when only wakes are in them. A new round of
// the turn they joined took them in itself, and its rows decide.
func (a *App) agentWakeTickTurn(threadID string, turnIndex int) bool {
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
	return a.agentWakes.ticks[threadID]
}

// endAgentWakeTickTurn runs when a top-level turn ends. It forgets the
// thread's tick mark, so a turn the provider starts on its own is not one;
// leaves rows Claude has not confirmed waiting for the turn after this one;
// and reports whether this turn took up queued rows that were only wakes.
func (a *App) endAgentWakeTickTurn(threadID string, turnIndex int) (pickupTick bool) {
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
	if p, ok := a.agentWakes.pickups[threadID]; ok && p.turn <= turnIndex {
		delete(a.agentWakes.pickups, threadID)
		return p.turn == turnIndex && p.tick
	}
	return false
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
		ids := meta.JoinedSendIDs
		if len(ids) == 0 {
			ids = []string{meta.SendID}
		}
		for _, id := range ids {
			if !strings.HasPrefix(id, agentWakeSendIDRoot) {
				return false, false
			}
		}
	}
	return !empty, empty
}

// agentTicksAreQuiet reports whether a thread's ticks stay silent: always,
// unless its standing agent row turns tick notifications on. A read error
// keeps the default, which is quiet.
func (a *App) agentTicksAreQuiet(threadID string) bool {
	agent, standing, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		log.Printf("agent wakes: read the standing agent row of %s: %v", threadID, err)
		return true
	}
	return !standing || !agent.NotifyOnTick
}

// flushGroupIsAgentWakes reports whether every message of a dispatch is a
// loop or watch wake.
func flushGroupIsAgentWakes(members []flushMember) bool {
	for _, member := range members {
		if !strings.HasPrefix(member.payload.SendID, agentWakeSendIDRoot) {
			return false
		}
	}
	return len(members) > 0
}
