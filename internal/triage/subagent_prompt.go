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

// persistResumePromptRow writes the message that opened a RESUMED round
// of one agent (claude-wire.md §E6). The rebind `system/task_started` is
// the only envelope carrying it and no later one repeats it, so without
// this row the resumed round opens with the agent's answer and no
// question.
//
// Identity is the CARRIER's scope, which is what keeps it distinct from
// the agent's round-1 opening prompt (the ROOT's scope). Placement is the
// ROOT's: a carrier is a lifecycle row and nothing is ever parented to it
// (transcript_root.go). The root is resolved here rather than trusted
// from the event, because on the live sequence this row arrives BEFORE
// the keep-running flip that fills Handle's carrier map.
//
// Provisional like the launch-input prompt: the terminal transcript
// replay later delivers the same text with its provider uuid, and
// persistWireOnlySubagentPrompt binds it onto this row in place.
func (r *Router) persistResumePromptRow(evt provider.ProviderEvent, meta userTextMeta) error {
	prompt := strings.TrimSpace(evt.Content)
	carrierID := strings.TrimSpace(meta.text(provider.MetaResumeCarrierIDKey))
	if carrierID == "" {
		carrierID = eventParentID(evt)
	}
	if prompt == "" || carrierID == "" {
		return nil
	}

	itemID := provider.SubagentOpeningPromptItemID(carrierID)
	if _, found, err := r.store.GetThreadItem(evt.ThreadID, itemID); err != nil {
		return fmt.Errorf("triage: inspect resume prompt %s/%s: %w", evt.ThreadID, itemID, err)
	} else if found {
		return nil
	}

	// The parser's own stamp first: it knew the original binding without
	// a lookup, and it is the one answer that does not depend on the
	// carrier row having been persisted yet. Falling back to resolving
	// through the carrier covers a rebind whose original launch the
	// parser never saw (a reconnect), where the persisted task_id is.
	parentID := strings.TrimSpace(meta.text(provider.MetaTranscriptRootIDKey))
	if parentID == "" {
		parentID = carrierID
		carrier, found, err := r.store.GetThreadItem(evt.ThreadID, carrierID)
		if err != nil {
			return fmt.Errorf("triage: resume prompt carrier lookup %s/%s: %w", evt.ThreadID, carrierID, err)
		}
		if found {
			root, resolved, rootErr := r.transcriptRoot(evt.ThreadID, carrier)
			if rootErr != nil {
				return rootErr
			}
			if resolved {
				parentID = root.ID
			}
		}
	}

	turnIndex, err := r.turnIndexForScope(evt.ThreadID, parentID)
	if err != nil {
		return fmt.Errorf("triage: resume prompt turn index %s/%s: %w", evt.ThreadID, parentID, err)
	}

	metaBytes, err := json.Marshal(map[string]any{
		"wire_only":                               true,
		provider.MetaSubagentResumePromptKey:      true,
		provider.MetaSubagentPromptProvisionalKey: true,
		provider.MetaResumeCarrierIDKey:           carrierID,
	})
	if err != nil {
		return fmt.Errorf("triage: encode resume prompt meta: %w", err)
	}
	now := eventTimestampMillis(evt)
	return r.persistItem(store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindUserText,
		Role:      "user",
		Status:    statusCompleted,
		Summary:   prompt,
		ParentID:  parentID,
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

// subagentScopedPromptState decodes the three facts every launch-scoped
// prompt row carries: whether it is one at all (an OPENING prompt for the
// agent, or the prompt that opened a resumed ROUND — both are scoped rows
// whose provider uuid arrives later), whether it is still provisional,
// and the uuid once bound.
func subagentScopedPromptState(meta string) (scoped, provisional bool, providerItemID string, err error) {
	if strings.TrimSpace(meta) == "" {
		return false, false, "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(meta), &fields); err != nil {
		return false, false, "", err
	}
	decode := userTextMeta(fields)
	return decode.flag(provider.MetaSubagentOpeningPromptKey) || decode.flag(provider.MetaSubagentResumePromptKey),
		decode.flag(provider.MetaSubagentPromptProvisionalKey),
		decode.text("provider_item_id"), nil
}

// subagentOpeningPromptState answers the same question for the agent's
// OPENING prompt specifically — the row whose identity is the launch
// scope, which persistWireOnlySubagentPrompt claims by id.
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
