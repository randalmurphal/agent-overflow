package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// The run state of a Claude background agent launch (claude-wire.md
// §E6b), served on the live list's rows and never stored: the launch row
// is immutable history (docs/specs/agent-visibility.md), and a park or a
// wake must not move its revision. The frontend mirrors the keys in
// frontend/src/lib/utils/subagentRunState.ts (mirror_pins_test.go).
const (
	metaKeySubagentRunState            = "subagentRunState"
	metaKeySubagentParkedCommands      = "subagentParkedCommands"
	metaKeySubagentParkedReportID      = "subagentParkedReportId"
	metaKeySubagentParkedReportPreview = "subagentParkedReportPreview"

	subagentRunRunning = "running"
	subagentRunParked  = "parked"
	subagentRunDone    = "done"
	subagentRunEnded   = "ended"
)

// A parked stop's bell (parkedBellMeta) is stored history that names the
// round's report. The frontend mirrors these keys in
// frontend/src/lib/utils/parkedAgentBell.ts (mirror_pins_test.go).
const (
	notificationKindParkedAgent = "parked_agent"
	metaKeyParkedCommands       = "parked_commands"
	metaKeyParkedReportItemID   = "parked_report_item_id"
	metaKeyParkedReportPreview  = "parked_report_preview"
)

// DecorateAgentRunStates adds the run state to every background agent
// launch (isSubagentTranscriptLaunch) in a Store.ListLiveBackgroundTasks
// read. It is the park model's own state, one keyed lookup per launch:
//
//   - a completion sibling settles the launch: "ended" when a session
//     death wrote it (status_source session_died), "done" otherwise. The
//     list returns a settled launch together with its sibling, so the
//     sibling is read from the list;
//   - else a stashed terminal for its task parks it: a parked stop keeps
//     the stash until the wake drops it (persistWakePromptRow). A parked
//     launch also serves the background commands at its transcript root
//     it waits on (launchParkedOn's count) and its newest report there;
//   - else it is running.
//
// Between a final stop's task_updated and its notification the stash is
// present and no sibling exists yet, so a read in that window says parked
// on zero commands; the sibling write that follows nudges the tray.
func (r *Router) DecorateAgentRunStates(threadID string, items []store.Item) ([]store.Item, error) {
	var siblings map[string]store.Item
	for _, item := range items {
		if item.CompletionOf != "" {
			if siblings == nil {
				siblings = make(map[string]store.Item)
			}
			siblings[item.CompletionOf] = item
		}
	}
	for i, item := range items {
		if item.CompletionOf != "" || !item.IsBackground || !isSubagentTranscriptLaunch(item) {
			continue
		}
		fields, err := r.agentRunState(threadID, item, siblings)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("triage: encode run state of %s/%s: %w", threadID, item.ID, err)
		}
		items[i].Meta = mergeItemMetaJSON(item.Meta, encoded)
	}
	return items, nil
}

func (r *Router) agentRunState(threadID string, launch store.Item, siblings map[string]store.Item) (map[string]any, error) {
	if sibling, settled := siblings[launch.ID]; settled {
		if completionStatusSource(sibling.Meta) == "session_died" {
			return map[string]any{metaKeySubagentRunState: subagentRunEnded}, nil
		}
		return map[string]any{metaKeySubagentRunState: subagentRunDone}, nil
	}
	running := map[string]any{metaKeySubagentRunState: subagentRunRunning}
	taskID := TaskIDFromItemMeta(launch.Meta)
	if taskID == "" {
		return running, nil
	}
	if _, stashed, err := r.store.GetPendingBackgroundTerminal(threadID, taskID); err != nil {
		return nil, fmt.Errorf("triage: run state stash %s/%s: %w", threadID, taskID, err)
	} else if !stashed {
		return running, nil
	}
	root, err := r.transcriptRootOrSelf(threadID, launch)
	if err != nil {
		return nil, err
	}
	waiting, err := r.commandsParkingAt(threadID, root.ID)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		metaKeySubagentRunState:       subagentRunParked,
		metaKeySubagentParkedCommands: waiting,
	}
	report, found, err := r.store.LatestSubagentReport(threadID, root.ID)
	if err != nil {
		return nil, err
	}
	if found {
		fields[metaKeySubagentParkedReportID] = report.ID
		fields[metaKeySubagentParkedReportPreview] = report.Preview
	}
	return fields, nil
}

// completionStatusSource reads the terminal source a completion sibling
// records (backgroundCompletionItemMeta).
func completionStatusSource(meta string) string {
	if !strings.Contains(meta, "status_source") {
		return ""
	}
	var decoded struct {
		StatusSource string `json:"status_source"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return ""
	}
	return decoded.StatusSource
}
