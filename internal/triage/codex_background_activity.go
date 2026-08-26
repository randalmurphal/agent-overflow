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

// persistCodexMailboxProgress writes one child -> parent MESSAGE delivery at
// the current timeline write head. The spawn row remains only the visible
// launch record; later communication is its own activity item.
func (r *Router) persistCodexMailboxProgress(
	evt provider.ProviderEvent,
	launch persistedCodexSpawnLaunch,
	parsed codexSubagentSignalMeta,
) error {
	digest := sha256.Sum256([]byte(
		strings.TrimSpace(launch.item.ID) + "\x00" +
			strings.TrimSpace(parsed.AgentPath) + "\x00" + strings.TrimSpace(parsed.DeliveryID),
	))
	itemID := fmt.Sprintf("collab-progress:%x", digest[:8])
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.item.TurnIndex)
	if err != nil {
		return fmt.Errorf("codex progress turn index %s: %w", itemID, err)
	}
	now := eventTimestampMillis(evt)
	meta, err := codexMailboxProgressItemMeta(launch.item.Meta, parsed)
	if err != nil {
		return fmt.Errorf("codex progress item meta %s: %w", itemID, err)
	}
	return r.persistItem(store.Item{
		ID:        itemID,
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
	}, nil)
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
