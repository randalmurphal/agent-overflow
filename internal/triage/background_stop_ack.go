package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// stopAckedSource is the status_source of a sibling a stop's reply
// writes for a task the CLI no longer had.
const stopAckedSource = "stop_acked"

// handleBackgroundTaskStopAcked settles what a successful stop_task left
// open. The CLI writes a stop's kill frames before its reply
// (claude-wire.md §E6b, captures B and E), and the reply reaches triage
// after them, so a named task still open here had nothing left to kill:
// the CLI had already ended or forgotten it without a frame AO saw (a
// dropped wake does this). Its reply is then the only answer the Stop gets,
// and without this the row stays live and the Stop does nothing.
//
// A command settles as killed unless its stashed terminal says how it
// ended. An agent settles every run of it still open, as killed: its
// stashed terminal is the stop that parked it, not an outcome.
func (r *Router) handleBackgroundTaskStopAcked(evt provider.ProviderEvent) error {
	var meta provider.BackgroundTaskStopAckedMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		return fmt.Errorf("triage: decode stop reply for thread %s: %w", evt.ThreadID, err)
	}
	if meta.TaskID == "" {
		return fmt.Errorf("triage: stop reply for thread %s names no task", evt.ThreadID)
	}
	launch, found, err := r.resolveBackgroundTaskLaunch(evt.ThreadID, "", "", meta.TaskID)
	if err != nil || !found || launch.Kind != itemKindToolCall || !launch.IsBackground {
		return err
	}
	if !store.IsAgentTranscriptLaunch(launch) {
		return r.settleStoppedRun(evt, launch.ID, meta.TaskID, false)
	}
	root, err := r.transcriptRootOrSelf(evt.ThreadID, launch)
	if err != nil {
		return err
	}
	runs, err := r.store.LiveAgentRunRows(evt.ThreadID, root.ID)
	if err != nil {
		return err
	}
	for _, runID := range runs {
		if err := r.settleStoppedRun(evt, runID, meta.TaskID, true); err != nil {
			return err
		}
	}
	return nil
}

// settleStoppedRun writes runID's sibling unless one is written or queued.
func (r *Router) settleStoppedRun(evt provider.ProviderEvent, runID, taskID string, agent bool) error {
	run, found, err := r.store.GetThreadItem(evt.ThreadID, runID)
	if err != nil {
		return fmt.Errorf("triage: read stopped run %s/%s: %w", evt.ThreadID, runID, err)
	}
	if !found || !run.IsBackground || run.Status != statusRunning {
		return nil
	}
	if settled, err := r.rowWrittenOrQueued(evt.ThreadID, ToolCompletionID(runID)); err != nil || settled {
		return err
	}
	meta := backgroundTaskTerminalMeta{TaskID: taskID, ToolUseID: runID, Source: stopAckedSource}
	stash, drained, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, taskID)
	if err != nil {
		return fmt.Errorf("triage: drain the stopped task's terminal %s/%s: %w", evt.ThreadID, taskID, err)
	}
	if drained && !agent {
		mergeStashIntoTerminalMeta(&meta, stash)
	}
	if meta.Status == "" {
		meta.Status = "killed"
	}
	end := provider.ProviderEvent{ThreadID: evt.ThreadID, ItemID: runID, Timestamp: evt.Timestamp}
	return r.writeBackgroundCompletionSibling(end, meta, drained)
}
