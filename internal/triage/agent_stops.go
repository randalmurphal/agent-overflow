package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Agent stops (claude-wire.md §E6b; docs/specs/agent-visibility.md
// §Agent runs and stops). Every stop of a Claude background agent's run
// is a completion-shaped sibling of the row that started the run, written
// at the write head: the launch, or a §E6 resume carrier. A stop while a
// background command the agent started still runs is PARKED
// (store.ItemStatusParked): the CLI wakes the agent when the command
// reports, and the wake starts its next run. The stop with none running,
// or a kill, is the ENDING sibling, which settles the row and ends the
// agent (writeBackgroundCompletionSibling, agent_end.go). An agent's stop
// rings no bell: its sibling is its card. A parked sibling's own meta
// records its run (store.MetaKeyParkedCommands and the keys beside it).

// handleAgentStop writes a background agent's stop: a parked sibling for
// a pause, else the ending sibling, which the notification's report then
// enriches. The ending sibling merges the host terminal the stop's
// task_updated stashed; a notification that arrives with none stashed
// and no sibling written (a TaskOutput read writes one first) is the
// end on its own, since no bell holds its report for a later writer. A
// stop the typed status reports as a kill or a failure ends the agent
// whatever it still owns: its commands die with it.
func (r *Router) handleAgentStop(evt provider.ProviderEvent, meta backgroundTaskNotificationMeta, launch store.Item) error {
	if !taskStatusEnds(meta.Status) {
		park, err := r.launchParkedOn(evt.ThreadID, launch)
		if err != nil {
			return err
		}
		if park.waiting > 0 {
			return r.writeParkedStop(evt, meta, launch, park)
		}
	}
	stash, stashed, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, meta.TaskID)
	if err != nil {
		return fmt.Errorf("triage: take the stop terminal of %s/%s: %w", evt.ThreadID, meta.TaskID, err)
	}
	switch {
	case stashed:
		err = r.writeNotificationTerminal(evt, meta, launch, &stash)
	default:
		ended, lookupErr := r.rowWrittenOrQueued(evt.ThreadID, ToolCompletionID(launch.ID))
		if lookupErr != nil {
			return lookupErr
		}
		if !ended {
			err = r.writeNotificationTerminal(evt, meta, launch, nil)
		}
	}
	if err != nil {
		return err
	}
	if meta.OutputFile == "" {
		return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, meta, nil, "ready", "")
	}
	// An agent's output_file is never read (backgroundOutputPayload).
	payload, err := backgroundOutputPayload(launch, meta.OutputFile, agentReportFromNotification(launch, evt.Content), nil, eventTimestampMillis(evt))
	if err != nil {
		return err
	}
	return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, meta, payload, "loaded", "")
}

// writeParkedStop writes the parked sibling of the row a paused run
// belongs to. The stash stays for the wake to drop (persistWakePromptRow),
// the row is written once (a re-delivered stop writes nothing), and it
// carries the agent's counters as the stop reported them without taking
// them: the ending sibling folds the final ones.
func (r *Router) writeParkedStop(evt provider.ProviderEvent, meta backgroundTaskNotificationMeta, launch store.Item, park parkedStop) error {
	now := eventTimestampMillis(evt)
	id := parkedStopID(launch.ID, meta.UUID, now)
	if written, err := r.rowWrittenOrQueued(evt.ThreadID, id); err != nil || written {
		return err
	}
	// A stop follows every wake written before it, and the store orders
	// the two by creation time (Store.CurrentParkedStop), so a stop in a
	// wake's own millisecond is written after it.
	wokeAt, woken, err := r.store.LatestAgentWake(evt.ThreadID, park.rootID, now)
	if err != nil {
		return err
	}
	if woken {
		now = wokeAt + 1
	}
	startedAt, woke, err := r.agentRunStart(evt.ThreadID, launch, park.rootID)
	if err != nil {
		return err
	}
	reportID, reported, err := r.store.LatestSubagentReport(evt.ThreadID, park.rootID, startedAt)
	if err != nil {
		return err
	}
	parentID := stringsxFirst(launch.ParentID, eventParentID(evt), meta.ParentToolUseID)
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.TurnIndex, parentID)
	if err != nil {
		log.Printf("triage: parked stop turn index %s: %v", id, err)
	}

	var payload *store.Payload
	outputState := "ready"
	if report := agentReportFromNotification(launch, evt.Content); report != "" || meta.OutputFile != "" {
		if payload, err = agentReportPayload("tool-call-result:"+id, meta.OutputFile, report, now); err != nil {
			return err
		}
		outputState = "loaded"
	}
	terminal := terminalMetaFromNotification(meta)
	terminal.Source = "task_notification"
	itemMeta := mergeBackgroundCompletionItemMeta(
		backgroundCompletionItemMeta(terminal, true),
		backgroundNotificationCompletionMeta(meta, payload != nil, outputState, "", ""),
	)
	run := map[string]any{
		store.MetaKeyParkedCommands: park.waiting,
		store.MetaKeyRunStartedAt:   startedAt,
	}
	if woke {
		run[store.MetaKeyRunWoke] = true
	}
	if reported {
		run[store.MetaKeyParkedReportItemID] = reportID
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("triage: encode parked stop %s: %w", id, err)
	}
	itemMeta, _ = r.completionMetaWithFinalProgress(launch, mergeItemMetaJSON(itemMeta, encoded))

	stop := store.Item{
		ID:           id,
		ThreadID:     evt.ThreadID,
		TurnIndex:    turnIndex,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       store.ItemStatusParked,
		Summary:      parkedStopSummary(launch),
		ParentID:     parentID,
		IsBackground: true,
		CompletionOf: launch.ID,
		ToolName:     launch.ToolName,
		Meta:         itemMeta,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// The row's push announces its launch to the tray when it lands
	// (appendTrayLaunches), now or at the drain that persists it.
	return r.deferOrPersist(evt.ThreadID, queuedPersistence{item: stop, payload: payload})
}

// agentRunStart reads when the run that stops now began. The first run
// of a row began with the row. Any later one was woken, so it began at
// the newest wake under the transcript root since the row's previous
// stop, or at that stop when the parser wrote no wake row.
func (r *Router) agentRunStart(threadID string, launch store.Item, rootID string) (int64, bool, error) {
	previous, stopped, err := r.store.NewestAgentStop(threadID, launch.ID)
	if err != nil {
		return 0, false, err
	}
	if !stopped {
		return launch.CreatedAt, false, nil
	}
	wokeAt, found, err := r.store.LatestAgentWake(threadID, rootID, previous.CreatedAt)
	if err != nil {
		return 0, false, err
	}
	if found {
		return wokeAt, true, nil
	}
	return previous.CreatedAt, true, nil
}

// parkedStopID is a parked sibling's id: one per stop, keyed by the
// notification envelope, or by the stop's time when it carries no uuid.
func parkedStopID(launchID, uuid string, now int64) string {
	key := strings.TrimSpace(uuid)
	if key == "" {
		key = strconv.FormatInt(now, 10)
	}
	return ToolCompletionID(launchID) + ":parked:" + key
}

// parkedStopSummary is a parked sibling's summary, in the shape of an
// ending sibling's (buildBackgroundTerminalSummary).
func parkedStopSummary(launch store.Item) string {
	if summary := strings.TrimSpace(launch.Summary); summary != "" {
		return summary + " -> " + store.ItemStatusParked
	}
	return store.ItemStatusParked
}
