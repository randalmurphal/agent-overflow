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
// a pause, else the ending sibling. Either lands with the stop's report
// in its first write (agentStopReport), so a mounted card never gains or
// changes its answer. The ending sibling merges the host terminal the
// stop's task_updated stashed; a notification that arrives with none
// stashed and no sibling written (a TaskOutput read writes one first) is
// the end on its own, since no bell holds its report for a later writer.
// A stop the typed status reports as a kill or a failure ends the agent
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
	report, err := newAgentStopReport("tool-call-result:"+launch.ID, launch, meta, evt.Content, eventTimestampMillis(evt))
	if err != nil {
		return err
	}
	stash, stashed, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, meta.TaskID)
	if err != nil {
		return fmt.Errorf("triage: take the stop terminal of %s/%s: %w", evt.ThreadID, meta.TaskID, err)
	}
	if stashed {
		return r.writeNotificationTerminal(evt, meta, launch, &stash, &report)
	}
	ended, err := r.rowWrittenOrQueued(evt.ThreadID, ToolCompletionID(launch.ID))
	if err != nil {
		return err
	}
	if ended {
		return r.fillAgentStopReport(evt, ToolCompletionID(launch.ID), report)
	}
	return r.writeNotificationTerminal(evt, meta, launch, nil, &report)
}

// notificationUsageMetaKey records, on a stop's row, the usage of the
// notification that recorded the stop (backgroundNotificationCompletionMeta).
const notificationUsageMetaKey = "notification_usage"

// agentStopCopy reports whether an agent's notification repeats the stop
// its newest row records. The CLI hands a stop to the model again when
// the model is mid-turn, as `<task-notification>` XML on the `isReplay`
// echo at its next tool round (claude-wire.md §Synthetic-XML), and that
// copy can arrive while the agent is still parked, or after its command
// reported but before it woke. The usage a stop reports names it: the
// copy reports the same numbers, and a later stop reports a longer
// duration, since the agent's duration runs from its launch. A
// notification without usage, such as the one a kill sends, is never a
// copy.
func (r *Router) agentStopCopy(threadID string, launch store.Item, usage provider.SubagentProgressMeta) (bool, error) {
	if usage == (provider.SubagentProgressMeta{}) {
		return false, nil
	}
	stop, found, err := r.newestStopRow(threadID, launch.ID)
	if err != nil || !found {
		return false, err
	}
	var meta struct {
		Usage provider.SubagentProgressMeta `json:"notification_usage"`
	}
	if json.Unmarshal([]byte(stop.Meta), &meta) != nil {
		return false, nil
	}
	return meta.Usage == usage, nil
}

// agentStopReport is how a stop's row records the agent's report: the
// payload whose preview is the report head (the card's collapsed answer)
// and the notification state the card reads it by. An agent's
// output_file is never read (backgroundOutputPayload).
type agentStopReport struct {
	payload *store.Payload
	meta    string
}

// newAgentStopReport builds a stop's report, on the payload payloadID,
// from its notification; summary is the envelope's summary, which for an
// agent is its report (agentReportFromNotification).
func newAgentStopReport(payloadID string, launch store.Item, meta backgroundTaskNotificationMeta, summary string, now int64) (agentStopReport, error) {
	var payload *store.Payload
	outputState := "ready"
	if report := agentReportFromNotification(launch, summary); report != "" || meta.OutputFile != "" {
		var err error
		if payload, err = agentReportPayload(payloadID, meta.OutputFile, report, now); err != nil {
			return agentStopReport{}, err
		}
		outputState = "loaded"
	}
	return agentStopReport{
		payload: payload,
		meta:    backgroundNotificationCompletionMeta(meta, payload != nil, outputState, "", ""),
	}, nil
}

// fillAgentStopReport handles a notification of a stop whose ending
// sibling is already written or queued. Either another signal recorded
// the stop first (a kill's task_updated, a TaskOutput read), or this is
// the CLI handing the same stop to the model again at its next tool
// round, whose summary is only the `Agent "…" finished` bell unless the
// parser lifted the report from its `<result>`. The sibling keeps the
// report it shows: this can give it the report it was written without,
// never replace or clear one.
func (r *Router) fillAgentStopReport(evt provider.ProviderEvent, rowID string, report agentStopReport) error {
	if !payloadHasPreview(report.payload) {
		return nil
	}
	_, err := r.patchWrittenRow(evt.ThreadID, rowID, func(item *store.Item, payload **store.Payload) bool {
		if item.Kind != itemKindBackgroundDone || showsAgentStopReport(*item, *payload) {
			return false
		}
		item.PayloadID = report.payload.ID
		*payload = report.payload
		item.Meta = mergeBackgroundCompletionItemMeta(item.Meta, report.meta)
		item.UpdatedAt = eventTimestampMillis(evt)
		return true
	})
	return err
}

// showsAgentStopReport reports whether a stop's row shows a report as its
// card's answer: its notification state says loaded and the payload it
// carries has a preview, the pair the frontend reads
// (completionAnswerPreview). payload is the one the row's pending write
// carries, or nil for a stored row, whose payload meta rides on the item.
func showsAgentStopReport(item store.Item, payload *store.Payload) bool {
	var meta struct {
		Loaded bool `json:"notification_output_loaded"`
	}
	if json.Unmarshal([]byte(item.Meta), &meta) != nil || !meta.Loaded {
		return false
	}
	if payload != nil && payload.ID == item.PayloadID {
		return payloadHasPreview(payload)
	}
	return payloadMetaHasPreview(item.PayloadMeta)
}

func payloadHasPreview(payload *store.Payload) bool {
	return payload != nil && payloadMetaHasPreview(payload.Meta)
}

func payloadMetaHasPreview(raw string) bool {
	var meta struct {
		Preview string `json:"preview"`
	}
	return json.Unmarshal([]byte(raw), &meta) == nil && strings.TrimSpace(meta.Preview) != ""
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
	parentID, err := r.agentStopScope(evt.ThreadID, stringsxFirst(launch.ParentID, eventParentID(evt), meta.ParentToolUseID))
	if err != nil {
		return err
	}
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.TurnIndex, parentID)
	if err != nil {
		log.Printf("triage: parked stop turn index %s: %v", id, err)
	}

	report, err := newAgentStopReport("tool-call-result:"+id, launch, meta, evt.Content, now)
	if err != nil {
		return err
	}
	terminal := terminalMetaFromNotification(meta)
	terminal.Source = "task_notification"
	itemMeta := mergeBackgroundCompletionItemMeta(backgroundCompletionItemMeta(terminal, true), report.meta)
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
	return r.deferOrPersist(evt.ThreadID, queuedPersistence{item: stop, payload: report.payload})
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

// agentStopScope is the scope a stop of an agent launched under parentID
// files its card in. A nested async agent is a task of the main session
// (claude-wire.md §Background task ownership): it can outlive the agent
// that launched it, and that agent's last card is a snapshot up to its
// own stop that never shows a later row. So the stop files under the
// nearest enclosing agent still running, whose next card holds it, and
// at top level when none is.
func (r *Router) agentStopScope(threadID, parentID string) (string, error) {
	for hops := 0; parentID != "" && hops < maxAgentScopeHops; hops++ {
		parent, found, err := r.store.GetThreadItem(threadID, parentID)
		if err != nil {
			return "", fmt.Errorf("triage: read the agent %s/%s a stop files under: %w", threadID, parentID, err)
		}
		if !found || !store.IsAgentTranscriptLaunch(parent) {
			return parentID, nil
		}
		live, err := r.agentRunLive(threadID, parent.ID)
		if err != nil || live {
			return parentID, err
		}
		parentID = parent.ParentID
	}
	return parentID, nil
}

// maxAgentScopeHops bounds agentStopScope's walk, so a parent cycle in
// provider data ends it.
const maxAgentScopeHops = 16

// agentRunLive reports whether the agent whose transcript root is rootID
// still runs: a row of its runs is running and no ending sibling of that
// row waits behind an open stream to be written.
func (r *Router) agentRunLive(threadID, rootID string) (bool, error) {
	ids, err := r.store.LiveAgentRunRows(threadID, rootID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if !r.rowQueued(threadID, ToolCompletionID(id)) {
			return true, nil
		}
	}
	return false, nil
}
