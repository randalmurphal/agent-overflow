package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
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
	metaObject := b.providerMetaObject(evt)
	meta := triage.DecodeToolStartMetaObject(metaObject)
	unavailableReason := importUnavailableReasonObject(metaObject)
	scope := stringsx.FirstNonEmptyTrimmed(evt.ParentToolUseID, meta.ParentToolUseID)
	turnIndex := b.turns.currentFor(evt)
	b.closeStreams(turnIndex, scope)

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
		existing.item.Meta = triage.MergeStoredToolCallMetaObject(
			existing.item.Meta, evt.ItemType, existing.item.ToolName, metaObject, writerControlMetaKeys[:]...)
		existing.item.UpdatedAt = now
		b.shapeToolMeta(existing, now)
		if err := b.applyFileChangeResultObject(evt, metaObject, existing, now); err != nil {
			return err
		}
		return b.markUnavailableReason(unavailableReason, existing)
	}

	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, evt.ItemType, "tool")
	item := store.Item{
		ID:           itemID,
		TurnIndex:    turnIndex,
		Kind:         kindToolCall,
		Role:         "assistant",
		Status:       statusRunning,
		Summary:      triage.BuildToolCallSummary(meta, evt.ItemType),
		ParentID:     scope,
		IsBackground: meta.IsBackground,
		ToolName:     toolName,
		Meta:         triage.StoredToolCallMetaObject(evt.ItemType, toolName, metaObject, writerControlMetaKeys[:]...),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	inputPayload := b.shapeItemToolMeta(&item, now)
	r, err := b.appendRow(evt, item, nil, inputPayload)
	if err != nil {
		return err
	}
	return b.applyFileChangeResultObject(evt, metaObject, r, now)
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
	metaObject := b.providerMetaObject(evt)
	unavailableReason := importUnavailableReasonObject(metaObject)
	r, found := b.byID[itemID]
	if !found {
		if unavailableReason == "" {
			return fmt.Errorf("tool completion %s has no launch in this session", itemID)
		}
		if r, err = b.placeholderToolLaunch(evt, metaObject, itemID, now); err != nil {
			return err
		}
	}
	if r.item.Kind != kindToolCall {
		return fmt.Errorf("tool completion %s targets a %s row", itemID, r.item.Kind)
	}
	meta := triage.DecodeToolCompleteMetaObject(metaObject)
	// A Claude SendMessage row's verdict is the ack, not the wire flag
	// (triage/sendmessage_ack.go). Read it BEFORE status, summary and the
	// payload header derive from meta, as the live path does.
	sendPatch := triage.ApplySendMessageAck(r.item.ToolName, metaObject, &meta)
	b.closeStreams(r.item.TurnIndex, r.item.ParentID)

	// Rich result payload first, so the completion's summary derivation
	// sees the payload-derived label — the live ordering in
	// handleToolComplete, and what makes an Edit row read "Edited x.go"
	// rather than "Edit: /abs/path".
	if err := b.applyFileChangeResultObject(evt, metaObject, r, now); err != nil {
		return err
	}

	if b.splitsCompletion(r.item.ToolName) {
		return b.splitToolCompletion(evt, metaObject, r, meta, now)
	}
	flaggedAtLaunch := r.item.IsBackground
	// On Claude the COMPLETION decides: a launch flagged
	// `run_in_background` can be refused (hook deny, permission denial),
	// and its result then carries no background marker. Keeping such a
	// row running on the launch flag alone is the tray-zombie class the
	// live path closed in triage/tool_lifecycle.go (2026-09-02). Codex
	// keeps the launch flag authoritative: the rollout reader stamps it
	// from wire-typed signals, never from a completion (invariant 25).
	keepRunning := meta.IsBackground || (b.thread.Provider == string(provider.Codex) && flaggedAtLaunch)
	if found && keepRunning && !b.isCodexSpawnLaunch(r) {
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
		if meta.TaskID != "" && triage.TaskIDFromItemMeta(r.item.Meta) == "" {
			// A text-only sidechain ack (claude-wire.md §E2b) is the only
			// place the task id is named; stamp it the way the live path
			// does so the row keeps its terminal correlation. First
			// binding wins, as in triage.
			encoded, _ := json.Marshal(meta.TaskID)
			r.item.Meta = triage.MergeStoredToolCallMetaObject(r.item.Meta, evt.ItemType, r.item.ToolName,
				map[string]json.RawMessage{"task_id": encoded})
		}
		r.item.IsBackground = true
		r.item.UpdatedAt = now
		return b.markUnavailableReason(unavailableReason, r)
	}
	if flaggedAtLaunch {
		// The hint was wrong: settle in place as an ordinary tool row so
		// nothing lists it as live background work.
		r.item.IsBackground = false
		metaObject = cloneWithIsBackgroundFalse(metaObject)
	}

	r.item.Status = triage.CompletionStatus(meta)
	r.item.Summary = triage.BuildCompletionSummary(
		triage.CompletionBaseSummary(r.item, meta, evt.ItemType), meta)
	if name := strings.TrimSpace(meta.ToolName); name != "" {
		r.item.ToolName = name
	}
	r.item.Meta = triage.MergeStoredToolCallMetaObject(
		r.item.Meta, evt.ItemType, r.item.ToolName, metaObject, writerControlMetaKeys[:]...)
	if len(sendPatch) > 0 {
		// After the wire merge, so the ack's verdict is what the stored
		// meta says (the wire's own `is_error:false` would otherwise win).
		b.stampSendMessageRecipient(r, sendPatch)
		r.item.Meta = triage.MergeStoredToolCallMetaObject(r.item.Meta, evt.ItemType, r.item.ToolName, sendPatch)
	}
	r.item.UpdatedAt = now
	b.shapeToolMeta(r, now)

	payload := triage.CompletionPayloadForToolObject(
		r.item.ID, r.item.ToolName, triage.CommandFromLaunch(r.item), evt.Content, metaObject, meta, now)
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
	return b.markUnavailableReason(unavailableReason, r)
}

func (b *builder) isCodexSpawnLaunch(r *row) bool {
	if b.thread.Provider != string(provider.Codex) || r == nil || r.item.ToolName != "collab_agent" {
		return false
	}
	var stored struct {
		Input struct {
			Tool string `json:"tool"`
		} `json:"input"`
	}
	if json.Unmarshal([]byte(r.item.Meta), &stored) != nil {
		return false
	}
	switch strings.TrimSpace(stored.Input.Tool) {
	case "spawn_agent", "spawnAgent":
		return true
	default:
		return false
	}
}

func (b *builder) subagentStatus(evt importir.Event) error {
	launchID := stringsx.FirstNonEmptyTrimmed(evt.ItemID, evt.ParentToolUseID)
	if launchID == "" {
		return fmt.Errorf("subagent status carries no launch id")
	}
	launch, found := b.byID[launchID]
	if !found {
		return fmt.Errorf("subagent status targets missing launch %s", launchID)
	}
	if launch.item.Kind != kindToolCall {
		return fmt.Errorf("subagent status %s targets a %s row", launchID, launch.item.Kind)
	}
	var signal struct {
		AgentPath string `json:"agent_path"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(evt.Meta, &signal); err != nil {
		return fmt.Errorf("decode subagent status %s: %w", launchID, err)
	}
	childID := strings.TrimSpace(signal.AgentPath)
	if childID == "" {
		return fmt.Errorf("subagent status %s carries no child id", launchID)
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	if launch.item.ToolName != "collab_agent" {
		return fmt.Errorf("subagent status %s targets tool %s", launchID, launch.item.ToolName)
	}
	updated, _, matched := triage.MergeCodexSubagentTerminalMeta(launch.item.Meta, childID, signal.Status)
	if !matched {
		return fmt.Errorf("subagent status %s names child %s outside the launch", launchID, childID)
	}
	launch.item.Meta = updated
	launch.item.IsBackground = true
	launch.item.UpdatedAt = now
	return nil
}

// placeholderToolLaunch builds the launch row an `import_unavailable`
// completion addresses when the real launch is outside the imported
// range.
//
// It is deliberately thin: the completion that follows immediately
// rewrites status, summary, meta and payload, so everything here is
// either overwritten or the honest minimum (the tool's own name, and the
// completion's clock — the launch's real time is not in the file).
func (b *builder) placeholderToolLaunch(evt importir.Event, metaObject map[string]json.RawMessage, itemID string, now int64) (*row, error) {
	meta := triage.DecodeToolCompleteMetaObject(metaObject)
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

func (b *builder) splitToolCompletion(evt importir.Event, metaObject map[string]json.RawMessage, launch *row, meta triage.ToolCompleteMeta, now int64) error {
	launch.item.Status = statusCompleted
	launch.item.Meta = triage.MergeStoredToolCallMetaObject(
		launch.item.Meta, evt.ItemType, launch.item.ToolName, metaObject, writerControlMetaKeys[:]...)
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
		Meta:         triage.StoredToolCallMetaObject(evt.ItemType, launch.item.ToolName, metaObject, writerControlMetaKeys[:]...),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	payload := triage.CompletionPayloadForToolObject(
		completion.ID, completion.ToolName, triage.CommandFromLaunch(launch.item), evt.Content, metaObject, meta, now)
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
func (b *builder) applyFileChangeResultObject(evt importir.Event, metaObject map[string]json.RawMessage, r *row, now int64) error {
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
		meta, diff, ok = triage.ExtractClaudeFileChangeToolResultObject(
			metaObject, toolName, triage.ExtractClaudeLaunchFilePath(r.item.Meta), b.thread.WorkspacePath)
	} else {
		meta, diff, ok = triage.ExtractFileChangeToolResultObject(metaObject, b.thread.WorkspacePath)
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

// cloneWithIsBackgroundFalse returns a copy of a completion's meta object
// with `is_background` pinned to false, so the merge onto a launch row
// that was flagged at launch overwrites the stale hint in `items.meta`.
func cloneWithIsBackgroundFalse(metaObject map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(metaObject)+1)
	for key, value := range metaObject {
		out[key] = value
	}
	out["is_background"] = json.RawMessage("false")
	return out
}

// stampSendMessageRecipient is the importer's half of the live
// Router.stampSendMessageRecipient: the recipient launch is in this batch
// rather than in the store, so it is the earliest row built so far that
// carries the task id — the same first binding the store query's
// `ORDER BY created_at` answers. A miss (a peer session, a name, a launch
// outside the imported range) leaves the patch as it was and the row
// shows the recipient as typed.
func (b *builder) stampSendMessageRecipient(r *row, patch map[string]json.RawMessage) {
	taskID := triage.SendMessageRecipientTaskID(patch, r.item.Meta)
	if taskID == "" {
		return
	}
	for _, candidate := range b.rows {
		if candidate == r || triage.TaskIDFromItemMeta(candidate.item.Meta) != taskID {
			continue
		}
		triage.StampSendMessageRecipient(patch, taskID, candidate.item.Meta)
		return
	}
}
