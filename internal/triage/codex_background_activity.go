package triage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// decodeCodexChildResumeGenerations reads the per-child turn counter kept on
// the launch for mailbox-delivery identity. It is operational correlation, not
// presentation state: later answers from the same reusable child must remain
// distinct even when their text is byte-identical.
func decodeCodexChildResumeGenerations(raw json.RawMessage) map[string]int {
	var parsed struct {
		Generations map[string]int `json:"codex_child_resume_generations"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil || parsed.Generations == nil {
		return make(map[string]int)
	}
	return parsed.Generations
}

const codexCollabProgressTextRunes = 240

func codexCollabProgressText(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	runes := make([]rune, 0, codexCollabProgressTextRunes)
	truncated := false
	for _, value := range line {
		if len(runes) == codexCollabProgressTextRunes {
			truncated = true
			break
		}
		runes = append(runes, value)
	}
	bounded := strings.TrimSpace(string(runes))
	if !truncated {
		return bounded
	}
	return bounded + "\u2026"
}

// persistCodexMailboxProgress writes one child -> parent mailbox delivery at
// the current timeline write head. The spawn row remains only the visible
// launch record; later communication is its own activity item.
func (r *Router) persistCodexMailboxProgress(
	evt provider.ProviderEvent,
	launch persistedCodexSpawnLaunch,
	parsed codexSubagentSignalMeta,
) error {
	identityScope := strings.TrimSpace(launch.item.ID)
	if strings.HasPrefix(parsed.DeliveryID, "item:") {
		identityScope = parsed.Recipient
	}
	digest := sha256.Sum256([]byte(
		identityScope + "\x00" +
			strings.TrimSpace(parsed.AgentPath) + "\x00" + strings.TrimSpace(parsed.DeliveryID),
	))
	itemID := fmt.Sprintf("collab-progress:%x", digest[:8])
	if _, found, err := r.store.GetThreadItem(evt.ThreadID, itemID); err != nil {
		return err
	} else if found {
		return nil
	}
	// The progress row is top-level (no ParentID below), so it always
	// follows the write head.
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.item.TurnIndex, "")
	if err != nil {
		return fmt.Errorf("codex progress turn index %s: %w", itemID, err)
	}
	now := eventTimestampMillis(evt)
	meta, err := codexMailboxProgressItemMeta(launch.item.Meta, parsed)
	if err != nil {
		return fmt.Errorf("codex progress item meta %s: %w", itemID, err)
	}
	content := parsed.Message
	if parsed.Encrypted {
		content = codexEncryptedMessagePlaceholder
	}
	payload := completionPayload(itemID, provider.ProviderEvent{Content: content}, ToolCompleteMeta{}, now)
	payloadID := ""
	if payload != nil {
		payloadID = payload.ID
	}
	return r.persistItem(store.Item{
		ID:        itemID,
		PayloadID: payloadID,
		ParentID:  evt.ParentToolUseID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   "Progress reported",
		ToolName:  "send_input",
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}, payload)
}

func codexMailboxProgressItemMeta(launchMeta string, parsed codexSubagentSignalMeta) (string, error) {
	var launch struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if strings.TrimSpace(launchMeta) != "" {
		if err := json.Unmarshal([]byte(launchMeta), &launch); err != nil {
			return "", err
		}
	}
	input := map[string]json.RawMessage{
		"tool":         json.RawMessage(`"send_input"`),
		"activityKind": json.RawMessage(`"progress"`),
	}
	target, err := json.Marshal(parsed.AgentPath)
	if err != nil {
		return "", err
	}
	messageType, err := json.Marshal(parsed.MessageType)
	if err != nil {
		return "", err
	}
	input["target"], input["messageType"] = target, messageType
	for _, key := range []string{
		"receiverThreadIds", "receiverAgents", "newAgentNickname", "newAgentRole",
		"agentNickname", "agentRole", "agentPath", "taskName",
	} {
		if value, ok := launch.Input[key]; ok {
			input[key] = value
		}
	}
	if message := codexCollabProgressText(parsed.Message); message != "" {
		encoded, err := json.Marshal(message)
		if err != nil {
			return "", err
		}
		input["message"] = encoded
	}
	encoded, err := json.Marshal(map[string]any{
		"toolName": "send_input",
		"input":    input,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// persistCodexReceivedMessage records the observed delivery in its recipient's
// scope. wire_only prevents edit/resend from treating it as human input.
func (r *Router) persistCodexReceivedMessage(evt provider.ProviderEvent, parsed codexSubagentSignalMeta) error {
	parent := strings.TrimSpace(evt.ParentToolUseID)
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, parent)
	if err != nil {
		return err
	}
	if !found || launch.ToolName != "collab_agent" {
		return fmt.Errorf("Codex message recipient launch %q is unavailable", parent)
	}
	digest := sha256.Sum256([]byte(parent + "\x00" + parsed.AgentPath + "\x00" + parsed.DeliveryID))
	id := fmt.Sprintf("agent-message:%x", digest[:16])
	if _, found, err := r.store.GetThreadItem(evt.ThreadID, id); err != nil {
		return err
	} else if found {
		return nil
	}
	index, err := r.turnIndexForScope(evt.ThreadID, parent)
	if err != nil {
		return err
	}
	body := parsed.Message
	if body == "" {
		body = "Empty message."
	}
	if parsed.Encrypted {
		body = codexEncryptedMessagePlaceholder
	}
	meta, err := json.Marshal(map[string]any{"wire_only": true, "agent_message": map[string]any{"sender": parsed.AgentPath, "recipient": parsed.Recipient, "messageType": parsed.MessageType, "encrypted": parsed.Encrypted, "deliveryId": parsed.DeliveryID}})
	if err != nil {
		return err
	}
	now := eventTimestampMillis(evt)
	return r.persistItem(store.Item{ID: id, ThreadID: evt.ThreadID, TurnIndex: index, ParentID: parent, Kind: itemKindUserText, Role: "user", Status: statusCompleted, Summary: body, Meta: string(meta), CreatedAt: now, UpdatedAt: now}, nil)
}

// codexEncryptedMessagePlaceholder stands in for a body Codex encrypted:
// the delivery row, the recipient-scope message and the completion
// payload all show the same words for the same envelope.
const codexEncryptedMessagePlaceholder = "Message content is encrypted by Codex."
