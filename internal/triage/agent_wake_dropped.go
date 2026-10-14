package triage

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// wakeDroppedSource is the status_source of the ending sibling a dropped
// wake writes (claude-wire.md §E6b, dropped wake).
const wakeDroppedSource = "wake_dropped"

// handleAgentWakeDropped ends a parked background agent the CLI will not
// wake. A parked agent wakes when a command it owns reports; when the CLI
// cannot resume it (its worktree is gone, it was stopped, its resume keeps
// failing) the CLI drops that report instead, and nothing else on the wire
// says so: no wake, no terminal, no level-set change. Without this the
// agent stays parked until its session ends.
//
// The drop names the command whose report was dropped. That command's
// transcript root is the agent's. The command has finished, since the CLI
// had its report to drop, whether or not its terminal is written yet. The
// agent ends only when it owns no other live command: a later command's
// report is another wake attempt, with its own recorded outcome. Each of its runs still parked ends with an errored
// sibling sourced wake_dropped, which settles the rows the agent left open
// (agent_end.go). The agent's stashed park terminal is dropped, not
// merged: it reports the stop that parked the agent, not its end.
func (r *Router) handleAgentWakeDropped(evt provider.ProviderEvent) error {
	var meta provider.AgentWakeDroppedMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		return fmt.Errorf("triage: decode dropped wake for thread %s: %w", evt.ThreadID, err)
	}
	if meta.TaskID == "" {
		return fmt.Errorf("triage: dropped wake for thread %s names no task", evt.ThreadID)
	}
	command, found, err := r.store.FindToolCallItemByTaskID(evt.ThreadID, meta.TaskID)
	if err != nil {
		return fmt.Errorf("triage: resolve the dropped notification's task %s/%s: %w", evt.ThreadID, meta.TaskID, err)
	}
	if !found || command.ParentID == "" {
		log.Printf("triage: dropped wake for task %s/%s (%s) names no agent's command", evt.ThreadID, meta.TaskID, meta.Reason)
		return nil
	}
	rootID := command.ParentID
	live, err := r.liveParkingCommands(evt.ThreadID, rootID, command.ID)
	if err != nil || live > 0 {
		return err
	}
	runs, err := r.store.LiveAgentRunRows(evt.ThreadID, rootID)
	if err != nil {
		return err
	}
	for _, runID := range runs {
		if err := r.endParkedRunOnDroppedWake(evt, runID, rootID); err != nil {
			return err
		}
	}
	return nil
}

// liveParkingCommands counts the commands under rootID a parked agent
// still waits on other than finished, less those whose completion is
// queued behind an open stream and not yet persisted.
func (r *Router) liveParkingCommands(threadID, rootID, finished string) (int, error) {
	ids, err := r.store.ParkingCommandIDs(threadID, rootID)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, id := range ids {
		if id != finished && !r.rowQueued(threadID, ToolCompletionID(id)) {
			live++
		}
	}
	return live, nil
}

// endParkedRunOnDroppedWake writes the ending sibling of runID, one of the
// agent's runs, when it is a background agent run parked at its newest
// stop and no ending sibling is written or queued.
func (r *Router) endParkedRunOnDroppedWake(evt provider.ProviderEvent, runID, rootID string) error {
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, runID)
	if err != nil {
		return fmt.Errorf("triage: read agent run %s/%s: %w", evt.ThreadID, runID, err)
	}
	if !found || !launch.IsBackground || !store.IsAgentTranscriptLaunch(launch) {
		return nil
	}
	if _, parked, err := r.store.CurrentParkedStop(evt.ThreadID, runID, rootID); err != nil || !parked {
		return err
	}
	if ended, err := r.rowWrittenOrQueued(evt.ThreadID, ToolCompletionID(runID)); err != nil || ended {
		return err
	}
	taskID := TaskIDFromItemMeta(launch.Meta)
	drained := false
	if taskID != "" {
		if _, drained, err = r.store.TakePendingBackgroundTerminal(evt.ThreadID, taskID); err != nil {
			return fmt.Errorf("triage: drop the park terminal of %s/%s: %w", evt.ThreadID, taskID, err)
		}
	}
	end := provider.ProviderEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    runID,
		Timestamp: evt.Timestamp,
	}
	return r.writeBackgroundCompletionSibling(end, backgroundTaskTerminalMeta{
		TaskID:    taskID,
		ToolUseID: runID,
		Status:    "failed",
		Source:    wakeDroppedSource,
	}, drained)
}
