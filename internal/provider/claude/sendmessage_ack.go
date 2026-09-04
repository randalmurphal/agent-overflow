package claude

import (
	"encoding/json"
	"strings"
)

// SendMessageAck is the structured reply Claude's `SendMessage` tool
// returns in the `tool_use_result` sibling of its tool_result
// (claude-wire.md §"`SendMessage` ack", verified 2.1.257).
//
// The tool_result block itself is NEVER flagged `is_error`: a refused
// send ("No agent named 'A' is reachable") is an ordinary result whose
// `success` is false. A consumer that reads only the block's flag files
// every refusal as a completed send, which is how five undeliverable
// messages rendered identically to five delivered ones (2026-09-04).
type SendMessageAck struct {
	Success bool
	// Reply is the one line the CLI's own TUI prints under the row: the
	// `display` string when the ack carries one (refusals, 2.1.257+),
	// else `message`. Newlines are the caller's to collapse.
	Reply string
	// AgentID is the in-process agent the CLI resolved the recipient to:
	// `pin.id` (2.1.257+) or `resumedAgentId` (§E6). Empty for a peer
	// session, a teammate mailbox, or a refused send.
	AgentID string
}

// DecodeSendMessageAck reads a SendMessage `tool_use_result`. False when
// the sibling is absent, is not an object, or carries no boolean
// `success` — the one field every ack shape since 2.1.88 has.
func DecodeSendMessageAck(raw json.RawMessage) (SendMessageAck, bool) {
	if len(raw) == 0 {
		return SendMessageAck{}, false
	}
	var payload struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Display string `json:"display"`
		Pin     struct {
			ID string `json:"id"`
		} `json:"pin"`
		ResumedAgentID string `json:"resumedAgentId"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Success == nil {
		return SendMessageAck{}, false
	}
	reply := strings.TrimSpace(payload.Display)
	if reply == "" {
		reply = strings.TrimSpace(payload.Message)
	}
	agentID := strings.TrimSpace(payload.Pin.ID)
	if agentID == "" {
		agentID = strings.TrimSpace(payload.ResumedAgentID)
	}
	return SendMessageAck{Success: *payload.Success, Reply: reply, AgentID: agentID}, true
}
