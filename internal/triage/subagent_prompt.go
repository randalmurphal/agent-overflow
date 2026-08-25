package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// persistProvisionalSubagentPrompt writes the launch input as the first row in
// an agent's scoped transcript. Claude does not echo an async agent's opening
// user row on ordinary stdout, so waiting for terminal transcript recovery
// would append the prompt after the work it caused.
//
// The launch scope is the row identity. The real transcript row later updates
// this item with its provider uuid, and session import uses the same identity.
func (r *Router) persistProvisionalSubagentPrompt(launch store.Item, meta ToolStartMeta, now int64) error {
	if !meta.SubagentLaunch {
		return nil
	}
	prompt, err := subagentPromptFromInput(meta.Input)
	if err != nil {
		return fmt.Errorf("triage: decode subagent launch prompt %s/%s: %w", launch.ThreadID, launch.ID, err)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil
	}

	itemID := provider.SubagentOpeningPromptItemID(launch.ID)
	if _, found, err := r.store.GetThreadItem(launch.ThreadID, itemID); err != nil {
		return fmt.Errorf("triage: inspect provisional subagent prompt %s/%s: %w", launch.ThreadID, itemID, err)
	} else if found {
		return nil
	}

	metaBytes, err := json.Marshal(map[string]any{
		"wire_only":                               true,
		provider.MetaSubagentOpeningPromptKey:     true,
		provider.MetaSubagentPromptProvisionalKey: true,
	})
	if err != nil {
		return fmt.Errorf("triage: encode provisional subagent prompt meta: %w", err)
	}
	return r.persistItem(store.Item{
		ID:        itemID,
		ThreadID:  launch.ThreadID,
		TurnIndex: launch.TurnIndex,
		Kind:      itemKindUserText,
		Role:      "user",
		Status:    statusCompleted,
		Summary:   prompt,
		ParentID:  launch.ID,
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
}

func subagentPromptFromInput(input json.RawMessage) (string, error) {
	if len(input) == 0 {
		return "", nil
	}
	var decoded struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &decoded); err != nil {
		return "", err
	}
	return decoded.Prompt, nil
}

func subagentOpeningPromptState(meta string) (opening, provisional bool, providerItemID string, err error) {
	if strings.TrimSpace(meta) == "" {
		return false, false, "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(meta), &fields); err != nil {
		return false, false, "", err
	}
	decode := userTextMeta(fields)
	return decode.flag(provider.MetaSubagentOpeningPromptKey),
		decode.flag(provider.MetaSubagentPromptProvisionalKey),
		decode.text("provider_item_id"), nil
}
