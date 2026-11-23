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
	if _, hasSession := a.activeCodexSession(thread.ID); !hasSession {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexapp.BackgroundTerminalStopTimeout)
	defer cancel()
	if err := a.stopCodexCommandsBeforeRevert(ctx, thread.ID); err != nil {
		return fmt.Errorf("%s: %w", errorPrefix, err)
	}
	return nil
}

// stopCodexCommandsBeforeRevert cleans the processes of every Codex thread
// of the session and returns once no command of the thread is live, or
// fails, naming the commands Codex still runs, when ctx ends first. A
// clean's answer comes before its processes are gone, so the commands are
// waited for: each leaves by its own item/completed, or when its thread's
// list no longer holds it, which is asked again at each change. A command
// that started after its thread was cleaned gets a clean of its own.
func (a *App) stopCodexCommandsBeforeRevert(ctx context.Context, threadID string) error {
	cleaned, err := a.cleanCodexScopes(ctx, threadID, nil)
	if err != nil {
		return a.codexCommandsStillRunning(threadID, fmt.Errorf("clean Codex background terminals: %w", err))
	}
	if a.triage == nil {
		return nil
	}
	for {
		changed := a.triage.CodexExecChanged(threadID)
		if cleaned, err = a.cleanCodexScopes(ctx, threadID, cleaned); err != nil {
			return a.codexCommandsStillRunning(threadID, fmt.Errorf("clean Codex background terminals: %w", err))
		}
		checks := map[string]triage.CodexScopeCheck{}
		for _, command := range a.triage.LiveCodexCommands(threadID) {
			if command.Addressable {
				checks[command.ScopeID] = command.Check
			}
		}
		for _, check := range checks {
			if err := a.confirmCodexScope(ctx, threadID, check, false); err != nil {
				return a.codexCommandsStillRunning(threadID, fmt.Errorf("list Codex background terminals: %w", err))
			}
		}
		if len(a.triage.LiveCodexCommands(threadID)) == 0 {
			return nil
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
