package triage

import (
	"encoding/json"
	"log"
	"strings"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// Claude's SendMessage row reads its verdict off the ack, not off the
// tool_result flag (claude-wire.md §"`SendMessage` ack"): the CLI
// answers a refused send with a normal `is_error:false` result whose
// `tool_use_result.success` is false. These are the keys the row
// persists so the frontend can show what the CLI's own TUI shows — the
// one-line reply, red when refused, and the recipient by name.
const (
	// MetaSendReplyKey is the CLI's one-line reply, newline-collapsed and
	// capped like every other row preview.
	MetaSendReplyKey = "send_reply"
	// MetaRecipientTaskIDKey is the agent the CLI resolved the recipient
	// to (`pin.id`), when it was an agent of this session.
	MetaRecipientTaskIDKey = "recipient_task_id"
	// MetaRecipientDescriptionKey is that agent's launch description —
	// what the tray and the agents panel call it — resolved at persist
	// time because the launch row is rarely in the same loaded window as
	// the send (Core Principle 4).
	MetaRecipientDescriptionKey = "recipient_description"

	sendReplyPreviewMax = 240
)

// ApplySendMessageAck reads Claude's SendMessage ack off a decoded
// completion envelope and returns the meta patch the row persists, or
// nil for any other tool and for an ack that does not decode. On a
// refusal it flips meta.IsError so status, summary suffix and the payload
// header all carry the same verdict; the patch restates `is_error` so
// the stored meta agrees with the row after the wire's own false flag
// has been merged in. Shared by the live completion path and the session
// importer, which build rows from the same envelope.
func ApplySendMessageAck(toolName string, obj map[string]json.RawMessage, meta *ToolCompleteMeta) map[string]json.RawMessage {
	if strings.TrimSpace(toolName) != "SendMessage" || obj == nil || meta == nil {
		return nil
	}
	ack, ok := claude.DecodeSendMessageAck(obj["tool_use_result"])
	if !ok {
		return nil
	}
	patch := make(map[string]json.RawMessage, 3)
	if !ack.Success {
		meta.IsError = true
		patch["is_error"] = json.RawMessage("true")
	}
	if reply := truncatePreview(ack.Reply, sendReplyPreviewMax); reply != "" {
		patch[MetaSendReplyKey] = jsonString(reply)
	}
	if ack.AgentID != "" {
		patch[MetaRecipientTaskIDKey] = jsonString(ack.AgentID)
	}
	return patch
}

// SendMessageRecipientTaskID is the agent a SendMessage row should be
// resolved against: the ack's pin when the CLI named one, else the
// row's own `input.to` (older CLIs ack a queued send without a pin, and
// the model addresses an agent by its id). The caller's lookup is what
// validates the fallback — a `to` that matches no launch in this thread
// is a peer session or a name, and stays as typed.
func SendMessageRecipientTaskID(patch map[string]json.RawMessage, launchMeta string) string {
	if raw, ok := patch[MetaRecipientTaskIDKey]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil && id != "" {
			return id
		}
	}
	var input struct {
		To string `json:"to"`
	}
	if in := DecodeToolStartMeta(json.RawMessage(launchMeta)).Input; len(in) > 0 {
		_ = json.Unmarshal(in, &input)
	}
	return strings.TrimSpace(input.To)
}

// stampSendMessageRecipient resolves the recipient agent's launch in
// this thread and adds its description to the patch. A miss (a peer
// session, a name, a pruned launch) leaves the patch as it was and the
// row shows the recipient as typed.
func (r *Router) stampSendMessageRecipient(threadID string, launch store.Item, patch map[string]json.RawMessage) {
	taskID := SendMessageRecipientTaskID(patch, launch.Meta)
	if taskID == "" {
		return
	}
	original, found, err := r.store.FindOriginalAgentLaunchByTaskID(threadID, taskID, launch.ID)
	if err != nil {
		// Cosmetic loss, never a failed completion.
		log.Printf("triage: resolve SendMessage recipient %s on %s/%s: %v", taskID, threadID, launch.ID, err)
		return
	}
	if found {
		StampSendMessageRecipient(patch, taskID, original.Meta)
	}
}

// StampSendMessageRecipient adds the resolved recipient launch's
// description (and the task id it resolved through) to a SendMessage
// row's meta patch. Pure: the caller found `launchMeta` — the Router in
// the store, the session importer in its own batch — and this is the one
// spelling of what gets stamped. A launch with no description stamps
// nothing, so the row falls back to the recipient as typed.
func StampSendMessageRecipient(patch map[string]json.RawMessage, taskID, launchMeta string) {
	desc := launchInputIdentity(launchMeta).Description
	if desc == "" || taskID == "" {
		return
	}
	patch[MetaRecipientDescriptionKey] = jsonString(truncatePreview(desc, 80))
	patch[MetaRecipientTaskIDKey] = jsonString(taskID)
}

func jsonString(s string) json.RawMessage {
	encoded, _ := json.Marshal(s)
	return encoded
}
