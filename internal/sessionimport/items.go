package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
	"agent-overflow/internal/triage"
)

// items.go — the per-kind row builders. Every function here turns one
// imported event into the same store.Item (and optional payload) the
// live handler in internal/triage writes for its wire twin; the shared
// plumbing they call lives in writer.go.

// userText routes a user-role message to one of the two rows a live
// session produces for it. Triage splits on the same axis: a top-level
// prompt is an AO send whose row already exists (handleUserText stamps
// it), while a PARENTED one is a subagent's task text the wire echoed
// and triage persists fresh (persistWireOnlySubagentPrompt). The two
// have different ids, different metas, and different turn semantics.
func (b *builder) userText(evt importir.Event) error {
	if strings.TrimSpace(evt.Content) == "" {
		return nil
	}
	if scope := strings.TrimSpace(evt.ParentToolUseID); scope != "" {
		return b.subagentPrompt(evt, scope)
	}
	return b.prompt(evt)
}

// prompt builds the row a top-level user message owns. Claude's
// top-level user rows start turns; Codex opens its own with
// task_started, so an already-open turn that has produced nothing is
// this prompt's own.
func (b *builder) prompt(evt importir.Event) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	turnIndex := b.turns.startTurnForUserText(evt, now, b.itemIndex[b.turns.current()] > 0)
	b.closeStreams(turnIndex, "")

	seq := b.userSeq[turnIndex]
	b.userSeq[turnIndex]++
	itemID := fmt.Sprintf("user:%d", turnIndex)
	if seq > 0 {
		// Unreachable while every top-level prompt opens its own turn;
		// kept so an unforeseen provider ordering yields a second row
		// instead of a duplicate-id refusal of the whole import.
		itemID = fmt.Sprintf("user:%d:import:%d", turnIndex, seq)
	}
	// A user row's meta is AO-authored, not provider-authored: live, it is
	// the composer's usermessage blob plus the two wire-correlation keys
	// triage stamps from the echo. An imported prompt has no composer
	// blob, so the correlation keys are the whole meta — the event's own
	// meta (dedup ids, command-echo flags) is CLI bookkeeping that never
	// reaches a live row either.
	meta, err := mergeMetaKeys("", map[string]string{
		"provider_item_id":     stringsx.FirstNonEmptyTrimmed(metaString(evt.Meta, "provider_item_id"), evt.ItemID),
		"provider_parent_uuid": metaString(evt.Meta, "parent_uuid"),
		"command":              metaString(evt.Meta, "command"),
	})
	if err != nil {
		return fmt.Errorf("user text meta %s: %w", itemID, err)
	}
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindUserText,
		Role:      "user",
		Status:    statusCompleted,
		Summary:   evt.Content,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil)
	return err
}

// subagentPrompt builds the nested user_text row for a subagent's task
// prompt, keyed off the provider item id exactly as triage's
// persistWireOnlySubagentPrompt is. Without that id the live path
// refuses the row (nothing stable to key it on); so does this one, with
// a warning, because a synthesized id would collide across a refresh.
func (b *builder) subagentPrompt(evt importir.Event, scope string) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	providerItemID := stringsx.FirstNonEmptyTrimmed(metaString(evt.Meta, "provider_item_id"), evt.ItemID)
	if providerItemID == "" {
		b.warn("import.unkeyed-subagent-prompt", fmt.Sprintf(
			"a subagent prompt under %s carried no provider item id and was dropped", scope))
		return nil
	}
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, scope)

	openingPrompt := metaBool(evt.Meta, provider.MetaSubagentOpeningPromptKey)
	metaFields := map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
	}
	itemID := "user:wire:" + providerItemID
	if openingPrompt {
		metaFields[provider.MetaSubagentOpeningPromptKey] = true
		itemID = provider.SubagentOpeningPromptItemID(scope)
	}
	meta, err := json.Marshal(metaFields)
	if err != nil {
		return fmt.Errorf("encode subagent prompt meta for %s: %w", providerItemID, err)
	}
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindUserText,
		Role:      "user",
		Status:    statusCompleted,
		Summary:   evt.Content,
		ParentID:  scope,
		Meta:      string(meta),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil)
	return err
}

// proposedPlan builds the row a Codex `plan` item produces: a completed
// `tool_call` named "plan" carrying the plan markdown as its payload —
// the same shape triage's handleProposedPlan writes, including the
// fallback to the tool name when the plan has no title.
func (b *builder) proposedPlan(evt importir.Event) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, "")

	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		itemID = fmt.Sprintf("plan:%d", turnIndex)
	}
	payloadEvt := evt.ProviderEvent
	payloadEvt.Meta = b.providerMeta(evt)
	payloadMeta := triage.BuildPayloadMeta(payloadKindProposedPlan, payloadEvt)

	summary := proposedPlanTitle(payloadMeta)
	if summary == "" {
		summary = proposedPlanToolName
	}
	if existing, ok := b.byID[itemID]; ok {
		return b.replacePlanRow(evt, existing, itemID, summary, payloadMeta, now)
	}
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindToolCall,
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   summary,
		ToolName:  proposedPlanToolName,
		CreatedAt: now,
		UpdatedAt: now,
	}, &store.Payload{
		ID:        uuid.NewString(),
		Kind:      payloadKindProposedPlan,
		Meta:      payloadMeta,
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}, nil)
	return err
}

// replacePlanRow upserts a plan row a later `item_completed` re-names.
//
// Codex keys a turn's plan item as `<turnID>-plan` and persists a fresh
// completed snapshot under that SAME id every time the plan changes —
// a live session's item/completed is a one-shot upsert, so the later
// snapshot replaces the row rather than adding a sibling. Only a plan
// row may be replaced this way; any other kind under the id is the
// structural collision appendRow refuses.
func (b *builder) replacePlanRow(evt importir.Event, existing *row, itemID, summary, payloadMeta string, now int64) error {
	if existing.item.Kind != kindToolCall || existing.item.ToolName != proposedPlanToolName {
		return fmt.Errorf("item id %s already used by an earlier row", itemID)
	}
	source, err := sourceCoordinate(evt)
	if err != nil {
		return err
	}
	meta, err := itemmeta.MarkImported(existing.item.Meta, source)
	if err != nil {
		return fmt.Errorf("stamp provenance on %s: %w", itemID, err)
	}
	existing.item.Meta = meta
	existing.item.Summary = summary
	existing.item.UpdatedAt = now
	if existing.payload == nil {
		return fmt.Errorf("plan row %s has no payload to replace", itemID)
	}
	existing.payload.Meta = payloadMeta
	existing.payload.Data = []byte(evt.Content)
	existing.payload.CreatedAt = now
	b.turns.touch(existing.item.TurnIndex, now)
	return b.markUnavailable(evt, existing)
}

func proposedPlanTitle(payloadMeta string) string {
	var meta triage.ProposedPlanMeta
	if json.Unmarshal([]byte(payloadMeta), &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.Title)
}

func (b *builder) compaction(evt importir.Event) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, "")

	seq := b.compactSeq[turnIndex]
	b.compactSeq[turnIndex]++
	itemID := triage.CompactionItemID(turnIndex, strings.TrimSpace(evt.ItemID), seq)
	summary, rest := triage.ExtractCompactionSummary(b.providerMeta(evt))
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindCompaction,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Context compacted"),
		Meta:      string(rest),
		CreatedAt: now,
		UpdatedAt: now,
	}, triage.BuildCompactionPayload(summary, now), nil)
	return err
}

func (b *builder) providerError(evt importir.Event) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	scope := strings.TrimSpace(evt.ParentToolUseID)
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, scope)

	kind := kindError
	meta := ""
	if triage.APIErrorEnum(evt.Meta) != "" {
		kind = kindAPIError
		meta = b.providerMetaString(evt)
	}
	seq := b.errorSeq[scopeKey(turnIndex, scope)]
	b.errorSeq[scopeKey(turnIndex, scope)]++
	_, err = b.appendRow(evt, store.Item{
		ID:        triage.ErrorItemID(turnIndex, scope, seq),
		TurnIndex: turnIndex,
		Kind:      kind,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   triage.ClampErrorSummary(stringsx.FirstNonEmptyTrimmed(evt.Content, "Provider error")),
		ParentID:  scope,
		Meta:      meta,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil)
	return err
}

func (b *builder) notification(evt importir.Event) error {
	summary := strings.TrimSpace(evt.Content)
	if summary == "" {
		return nil
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	turnIndex := b.turns.currentFor(evt)
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		itemID = triage.NotificationItemID(turnIndex, b.notifySeq[turnIndex])
		b.notifySeq[turnIndex]++
	}
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindNotification,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   summary,
		ParentID:  strings.TrimSpace(evt.ParentToolUseID),
		ToolName:  stringsx.FirstNonEmptyTrimmed(evt.ItemType, "notification"),
		Meta:      b.providerMetaString(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil)
	return err
}

func (b *builder) commandResult(evt importir.Event) error {
	text := strings.TrimSpace(evt.Content)
	if text == "" {
		return nil
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	turnIndex := b.turns.currentFor(evt)
	seq := b.commandSeq[turnIndex]
	b.commandSeq[turnIndex]++
	itemID := triage.CommandResultItemID(turnIndex, strings.TrimSpace(evt.ItemID), seq)

	var resultMeta provider.CommandResultMeta
	if len(evt.Meta) > 0 {
		_ = json.Unmarshal(evt.Meta, &resultMeta)
	}
	shaped, err := triage.BuildCommandResultRowWithAgentResult(text, resultMeta.AgentResult)
	if err != nil {
		return fmt.Errorf("command result meta %s: %w", itemID, err)
	}
	var payload *store.Payload
	if shaped.Oversized {
		payloadEvt := evt.ProviderEvent
		payloadEvt.Meta = b.providerMeta(evt)
		payload = &store.Payload{
			ID:        commandResultPayloadID(itemID),
			Kind:      payloadKindCommandResult,
			Meta:      triage.BuildPayloadMeta(payloadKindCommandResult, payloadEvt),
			Data:      []byte(evt.Content),
			CreatedAt: now,
		}
	}
	_, err = b.appendRow(evt, store.Item{
		ID:        itemID,
		TurnIndex: turnIndex,
		Kind:      kindCommandResult,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   shaped.Summary,
		ParentID:  strings.TrimSpace(evt.ParentToolUseID),
		Meta:      shaped.Meta,
		CreatedAt: now,
		UpdatedAt: now,
	}, payload, nil)
	return err
}

// commandResultPayloadID keys the oversized-output blob off its row. The
// live path allocates a uuid there (attachPayloadToItem); an import has
// no reason to be non-deterministic, and the id is never compared across
// the two.
func commandResultPayloadID(itemID string) string {
	return "command-result-output:" + itemID
}
