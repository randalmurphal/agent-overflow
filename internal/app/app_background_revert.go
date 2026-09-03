package app

import (
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

//ao:scope threads:read
func (a *App) CountRunningBackgroundTasks(threadID string) (int, error) {
	return a.countRunningBackgroundTasks(threadID)
}

func (a *App) countRunningBackgroundTasks(threadID string) (int, error) {
	total, err := a.store.CountLiveRunningBackgroundToolCalls(threadID)
	if err != nil {
		return 0, err
	}
	codexSubagents, err := a.store.CountLiveCodexSubagentLaunches(threadID)
	if err != nil {
		return 0, err
	}
	total += codexSubagents
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
// the app-server and the protocol exposes only the thread-wide clean
// RPC, which requires the session to still be LIVE — so this must run
// before the rollback stops it. Claude background tasks die with
// stopSession's process-group close; no pre-stop step exists for them.
func (a *App) cleanRunningBackgroundTasksBeforeProviderRevert(thread store.Thread, errorPrefix string) error {
	caps := provider.CapabilitiesForProvider(thread.Provider)
	if caps.BackgroundTerminalCleaner != provider.CodexBackgroundTerminalCleaner {
		return nil
	}
	if _, hasSession := a.activeCodexSession(thread.ID); !hasSession {
		return nil
	}
	if err := a.CleanCodexBackgroundTerminals(thread.ID); err != nil {
		return fmt.Errorf("%s: clean Codex background terminals: %w", errorPrefix, err)
	}
	return nil
}

// markConfirmedBackgroundTasksInactiveAfterProviderCleanup flips every
// persisted "running" background row (tool calls + Codex subagent
// launches) to inactive and drops the triage-side transient trackers,
// then notifies the tray. Runs immediately after provider-owned work is
// terminated and BEFORE the destructive truncation, so killed work does
// not stay advertised as running if a later rollback step fails.
//
// The work is already dead by the time this runs, so a failed row flip
// must not short-circuit the rest: both flips are attempted, the tray
// notification fires regardless (the triage-side clear above already
// changed the live counts, and a partial flip changed persisted state),
// and the errors surface joined.
func (a *App) markConfirmedBackgroundTasksInactiveAfterProviderCleanup(threadID string, errorPrefix string) error {
	if a.triage != nil {
		a.triage.ClearLiveCodexBackgroundTasks(threadID)
	}
	now := time.Now().UnixMilli()
	_, toolCallErr := a.store.MarkLiveBackgroundToolCallsInactive(threadID, now)
	_, subagentErr := a.store.MarkLiveCodexSubagentLaunchesInactive(threadID, now)
	a.emit(eventchan.ProviderBackgroundTasksChanged, map[string]any{"threadId": threadID})
	if toolCallErr != nil {
		toolCallErr = fmt.Errorf("%s: clear running background tasks: %w", errorPrefix, toolCallErr)
	}
	if subagentErr != nil {
		subagentErr = fmt.Errorf("%s: clear Codex subagent background tasks: %w", errorPrefix, subagentErr)
	}
	return errors.Join(toolCallErr, subagentErr)
}
