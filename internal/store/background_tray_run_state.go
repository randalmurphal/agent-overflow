package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// The run state of a Claude background agent launch (claude-wire.md
// §E6b), served on the tray's rows and never stored: the launch row is
// immutable history (docs/specs/agent-visibility.md), and a park or a
// wake must not move its revision. The frontend mirrors the keys and the
// states in frontend/src/lib/utils/subagentRunState.ts
// (internal/triage/mirror_pins_test.go).
const (
	MetaKeySubagentRunState            = "subagentRunState"
	MetaKeySubagentParkedCommands      = "subagentParkedCommands"
	MetaKeySubagentParkedReportID      = "subagentParkedReportId"
	MetaKeySubagentParkedReportPreview = "subagentParkedReportPreview"

	AgentRunRunning = "running"
	AgentRunParked  = "parked"
	AgentRunDone    = "done"
	AgentRunEnded   = "ended"
)

// commandOutputToolNames are the tools whose output is a command's
// captured stdout and stderr.
var commandOutputToolNames = []string{"Bash", "command_execution", "commandExecution", "exec_command"}

// IsCommandOutputToolName reports whether a tool's output is a command's
// captured stdout and stderr.
func IsCommandOutputToolName(toolName string) bool {
	return slices.Contains(commandOutputToolNames, strings.TrimSpace(toolName))
}

// IsAgentTranscriptLaunch reports whether a launch is an agent, whose
// `output_file` is its sidechain transcript, rather than a task whose
// `output_file` is captured stdout/stderr (a background Bash, a Monitor
// watch). Claude names the same field for every task type and backgrounds
// both kinds, so only the agent's identity tells them apart: the agent
// tool, "Agent" or "Task" on older CLIs (the parser's isAgentLaunchToolName
// set), or a §E6 resume carrier, the SendMessage row that runs a resumed
// agent's round, which the parser stamps with the agent it resumes (the
// keys triage's isResumeCarrierMeta reads). backgroundOutputPayload
// splits on the same test.
func IsAgentTranscriptLaunch(launch Item) bool {
	if launch.Kind != "tool_call" {
		return false
	}
	switch strings.TrimSpace(launch.ToolName) {
	case "Agent", "Task":
		return true
	}
	var carrier struct {
		TranscriptRootID string `json:"transcript_root_id"`
		ResumesToolUseID string `json:"resumes_tool_use_id"`
		Description      string `json:"description"`
		SubagentType     string `json:"subagent_type"`
	}
	if json.Unmarshal([]byte(launch.Meta), &carrier) != nil {
		return false
	}
	return carrier.TranscriptRootID != "" || carrier.ResumesToolUseID != "" ||
		carrier.Description != "" || carrier.SubagentType != ""
}

// AgentRunState reads the run state a tray read served on a launch row,
// or "" for a row it did not serve one on.
func AgentRunState(item Item) string {
	if !strings.Contains(item.Meta, MetaKeySubagentRunState) {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(item.Meta), &fields) != nil {
		return ""
	}
	var state string
	if json.Unmarshal(fields[MetaKeySubagentRunState], &state) != nil {
		return ""
	}
	return state
}

// countParkingCommandsSQL counts the live background commands under a
// transcript root that an agent stopping now would wait on. An async agent
// that stops while one of those is still running is idle, not done: the
// CLI wakes it when the command reports (claude-wire.md §E6b), and these
// rows are the only evidence, because the agent's own terminal and
// notification look exactly like a final stop's. A shell or a watch task
// counts; a nested async agent does not, as it is a task of its own and
// never wakes its parent.
//
// A stashed command (exited, terminal not yet observed) still counts: on
// the wire its notification follows its terminal immediately and the
// agent's wake follows both, so the transient state resolves inside the
// same flush either way.
//
// Direct children only: everything an agent produces, in every resumed
// round, is parented to its transcript ROOT (triage's transcript_root.go),
// so the caller passes the root. Served by the partial idx_items_parent,
// which is why the empty-parent guard is repeated beside the root.
var countParkingCommandsSQL = func() string {
	names := make([]string, len(commandOutputToolNames))
	for i, name := range commandOutputToolNames {
		names[i] = "'" + name + "'"
	}
	return `SELECT COUNT(*) FROM items w
		  WHERE w.thread_id = ?
		    AND w.parent_id = ?
		    AND w.parent_id <> ''
		    AND w.kind = 'tool_call'
		    AND w.status = 'running'
		    AND w.is_background = 1
		    AND NOT EXISTS (
		      SELECT 1 FROM items c
		       WHERE c.thread_id = w.thread_id
		         AND c.completion_of = w.id
		         AND c.completion_of <> ''
		    )
		    AND (trim(w.tool_name) IN (` + strings.Join(names, ", ") + `)
		         OR json_type(w.meta, '$.watch_task') = 'true')`
}()

// CountParkingCommands counts the live background commands under rootID,
// a transcript root, that an agent stopping now would wait on: a stop
// with any is a pause, and its parked sibling records the count
// (MetaKeyParkedCommands).
func (s *Store) CountParkingCommands(threadID, rootID string) (int, error) {
	var waiting int
	if err := s.reader().QueryRow(countParkingCommandsSQL, threadID, rootID).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("store: count the commands %s/%s waits on: %w", threadID, rootID, err)
	}
	return waiting, nil
}

// parkedRun is what a tray statement read of one row's parked stop
// (trayProjectionSQL): whether the launch is paused at one, and what that
// sibling recorded of its run.
type parkedRun struct {
	parked   bool
	commands int64
	reportID string
	preview  string
}

// serveAgentRunStates adds the run state to every background agent launch
// (IsAgentTranscriptLaunch) of a tray read, stops[i] being the parked stop
// read for items[i]. It is the park model's own state (agent_stops.go):
//
//   - an ending sibling settles the launch: "ended" when a session death
//     wrote it (status_source session_died), "done" otherwise. The read
//     returns a settled launch together with that sibling;
//   - else the launch is parked while its newest stop is a parked sibling
//     no wake has followed (Store.CurrentParkedStop). A parked launch also
//     serves what that sibling records: the background commands it waits
//     on and its run's report, by row id with the head of its text;
//   - else it is running.
func serveAgentRunStates(items []Item, stops []parkedRun) error {
	siblings := make(map[string]Item)
	for _, item := range items {
		if item.CompletionOf != "" {
			siblings[item.CompletionOf] = item
		}
	}
	for i, item := range items {
		if item.CompletionOf != "" || !item.IsBackground || !IsAgentTranscriptLaunch(item) {
			continue
		}
		fields := map[string]any{MetaKeySubagentRunState: AgentRunRunning}
		if sibling, settled := siblings[item.ID]; settled {
			fields[MetaKeySubagentRunState] = AgentRunDone
			if completionStatusSource(sibling.Meta) == "session_died" {
				fields[MetaKeySubagentRunState] = AgentRunEnded
			}
		} else if stop := stops[i]; stop.parked {
			fields[MetaKeySubagentRunState] = AgentRunParked
			fields[MetaKeySubagentParkedCommands] = stop.commands
			if stop.reportID != "" {
				fields[MetaKeySubagentParkedReportID] = stop.reportID
				fields[MetaKeySubagentParkedReportPreview] = stop.preview
			}
		}
		served, err := mergeReadTimeMeta(item.Meta, fields)
		if err != nil {
			return fmt.Errorf("serve the run state of %s/%s: %w", item.ThreadID, item.ID, err)
		}
		items[i].Meta = served
	}
	return nil
}

// completionStatusSource reads the terminal source a completion sibling
// records (triage's backgroundCompletionItemMeta).
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
