package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

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

func (a *App) markConfirmedBackgroundTasksInactiveAfterProviderCleanup(threadID string, errorPrefix string) error {
	if a.triage != nil {
		a.triage.ClearLiveCodexBackgroundTasks(threadID)
	}
	now := time.Now().UnixMilli()
	if _, err := a.store.MarkLiveBackgroundToolCallsInactive(threadID, now); err != nil {
		return fmt.Errorf("%s: clear running background tasks: %w", errorPrefix, err)
	}
	if _, err := a.store.MarkLiveCodexSubagentLaunchesInactive(threadID, now); err != nil {
		return fmt.Errorf("%s: clear Codex subagent background tasks: %w", errorPrefix, err)
	}
	a.emit("provider:background_tasks_changed", map[string]any{"threadId": threadID})
	return nil
}
