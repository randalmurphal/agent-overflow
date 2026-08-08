package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
	"agent-overflow/internal/triage"
)

const toolResultPayloadKind = "tool_result"

// toolStart builds (or re-annotates) a tool_call launch row. A tool call
// is ONE row across its whole life: the launch, any rich result payload,
// and the completion all land on the same item, exactly as the live
// upsert path leaves it.
func (b *builder) toolStart(evt importir.Event) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return fmt.Errorf("tool start carries no item id")
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	meta := triage.DecodeToolStartMeta(evt.Meta)
	scope := stringsx.FirstNonEmptyTrimmed(evt.ParentToolUseID, meta.ParentToolUseID)
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, scope)

	cleaned := b.providerMeta(evt)
	if existing, found := b.byID[itemID]; found {
		if existing.item.Kind != kindToolCall {
			b.warn("import.tool-id-collision", fmt.Sprintf(
				"tool start %s collides with a %s row and was dropped", itemID, existing.item.Kind))
			return nil
		}
		// A later signal annotating an already-built launch (Claude's
		// system/task_started task_id mapping, a subagent model stamp,
		// Codex child-thread labels). Status and payloads are preserved.
		if name := strings.TrimSpace(meta.ToolName); name != "" {
			existing.item.ToolName = name
			existing.item.Summary = triage.BuildToolCallSummary(meta, evt.ItemType)
		}
		if existing.item.ParentID == "" {
			existing.item.ParentID = scope
		}
		existing.item.IsBackground = existing.item.IsBackground || meta.IsBackground
		existing.item.Meta = triage.MergeStoredToolCallMeta(existing.item.Meta, evt.ItemType, cleaned)
		existing.item.UpdatedAt = now
		b.shapeToolMeta(existing, now)
		if err := b.applyFileChangeResult(evt, existing, now); err != nil {
			return err
		}
		return b.markUnavailable(evt, existing)
	}

	item := store.Item{
		ID:           itemID,
		TurnIndex:    turnIndex,
		Kind:         kindToolCall,
		Role:         "assistant",
		Status:       statusRunning,
		Summary:      triage.BuildToolCallSummary(meta, evt.ItemType),
		ParentID:     scope,
		IsBackground: meta.IsBackground,
		ToolName:     stringsx.FirstNonEmptyTrimmed(meta.ToolName, evt.ItemType, "tool"),
		Meta:         triage.StoredToolCallMeta(evt.ItemType, cleaned),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	inputPayload := b.shapeItemToolMeta(&item, now)
	r, err := b.appendRow(evt, item, nil, inputPayload)
	if err != nil {
		return err
	}
	return b.applyFileChangeResult(evt, r, now)
}

// toolComplete settles the launch row its tool_use id names.
//
// A completion with no launch and no marker is refused: the session
// file's tool lifecycle is the one thing an import cannot reconstruct,
// and inventing a launch row would put a tool call in the timeline that
// the provider never reported starting.
//
// A completion carrying `import_unavailable` is the one exception, and it
// is not a guess — the reader has already established that the launch is
// outside the range being imported (a fork's inherited prefix, a
// compaction that replaced the window, a tail refresh that starts after
// the call) and said so on the event. That marker exists precisely to
// produce a placeholder row the frontend renders as "Not available from
// import"; failing the whole session over one visible gap would be the
// worse answer, and it is what the Codex reader's orphan completions
// used to do.
func (b *builder) toolComplete(evt importir.Event) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return fmt.Errorf("tool completion carries no item id")
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	r, found := b.byID[itemID]
	if !found {
		if importUnavailableReason(evt.Meta) == "" {
			return fmt.Errorf("tool completion %s has no launch in this session", itemID)
		}
		if r, err = b.placeholderToolLaunch(evt, itemID, now); err != nil {
			return err
		}
	}
	if r.item.Kind != kindToolCall {
		return fmt.Errorf("tool completion %s targets a %s row", itemID, r.item.Kind)
	}
	meta := triage.DecodeToolCompleteMeta(evt.Meta)
	b.closeStreams(r.item.TurnIndex, r.item.ParentID)

	// Rich result payload first, so the completion's summary derivation
	// sees the payload-derived label — the live ordering in
	// handleToolComplete, and what makes an Edit row read "Edited x.go"
	// rather than "Edit: /abs/path".
	if err := b.applyFileChangeResult(evt, r, now); err != nil {
		return err
	}

	if b.splitsCompletion(r.item.ToolName) {
		return b.splitToolCompletion(evt, r, meta, now)
	}
	if found && (r.item.IsBackground || meta.IsBackground) {
		// A backgrounded launch's tool_result is a placeholder: the real
		// terminal arrives later as EventBackgroundTaskTerminal, which
		// writes the sibling row. Keep the launch running.
		//
		// `found` is what keeps that from stranding a row. A SYNTHESIZED
		// launch (the `import_unavailable` orphan above) exists only
		// because the real launch is outside the imported range — so the
		// terminal that would settle it is outside too, and nothing else
		// can: the turn-boundary force-close exempts backgrounded rows by
		// design (invariant 24), which would leave the card spinning for
		// the life of the thread. A placeholder therefore always settles
		// on the completion that created it. A terminal that DOES arrive
		// later still lands, because it settles the row by id and does not
		// care what status it was left in.
		r.item.IsBackground = true
		r.item.UpdatedAt = now
		return b.markUnavailable(evt, r)
	}

	cleaned := b.providerMeta(evt)
	r.item.Status = triage.CompletionStatus(meta)
	r.item.Summary = triage.BuildCompletionSummary(
		triage.CompletionBaseSummary(r.item, meta, evt.ItemType), meta)
	if name := strings.TrimSpace(meta.ToolName); name != "" {
		r.item.ToolName = name
	}
	r.item.Meta = triage.MergeStoredToolCallMeta(r.item.Meta, evt.ItemType, cleaned)
	r.item.UpdatedAt = now
	b.shapeToolMeta(r, now)

	resultEvt := evt.ProviderEvent
	resultEvt.Meta = cleaned
	payload := triage.CompletionPayloadForTool(
		r.item.ID, r.item.ToolName, triage.CommandFromLaunch(r.item), resultEvt, meta, now)
	switch {
	case payload == nil:
	case r.payload == nil:
		r.payload = payload
		r.item.PayloadID = payload.ID
	case r.payload.Kind == payloadKindToolCallResult:
		payload.ID = r.payload.ID
		r.payload = payload
	default:
		// The row already owns a richer payload (a tool_result diff, a
		// streamed command_output); the generic result blob is redundant.
	}
	return b.markUnavailable(evt, r)
}

// placeholderToolLaunch builds the launch row an `import_unavailable`
// completion addresses when the real launch is outside the imported
// range.
//
// It is deliberately thin: the completion that follows immediately
// rewrites status, summary, meta and payload, so everything here is
// either overwritten or the honest minimum (the tool's own name, and the
// completion's clock — the launch's real time is not in the file).
func (b *builder) placeholderToolLaunch(evt importir.Event, itemID string, now int64) (*row, error) {
	meta := triage.DecodeToolCompleteMeta(evt.Meta)
	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, evt.ItemType, "tool")
	item := store.Item{
		ID:        itemID,
		TurnIndex: b.turns.currentFor(evt),
		Kind:      kindToolCall,
		Role:      "assistant",
		Status:    statusRunning,
		Summary:   triage.BuildToolCallSummary(triage.ToolStartMeta{ToolName: toolName}, evt.ItemType),
		ParentID:  strings.TrimSpace(evt.ParentToolUseID),
		ToolName:  toolName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return b.appendRow(evt, item, nil, nil)
}

// splitsCompletion reports whether this tool's completion gets its own
// `tool_completion` sibling row rather than settling the launch in place.
//
// The tool-name rule is triage's, not a copy of it: one definition serves
// the live path and the import, and the parity test drives a `wait_agent`
// through both. Only the provider gate is local — live asks its session
// registry, an import asks the thread row it is writing into.
func (b *builder) splitsCompletion(toolName string) bool {
	return b.thread.Provider == "codex" && triage.ShouldSplitCodexToolCompletion(toolName)
}

func (b *builder) splitToolCompletion(evt importir.Event, launch *row, meta triage.ToolCompleteMeta, now int64) error {
	cleaned := b.providerMeta(evt)
	launch.item.Status = statusCompleted
	launch.item.Meta = triage.MergeStoredToolCallMeta(launch.item.Meta, evt.ItemType, cleaned)
	launch.item.UpdatedAt = now
	b.shapeToolMeta(launch, now)

	completion := store.Item{
		ID:           triage.ToolCompletionID(launch.item.ID),
		TurnIndex:    launch.item.TurnIndex,
		Kind:         kindToolCompletion,
		Role:         "assistant",
		Status:       triage.CompletionStatus(meta),
		Summary:      triage.BuildCompletionSummary(triage.CompletionBaseSummary(launch.item, meta, evt.ItemType), meta),
		ParentID:     launch.item.ParentID,
		CompletionOf: launch.item.ID,
		ToolName:     launch.item.ToolName,
		Meta:         string(cleaned),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	resultEvt := evt.ProviderEvent
	resultEvt.Meta = cleaned
	payload := triage.CompletionPayloadForTool(
		completion.ID, completion.ToolName, triage.CommandFromLaunch(launch.item), resultEvt, meta, now)
	inputPayload := b.shapeItemToolMeta(&completion, now)
	_, err := b.appendRow(evt, completion, payload, inputPayload)
	return err
}

// backgroundTaskTerminal settles a backgrounded Claude Task: the launch
// row stops running and a `tool_completion` sibling records the observed
// outcome. The live path layers extra live-only state on this (a pending
// terminal stash, resume carriers); an import sees the finished file and
// needs neither.
func (b *builder) backgroundTaskTerminal(evt importir.Event) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return fmt.Errorf("background task terminal carries no item id")
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	launch, found := b.byID[itemID]
	if !found {
		b.warn("import.orphan-background-terminal", fmt.Sprintf(
			"a background task terminal for %s had no launch row and was dropped", itemID))
		return nil
	}
	meta := triage.DecodeToolCompleteMeta(evt.Meta)
	cleaned := b.providerMeta(evt)
	launch.item.Status = statusCompleted
	launch.item.UpdatedAt = now

	completion := store.Item{
		ID:           triage.ToolCompletionID(launch.item.ID),
		TurnIndex:    launch.item.TurnIndex,
		Kind:         kindToolCompletion,
		Role:         "assistant",
		Status:       triage.CompletionStatus(meta),
		Summary:      triage.BuildCompletionSummary(triage.CompletionBaseSummary(launch.item, meta, evt.ItemType), meta),
		ParentID:     launch.item.ParentID,
		CompletionOf: launch.item.ID,
		ToolName:     launch.item.ToolName,
		Meta:         string(cleaned),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	resultEvt := evt.ProviderEvent
	resultEvt.Meta = cleaned
	payload := triage.CompletionPayloadForTool(
		completion.ID, completion.ToolName, triage.CommandFromLaunch(launch.item), resultEvt, meta, now)
	_, err = b.appendRow(evt, completion, payload, nil)
	return err
}

// attachPayload hangs a diff / command_output blob on the tool_call row
// its event names, replacing whatever payload the row held. Imported
// events carry whole blobs, so there is no delta-append branch.
func (b *builder) attachPayload(evt importir.Event, payloadKind string) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		b.warn("import.unanchored-payload", fmt.Sprintf(
			"a %s payload named no tool call and was dropped", payloadKind))
		return nil
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	r, found := b.byID[itemID]
	if !found {
		b.warn("import.unanchored-payload", fmt.Sprintf(
			"a %s payload for %s had no tool call and was dropped", payloadKind, itemID))
		return nil
	}
	payloadEvt := evt.ProviderEvent
	payloadEvt.Meta = b.providerMeta(evt)
	payloadID := payloadKind + ":" + itemID
	r.payload = &store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      triage.BuildPayloadMeta(payloadKind, payloadEvt),
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
	r.item.PayloadID = payloadID
	r.item.UpdatedAt = now
	return b.markUnavailable(evt, r)
}

// applyFileChangeResult lifts an edit tool's structured result into the
// `tool_result` payload the diff card renders from. Both providers route
// through the same pair of extractors triage uses; a shape that carries
// no renderable change is a no-op (the common case on a launch event,
// where no result exists yet).
func (b *builder) applyFileChangeResult(evt importir.Event, r *row, now int64) error {
	toolName := stringsx.FirstNonEmptyTrimmed(evt.ItemType, r.item.ToolName)
	if !triage.IsFileChangeItemType(toolName) || len(evt.Meta) == 0 {
		return nil
	}
	var (
		meta triage.ToolResultMeta
		diff []byte
		ok   bool
	)
	if triage.IsClaudeFilePathTool(toolName) {
		meta, diff, ok = triage.ExtractClaudeFileChangeToolResult(
			evt.Meta, toolName, triage.ExtractClaudeLaunchFilePath(r.item.Meta), b.thread.WorkspacePath)
	} else {
		meta, diff, ok = triage.ExtractFileChangeToolResult(evt.Meta, b.thread.WorkspacePath)
	}
	if !ok {
		return nil
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode tool result meta for %s: %w", r.item.ID, err)
	}
	payloadID := triage.ToolResultPayloadID(r.item.ID)
	r.payload = &store.Payload{
		ID:        payloadID,
		Kind:      toolResultPayloadKind,
		Meta:      string(metaJSON),
		Data:      diff,
		CreatedAt: now,
	}
	r.item.PayloadID = payloadID
	r.item.Summary = triage.SummarizeToolResult(meta)
	r.item.UpdatedAt = now
	return nil
}

// shapeToolMeta bounds an already-appended row's meta and promotes heavy
// tool input into the row's input payload. The launch's payload stays
// canonical, so a later re-shape never replaces it.
func (b *builder) shapeToolMeta(r *row, now int64) {
	payload := b.shapeItemToolMeta(&r.item, now)
	if payload != nil && r.inputPayload == nil {
		r.inputPayload = payload
		r.item.InputPayloadID = payload.ID
	}
}

// shapeItemToolMeta is shapeToolMeta for an item not yet in the batch. A
// shaping failure leaves the meta at its last good form and warns: the
// row is still worth importing unshaped, and triage's live wrapper takes
// the same log-and-continue stance.
func (b *builder) shapeItemToolMeta(item *store.Item, now int64) *store.Payload {
	payload, err := triage.ShapeToolItemMeta(item, now)
	if err != nil {
		b.warn("import.tool-meta-shape", fmt.Sprintf(
			"could not shape %s metadata for %s: %v", item.ToolName, item.ID, err))
		return nil
	}
	return payload
}
