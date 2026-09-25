package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// persistProvisionalSubagentPrompt writes the launch input as the first row in
// an agent's scoped transcript. Claude does not echo an async agent's opening
// user row on ordinary stdout, so waiting for the session mirror would
// append the prompt after the work it caused.
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
// Provisional like the launch-input prompt: the session mirror later
// delivers the same text with its provider uuid, and
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
	parentID, err := r.promptScopeRoot(evt.ThreadID, strings.TrimSpace(meta.text(provider.MetaTranscriptRootIDKey)), carrierID)
	if err != nil {
		return err
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

// persistWakePromptRow records a PARKED agent being woken (claude-wire.md
// §E6b): the `<task-notification>` the CLI resumed it with when one of
// its owned background shells reported. Two effects, in this order:
//
//  1. The completed terminal stashed at the agent's stop is DROPPED. That
//     stop was a pause, not the end; settling the stash would put the
//     agent's card at a stop the wake just undid, and leaving it would
//     let the next terminal's drain read a stale end time. The round's
//     own terminal stashes afresh.
//  2. The prompt lands as a user-role row under the transcript ROOT, the
//     way the §E6 resume message does, so the woken round opens with what
//     the agent was told rather than with its answer. Placement resolves
//     like persistResumePromptRow (promptScopeRoot): the parser's
//     `transcript_root_id` stamp first, else the bound row. Nothing
//     ever binds a provider uuid onto this row (the sidechain records the
//     wake as an `isMeta` row the converter drops), so it is not
//     provisional, and it carries the wake marker rather than the resume
//     one because the store's round slicing keys resume rows on a
//     carrier this round does not have.
//
// The stash drop runs even when the prompt is empty: liveness is the
// load-bearing half. An existing row (a re-delivered envelope) is left
// alone. The reaper and the workspace lock read the stash, and the tray
// reads the wake row (Store.CurrentParkedStop), so either change nudges
// them once both are written.
func (r *Router) persistWakePromptRow(evt provider.ProviderEvent, meta userTextMeta) error {
	var dropErr error
	dropped := false
	if taskID := strings.TrimSpace(meta.text("task_id")); taskID != "" {
		_, took, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, taskID)
		if err != nil {
			dropErr = fmt.Errorf("triage: drop parked terminal on wake %s/%s: %w", evt.ThreadID, taskID, err)
		}
		dropped = took
	}
	wrote, err := r.writeWakePromptRow(evt, meta)
	if dropped || wrote {
		r.emitBackgroundTasksChangedNudge(evt.ThreadID)
	}
	return errors.Join(dropErr, err)
}

// writeWakePromptRow writes the wake's prompt row and reports whether it
// wrote one.
func (r *Router) writeWakePromptRow(evt provider.ProviderEvent, meta userTextMeta) (bool, error) {
	prompt := strings.TrimSpace(evt.Content)
	itemID := strings.TrimSpace(evt.ItemID)
	parentID := eventParentID(evt)
	if prompt == "" || itemID == "" || parentID == "" {
		return false, nil
	}
	if _, found, err := r.store.GetThreadItem(evt.ThreadID, itemID); err != nil {
		return false, fmt.Errorf("triage: inspect wake prompt %s/%s: %w", evt.ThreadID, itemID, err)
	} else if found {
		return false, nil
	}

	rootID, err := r.promptScopeRoot(evt.ThreadID, strings.TrimSpace(meta.text(provider.MetaTranscriptRootIDKey)), parentID)
	if err != nil {
		return false, err
	}
	turnIndex, err := r.turnIndexForScope(evt.ThreadID, rootID)
	if err != nil {
		return false, fmt.Errorf("triage: wake prompt turn index %s/%s: %w", evt.ThreadID, rootID, err)
	}

	fields := map[string]any{
		"wire_only":                        true,
		provider.MetaSubagentWakePromptKey: true,
	}
	for _, key := range []string{"task_id", provider.MetaWakeTaskIDKey, provider.MetaWakeToolUseIDKey, provider.MetaWakeStatusKey} {
		if value := strings.TrimSpace(meta.text(key)); value != "" {
			fields[key] = value
		}
	}
	metaBytes, err := json.Marshal(fields)
	if err != nil {
		return false, fmt.Errorf("triage: encode wake prompt meta: %w", err)
	}
	// The wake follows the stop it wakes from, the task's newest: the
	// launch's, or a §E6 carrier's, whose stop does not complete the root
	// the wake is filed under. The tray and the cards order the two by
	// creation time (Store.CurrentParkedStop, the frontend's per-stop card
	// rows), so the wake is written after the stop's millisecond.
	now := eventTimestampMillis(evt)
	stop, stopped, err := r.store.NewestTaskStop(evt.ThreadID, strings.TrimSpace(meta.text("task_id")))
	if err != nil {
		return false, err
	}
	if stopped && stop.CreatedAt >= now {
		now = stop.CreatedAt + 1
	}
	err = r.persistItem(store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindUserText,
		Role:      "user",
		Status:    statusCompleted,
		Summary:   prompt,
		ParentID:  rootID,
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
	return err == nil, err
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
