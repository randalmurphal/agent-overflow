package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/codexapp"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

//ao:scope threads:read
func (a *App) CountRunningBackgroundTasks(threadID string) (int, error) {
	return a.countRunningBackgroundTasks(threadID)
}

// countRunningBackgroundTasks counts the background work ending the
// thread's session would lose: the store's live launches and the Codex
// agents and commands only the router holds. Every gate on ending a
// session reads it, so a source added here reaches all of them.
func (a *App) countRunningBackgroundTasks(threadID string) (int, error) {
	total, err := a.store.CountLiveRunningBackgroundToolCalls(threadID)
	if err != nil {
		return 0, err
	}

	if a.triage != nil {
		total += a.triage.CountLiveCodexBackgroundTasks(threadID)
	}
	return total, nil
}

func (a *App) hasRunningBackgroundTasks(threadID string) (bool, error) {
	count, err := a.countRunningBackgroundTasks(threadID)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// cleanRunningBackgroundTasksBeforeProviderRevert terminates
// provider-owned background work that a session stop alone would not
// reach cleanly. Only Codex needs it: background terminals belong to
// the app-server, a revert can keep the session live, and each Codex
// thread's terminals are cleaned by its own thread-wide clean RPC
// (stopCodexCommandsBeforeRevert). The RPC requires the session to still
// be LIVE, so this must run before the rollback can stop it. Claude
// background tasks die with stopSession's process-group close; no
// pre-stop step exists for them.
func (a *App) cleanRunningBackgroundTasksBeforeProviderRevert(thread store.Thread, errorPrefix string) error {
	caps := provider.CapabilitiesForProvider(thread.Provider)
	if caps.BackgroundTerminalCleaner != provider.CodexBackgroundTerminalCleaner {
		return nil
	}
	session, hasSession := a.activeCodexSession(thread.ID)
	if !hasSession {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	if err := a.stopCodexCommandsBeforeRevert(ctx, thread.ID, session); err != nil {
		return fmt.Errorf("%s: %w", errorPrefix, err)
	}
	return nil
}

// stopCodexCommandsBeforeRevert returns once no Codex command of the thread
// is live, or fails, naming the commands still running, when ctx ends
// first. Every request goes to session, the one the revert found live.
//
// A revert that keeps the session reloads only the root thread
// (thread/revert), so the thread's agents would go on running and could
// start a command after any check. Each active agent is interrupted first,
// as Stop All does: Codex answers an interrupt when the agent's turn has
// ended (app-server bespoke_event_handling.rs
// respond_to_pending_interrupts), so every command the agent started is
// read by then. The thread's provider events already read are handled
// next, so a command whose start waits in that queue is counted. Then the
// root thread and each thread that runs a live command are cleaned. A
// clean's answer comes before its processes are gone, so the revert waits
// for each command's own item/completed or its thread's unload, and cleans
// again for a command that started since.
//
// Codex writes a cleaned command's completion whether or not its process
// died: an exec-server process's termination runs detached and its error is
// discarded (core unified_exec process.rs terminate). The wait cannot tell
// that case from a stopped process.
func (a *App) stopCodexCommandsBeforeRevert(ctx context.Context, threadID string, session *codex.Session) error {
	if a.triage != nil {
		for _, agent := range a.triage.ListLiveCodexAgentTasks(threadID) {
			if _, err := session.InterruptSubagent(ctx, agent.ID); err != nil {
				return fmt.Errorf("the agent %q could not be stopped; stop it and try again: %w", firstNonEmpty(agent.Summary, agent.ID), err)
			}
		}
		a.drainProviderEvents(threadID, "revert: handle the stopped agents' last events")
	}
	cleaned, err := a.cleanCodexScopes(ctx, threadID, session, nil)
	if err != nil {
		return a.codexCommandsStillRunning(threadID, fmt.Errorf("clean Codex background terminals: %w", err))
	}
	if a.triage == nil {
		return nil
	}
	for {
		changed := a.triage.CodexExecChanged(threadID)
		if len(a.triage.LiveCodexCommands(threadID)) == 0 {
			return nil
		}
		if cleaned, err = a.cleanCodexScopes(ctx, threadID, session, cleaned); err != nil {
			return a.codexCommandsStillRunning(threadID, fmt.Errorf("clean Codex background terminals: %w", err))
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return a.codexCommandsStillRunning(threadID, fmt.Errorf("they did not end within %s of the stop", codexapp.BackgroundTerminalStopTimeout))
		}
	}
}

// codexCommandsStillRunning is a revert's failure while Codex commands may
// still run: it names them, so the person can stop them and try again.
func (a *App) codexCommandsStillRunning(threadID string, cause error) error {
	var names []string
	if a.triage != nil {
		for _, command := range a.triage.LiveCodexCommands(threadID) {
			names = append(names, fmt.Sprintf("%q", command.Summary))
		}
	}
	if len(names) == 0 {
		return cause
	}
	sort.Strings(names)
	return fmt.Errorf("these commands are still running: %s; stop them and try again: %w", strings.Join(names, ", "), cause)
}

// markConfirmedBackgroundTasksInactiveAfterProviderCleanup flips every
// persisted "running" background row (tool calls + Codex subagent
// launches) to inactive and drops the agent completions triage holds for
// an answer, then notifies the tray. Runs after provider-owned work is
// confirmed gone and BEFORE the destructive truncation, so killed work
// does not stay advertised as running if a later rollback step fails.
//
// The work is already dead by the time this runs, so a failed row flip
// must not short-circuit the rest: the tray notification fires
// regardless (a partial flip changed persisted state), and the error
// surfaces.
func (a *App) markConfirmedBackgroundTasksInactiveAfterProviderCleanup(threadID string, errorPrefix string) error {
	if a.triage != nil {
		a.triage.DropHeldCodexCompletions(threadID)
	}
	now := time.Now().UnixMilli()
	_, toolCallErr := a.store.MarkLiveBackgroundToolCallsInactive(threadID, now)
	a.emitBackgroundChanged(threadID)
	if toolCallErr != nil {
		toolCallErr = fmt.Errorf("%s: clear running background tasks: %w", errorPrefix, toolCallErr)
	}
	return toolCallErr
}

// emitBackgroundChanged announces a change to a thread's live background
// work that no tray delta carries (a remote job, a teardown's inactive
// mark): the provider:background_tasks_changed nudge, and a tray refresh.
func (a *App) emitBackgroundChanged(threadID string) {
	a.emit(eventchan.ProviderBackgroundTasksChanged, triage.BackgroundTasksChangedEvent{ThreadID: threadID})
	a.emit(eventchan.ProviderBackgroundTray, triage.BackgroundTrayEvent{ThreadID: threadID, Refresh: true})
}
