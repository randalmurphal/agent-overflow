package triage

import (
	"encoding/json"
	"fmt"
	"strings"
)

// codexBackgroundCompletionStatus maps the completing item's item_status
// meta key onto the canonical item status enum. A unifiedExec command
// that yielded and later closed either completed cleanly or failed; the
// wire uses CommandExecutionStatus (inProgress | completed | failed).
// An absent item_status (e.g. synthesized for spawn_agent after a
// subagent_notification) defaults to completed.
func codexBackgroundCompletionStatus(meta json.RawMessage) string {
	decoded := decodeCodexItemMeta(meta)
	switch decoded.ItemStatus {
	case "failed", "errored":
		return statusErrored
	case "killed":
		return statusKilled
	default:
		return statusCompleted
	}
}

// buildCodexBackgroundCompletionSummary produces the sibling row's
// summary. Prefers the launch summary followed by a short outcome
// marker (`-> done`, `-> failed`) so the tray stays readable. Falls
// back to a bare "done" when no launch summary is available.
func buildCodexBackgroundCompletionSummary(launchSummary string, meta json.RawMessage) string {
	outcome := codexBackgroundOutcome(meta)
	summary := strings.TrimSpace(launchSummary)
	if outcome == "" {
		if summary == "" {
			return "done"
		}
		return summary
	}
	if summary == "" {
		return outcome
	}
	return summary + " -> " + outcome
}

func codexBackgroundOutcome(meta json.RawMessage) string {
	decoded := decodeCodexItemMeta(meta)
	switch decoded.ItemStatus {
	case "failed":
		return "failed"
	case "errored":
		return "errored"
	case "killed":
		return "killed"
	case "completed":
		return "done"
	default:
		return ""
	}
}

// codexItemMeta is the subset of a Codex EventToolStart /
// EventToolComplete Meta blob that the projector needs. See
// protocol.enrichItemMeta for the source of each field.
type codexItemMeta struct {
	Source            string
	ItemStatus        string
	ProcessID         string
	Tool              string
	AgentsStates      map[string]json.RawMessage
	ReceiverThreadIDs []string
}

// decodeCodexItemMeta pulls the projector-relevant fields out of a
// wire-enriched Meta blob. Malformed JSON returns the zero value rather
// than bubbling the error — the projector's behaviour on an empty meta
// is "no wire-typed signal, skip", which is the correct fallback for a
// garbled envelope. Upstream validation (`enrichItemMeta` in the Codex
// parser) already produces well-formed JSON; a decode failure here
// would indicate a corrupt event that couldn't have been routed
// correctly anyway.
func decodeCodexItemMeta(raw json.RawMessage) codexItemMeta {
	if len(raw) == 0 {
		return codexItemMeta{}
	}
	var shell struct {
		Source     string          `json:"source"`
		ItemStatus string          `json:"item_status"`
		ProcessID  string          `json:"process_id"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return codexItemMeta{}
	}
	out := codexItemMeta{
		Source:     shell.Source,
		ItemStatus: shell.ItemStatus,
		ProcessID:  shell.ProcessID,
	}
	if len(shell.Input) == 0 {
		return out
	}
	var input struct {
		Tool              string                     `json:"tool"`
		AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
		ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
	}
	if err := json.Unmarshal(shell.Input, &input); err != nil {
		return out
	}
	out.Tool = input.Tool
	out.AgentsStates = input.AgentsStates
	out.ReceiverThreadIDs = input.ReceiverThreadIDs
	return out
}

// hasRunningChild reports whether agentsStates contains at least one
// child in a non-terminal state. The agentsStates map keys are child
// thread ids; values are CollabAgentStatus variants ("pendingInit" |
// "running" | "interrupted" | "completed" | "errored" | "shutdown" |
// "notFound"). Running or pendingInit count as non-terminal; anything
// else is terminal. The value may be either a bare string ("running") or
// an object ({status: "running"}) — Codex ships both shapes depending on
// wire version.
func hasRunningChild(states map[string]json.RawMessage) bool {
	for _, raw := range states {
		status := extractAgentStatus(raw)
		switch status {
		case "running", "pendingInit":
			return true
		}
	}
	return false
}

// extractAgentStatus pulls the status string from an agentsStates entry.
// Accepts both bare-string ("running") and object ({status: "running"})
// shapes — older wire sends strings, v2 sends nested objects.
func extractAgentStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	var obj struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Status
	}
	return ""
}

func extractAgentMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return strings.TrimSpace(obj.Message)
}

func formatAgentCompletionMessages(results []agentTerminalResult, totalChildren int) string {
	messages := make([]string, 0, len(results))
	for _, result := range results {
		if formatted := formatAgentCompletionMessage(result, totalChildren); formatted != "" {
			messages = append(messages, formatted)
		}
	}
	return strings.Join(messages, "\n\n")
}

func formatAgentCompletionMessage(result agentTerminalResult, totalChildren int) string {
	message := result.message
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	if totalChildren <= 1 {
		return message
	}
	ordinal := result.ordinal
	if ordinal <= 0 {
		ordinal = 1
	}
	header := fmt.Sprintf("Agent %d", ordinal)
	if strings.TrimSpace(result.status) != "" {
		header += " (" + result.status + ")"
	}
	return header + ":\n" + message
}

// codexSubagentSignalMeta is the common shape for Codex child-agent terminal
// signals. <subagent_notification> includes Message and can create transcript
// completion once every child in the spawn is terminal. EventSubagentStatus is
// status-only live-state evidence and does not synthesize transcript rows.
type codexSubagentSignalMeta struct {
	AgentPath string `json:"agent_path"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	// MessageType is the mailbox envelope header (FINAL_ANSWER | MESSAGE).
	// FINAL_ANSWER is the transcript-completion boundary; MESSAGE is a
	// mid-run progress note that must never mark a child terminal.
	MessageType     string `json:"message_type"`
	MailboxDelivery bool   `json:"mailbox_delivery"`
	DeliveryID      string `json:"delivery_id"`
}

// isCodexMailboxProgressDelivery reports whether this delivery is a `MESSAGE`
// progress note rather than the child's `FINAL_ANSWER`. Wire-typed on the
// envelope header only (invariant 25) — never inferred from what followed.
func (m codexSubagentSignalMeta) isCodexMailboxProgressDelivery() bool {
	return m.MailboxDelivery && strings.TrimSpace(m.MessageType) == "MESSAGE"
}

func decodeCodexSubagentSignalMeta(raw json.RawMessage) codexSubagentSignalMeta {
	if len(raw) == 0 {
		return codexSubagentSignalMeta{}
	}
	var m codexSubagentSignalMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return codexSubagentSignalMeta{}
	}
	return m
}

func decodeCodexChildTerminalStatuses(raw json.RawMessage) map[string]string {
	var parsed struct {
		Statuses map[string]string `json:"codex_child_terminal_statuses"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil || parsed.Statuses == nil {
		return make(map[string]string)
	}
	return parsed.Statuses
}

func allCodexSpawnChildrenTerminal(receiverThreadIDs []string, terminalStatuses map[string]string) bool {
	if len(receiverThreadIDs) == 0 {
		return false
	}
	for _, childID := range receiverThreadIDs {
		if strings.TrimSpace(terminalStatuses[childID]) == "" {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
