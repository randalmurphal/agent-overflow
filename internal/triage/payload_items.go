package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

func (r *Router) handleDiff(evt provider.ProviderEvent) error {
	if isUpgradeOnlyDiff(evt) {
		turnIndex, err := r.currentTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("diff turn index: %w", err)
		}
		_, err = r.upgradeSummaryOnlyToolResults(evt.ThreadID, turnIndex, evt.Content)
		return err
	}

	if itemID := eventItemID(evt); itemID != "" {
		item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
		if err != nil {
			return fmt.Errorf("diff get item %s: %w", itemID, err)
		}
		if !found {
			item, err = r.newToolCallItem(evt.ThreadID, itemID, "file_change", buildSummary("diff", BuildPayloadMeta("diff", evt)), statusCompleted, eventTimestampMillis(evt))
			if err != nil {
				return fmt.Errorf("diff create tool_call %s: %w", itemID, err)
			}
		}
		return r.attachPayloadToItem(item, evt, "diff", item.Summary, evt.Replace)
	}

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("diff turn index: %w", err)
	}
	upgraded, err := r.upgradeSummaryOnlyToolResults(evt.ThreadID, turnIndex, evt.Content)
	if err != nil {
		return err
	}
	if upgraded {
		return nil
	}

	item, found, err := r.findLatestToolCall(evt.ThreadID, "file_change", "edit", "write", "multiedit", "multi_edit")
	if err != nil {
		return fmt.Errorf("diff resolve target: %w", err)
	}
	if !found {
		item, err = r.newToolCallItem(
			evt.ThreadID,
			fmt.Sprintf("diff:%d", turnIndex),
			"file_change",
			buildSummary("diff", BuildPayloadMeta("diff", evt)),
			statusCompleted,
			eventTimestampMillis(evt),
		)
		if err != nil {
			return fmt.Errorf("diff fallback tool_call: %w", err)
		}
	}
	return r.attachPayloadToItem(item, evt, "diff", item.Summary, evt.Replace)
}

func isUpgradeOnlyDiff(evt provider.ProviderEvent) bool {
	var meta struct {
		UpgradeOnly bool   `json:"upgrade_only"`
		Source      string `json:"source"`
	}
	if len(evt.Meta) == 0 || json.Unmarshal(evt.Meta, &meta) != nil {
		return false
	}
	return meta.UpgradeOnly && meta.Source == "turn/diff/updated"
}

func (r *Router) handleCommandOutput(evt provider.ProviderEvent) error {
	if r.observeCodexCommandOutput(evt) {
		return nil
	}
	itemID := eventItemID(evt)
	if itemID != "" {
		if evt.Replace {
			return r.replaceCommandOutput(evt, itemID)
		}
		return r.bufferCommandOutputDelta(evt, itemID)
	}

	item, found, err := r.findLatestToolCall(evt.ThreadID, "command_execution", "bash")
	if err != nil {
		return fmt.Errorf("command output resolve target: %w", err)
	}
	if !found {
		turnIndex, terr := r.currentTurnIndex(evt.ThreadID)
		if terr != nil {
			return fmt.Errorf("command output turn index: %w", terr)
		}
		item, err = r.newCommandOutputToolCall(
			evt.ThreadID,
			fmt.Sprintf("command-output:%d", turnIndex),
			evt,
		)
		if err != nil {
			return fmt.Errorf("command output fallback tool_call: %w", err)
		}
	}
	return r.attachPayloadToItem(item, evt, payloadKindCommandOutput, item.Summary, evt.Replace)
}

// newCommandOutputToolCall builds the command_execution row that backs a
// command-output payload when no prior row exists for it. Centralizes the
// summary/meta construction (payloadKindCommandOutput) the three not-found
// fallbacks share; each caller keeps its own policy around it — the legacy
// bash fallback's computed id, attach-and-return on a buffered first delta,
// or create-then-replace on the authoritative snapshot.
func (r *Router) newCommandOutputToolCall(
	threadID, itemID string,
	evt provider.ProviderEvent,
) (store.Item, error) {
	return r.newToolCallItem(
		threadID,
		itemID,
		"command_execution",
		buildSummary(payloadKindCommandOutput, BuildPayloadMeta(payloadKindCommandOutput, evt)),
		statusRunning,
		eventTimestampMillis(evt),
	)
}

// bufferCommandOutputDelta routes a streaming Codex output chunk through
// the stream-persist buffer instead of writing it straight through. The
// old per-chunk path cost one item read, a payload append, a full item
// upsert, and a wire upsert PER CHUNK — a chatty command (build logs,
// file dumps) turned into hundreds of SQLite transactions and frontend
// upserts per second. Buffered, each flush window (100ms / 64KB) pays
// that cost once.
func (r *Router) bufferCommandOutputDelta(evt provider.ProviderEvent, itemID string) error {
	if !r.hasCommandOutputBuffer(evt.ThreadID, itemID) {
		// Window start: verify the row once per flush window. The row
		// normally pre-exists (item/started created it before any
		// outputDelta); the create fallback persists + emits this chunk
		// immediately so the row is visible without waiting on a flush.
		_, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
		if err != nil {
			return fmt.Errorf("command output get item %s: %w", itemID, err)
		}
		if !found {
			item, err := r.newCommandOutputToolCall(evt.ThreadID, itemID, evt)
			if err != nil {
				return fmt.Errorf("command output create tool_call %s: %w", itemID, err)
			}
			return r.attachPayloadToItem(item, evt, payloadKindCommandOutput, item.Summary, false)
		}
	}
	return r.bufferCommandOutputPersistence(evt.ThreadID, itemID, evt.Content, evt.Meta, eventTimestampMillis(evt))
}

// replaceCommandOutput rewrites the item's payload with an authoritative
// full-output snapshot (Codex aggregatedOutput, shipped at item/completed
// right before EventToolComplete). Holds streamFlushMu across discard +
// rewrite: an in-flight timer flush must commit before the rewrite, and
// once the buffer is discarded no later flush can append a stale tail
// after the authoritative content.
func (r *Router) replaceCommandOutput(evt provider.ProviderEvent, itemID string) error {
	r.streamFlushMu.Lock()
	defer r.streamFlushMu.Unlock()
	r.mu.Lock()
	r.discardCommandOutputBufferLocked(evt.ThreadID, itemID)
	r.mu.Unlock()

	item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("command output get item %s: %w", itemID, err)
	}
	if !found {
		item, err = r.newCommandOutputToolCall(evt.ThreadID, itemID, evt)
		if err != nil {
			return fmt.Errorf("command output create tool_call %s: %w", itemID, err)
		}
	}
	return r.attachPayloadToItem(item, evt, payloadKindCommandOutput, item.Summary, true)
}

// flushCommandOutputPersistence delivers an accumulated window of command
// output as one synthetic delta through the existing attach path: one
// payload append (or create+link on the item's first flush), one item
// upsert, one wire emission per window instead of per chunk. Runs under
// streamFlushMu via the stream-flush funnel.
func (r *Router) flushCommandOutputPersistence(flush pendingStreamFlush) error {
	item, found, err := r.store.GetThreadItem(flush.threadID, flush.itemID)
	if err != nil {
		return fmt.Errorf("command output flush get item %s: %w", flush.itemID, err)
	}
	if !found {
		// Row vanished between stage and flush (thread cleanup raced the
		// timer). The window has nothing to attach to; drop it loudly.
		log.Printf("triage: command output flush dropped %s/%s: item gone", flush.threadID, flush.itemID)
		return nil
	}
	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  flush.threadID,
		ItemID:    flush.itemID,
		Content:   flush.payloadDelta,
		Meta:      flush.meta,
		Timestamp: time.UnixMilli(flush.updatedAt),
	}
	return r.attachPayloadToItem(item, evt, payloadKindCommandOutput, item.Summary, false)
}

func (r *Router) handleProposedPlan(evt provider.ProviderEvent) error {
	r.observeCodexModelContent(evt.ThreadID)

	now := eventTimestampMillis(evt)
	metaJSON := BuildPayloadMeta("proposed_plan", evt)
	summary := buildSummary("proposed_plan", metaJSON)
	itemID := eventItemID(evt)
	if itemID == "" {
		turnIndex, err := r.currentTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("plan turn index: %w", err)
		}
		itemID = fmt.Sprintf("plan:%d", turnIndex)
	} else if existing, found, err := r.findMatchingProposedPlanItemInCurrentTurn(evt); err != nil {
		return err
	} else if found {
		itemID = existing.ID
	}

	item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("plan get item %s: %w", itemID, err)
	}
	if !found {
		item, err = r.newToolCallItem(evt.ThreadID, itemID, "plan", summary, statusCompleted, now)
		if err != nil {
			return fmt.Errorf("plan create tool_call %s: %w", itemID, err)
		}
	} else {
		item.Kind = itemKindToolCall
		item.Role = "assistant"
		item.Status = statusCompleted
		item.ToolName = "plan"
		item.Summary = summary
		item.UpdatedAt = now
	}

	// Plan markdown lives in evt.Content (the payload data), not in
	// item.Summary (which is the plan title). Validate the plan body
	// against the workspace so the frontend can linkify file mentions
	// in the rendered plan and inline-comment review surface.
	r.enrichPathRefsFromTexts(evt.ThreadID, &item, evt.Content)

	if err := r.attachPayloadToItemQuiet(item, evt, "proposed_plan", item.Summary, true); err != nil {
		return err
	}
	parentItemID := ""
	turnIndex := item.TurnIndex
	if turnIndex == 0 {
		if current, err := r.currentTurnIndex(evt.ThreadID); err == nil {
			turnIndex = current
		}
	}
	if source, found, err := r.store.RevisionSourceProposedPlanForTurn(evt.ThreadID, turnIndex); err != nil {
		return fmt.Errorf("plan revision source %s: %w", item.ID, err)
	} else if found && source.ThreadID == evt.ThreadID {
		parentItemID = source.ItemID
	}
	if _, err := r.store.EnsureProposedPlanStateWithParent(evt.ThreadID, item.ID, parentItemID, now); err != nil {
		return fmt.Errorf("plan state %s: %w", item.ID, err)
	}
	if plan, found, err := r.store.GetThreadProposedPlanItem(evt.ThreadID, item.ID); err != nil {
		return fmt.Errorf("plan decorated item %s: %w", item.ID, err)
	} else if found {
		r.emit("provider:item_event", NewItemStreamUpsert(plan))
	}
	return nil
}

func (r *Router) findMatchingProposedPlanItemInCurrentTurn(evt provider.ProviderEvent) (store.Item, bool, error) {
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return store.Item{}, false, fmt.Errorf("plan turn index: %w", err)
	}
	items, err := r.store.ListItemsForTurn(evt.ThreadID, turnIndex)
	if err != nil {
		return store.Item{}, false, fmt.Errorf("plan matching items for turn %d: %w", turnIndex, err)
	}
	for _, item := range items {
		if item.PayloadKind != "proposed_plan" || item.PayloadID == "" {
			continue
		}
		data, err := r.store.GetPayloadData(evt.ThreadID, item.PayloadID)
		if err != nil {
			return store.Item{}, false, fmt.Errorf("plan matching payload %s: %w", item.PayloadID, err)
		}
		if strings.TrimSpace(string(data)) == strings.TrimSpace(evt.Content) {
			return item, true, nil
		}
	}
	return store.Item{}, false, nil
}

func eventTimestampMillis(evt provider.ProviderEvent) int64 {
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	return now
}

func eventItemID(evt provider.ProviderEvent) string {
	if evt.ItemID != "" {
		return strings.TrimSpace(evt.ItemID)
	}
	if id := strings.TrimSpace(metaNestedString(evt.Meta, "item", "id")); id != "" {
		return id
	}
	return strings.TrimSpace(metaNestedString(evt.Meta, "itemId"))
}

func metaNestedString(raw json.RawMessage, path ...string) string {
	if len(raw) == 0 || len(path) == 0 {
		return ""
	}

	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return ""
	}
	for _, segment := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		next, ok := obj[segment]
		if !ok {
			return ""
		}
		current = next
	}
	value, _ := current.(string)
	return value
}

func (r *Router) newToolCallItem(
	threadID, itemID, toolName, summary, status string,
	now int64,
) (store.Item, error) {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		return store.Item{}, err
	}
	if summary == "" {
		summary = toolName
	}
	item := store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    status,
		Summary:   summary,
		ToolName:  toolName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if decision := r.peekApprovalDecision(threadID, itemID); decision != "" {
		item.Decision = decision
	}
	return item, nil
}

// findLatestToolCall returns the most-recent tool_call row in the
// current turn whose tool_name matches one of toolNames
// (case-insensitive). Delegates to store.LatestToolCallByName so the
// filter runs in SQLite — previous implementations pulled every turn
// item into Go and scanned in reverse, which was O(turn_items) on
// a path called once per fallback diff/command-output event.
func (r *Router) findLatestToolCall(threadID string, toolNames ...string) (store.Item, bool, error) {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		return store.Item{}, false, err
	}
	normalized := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		trimmed := strings.TrimSpace(strings.ToLower(name))
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return r.store.LatestToolCallByName(threadID, turnIndex, normalized)
}

func (r *Router) attachPayloadToItem(
	item store.Item,
	evt provider.ProviderEvent,
	payloadKind string,
	summary string,
	replace bool,
) error {
	return r.attachPayloadToItemWithEmit(item, evt, payloadKind, summary, replace, true)
}

func (r *Router) attachPayloadToItemQuiet(
	item store.Item,
	evt provider.ProviderEvent,
	payloadKind string,
	summary string,
	replace bool,
) error {
	return r.attachPayloadToItemWithEmit(item, evt, payloadKind, summary, replace, false)
}

func (r *Router) attachPayloadToItemWithEmit(
	item store.Item,
	evt provider.ProviderEvent,
	payloadKind string,
	summary string,
	replace bool,
	emit bool,
) error {
	now := eventTimestampMillis(evt)
	payloadID := item.PayloadID
	data := []byte(evt.Content)
	linked := payloadID != "" && item.PayloadKind == payloadKind

	// Append-only hot path: when the item already owns a payload of the
	// same kind and the caller isn't replacing the blob wholesale, we
	// append the delta inside SQLite and update meta + summary without
	// ever reading the prior data into Go memory. The former path —
	// GetPayloadData → append(existing, data...) → write full blob —
	// was O(N^2) in cumulative payload size. Meta is derived from the
	// DELTA alone while streaming. command_output meta gets rebuilt
	// from the cumulative payload at completion; for diff the caller
	// passes replace=true anyway, so we stay on the full-write branch
	// below.
	if linked && !replace {
		metaEvt := evt
		metaEvt.Content = string(data)
		metaJSON := BuildPayloadMeta(payloadKind, metaEvt)
		item.PayloadID = payloadID
		if summary != "" {
			item.Summary = summary
		}
		item.UpdatedAt = now
		if item.CreatedAt == 0 {
			item.CreatedAt = now
		}
		// Chunk append + item upsert run as ONE store transaction — this
		// fires per 100ms command-output flush window (and per diff
		// append), where the former AppendPayloadData → persistItem pair
		// cost two writer-lock acquisitions for one logical operation.
		if err := r.persistItemWithPayloadAppend(item, payloadID, data, metaJSON, emit); err != nil {
			return fmt.Errorf("append %s payload %s: %w", payloadKind, payloadID, err)
		}
		return nil
	}

	if !linked {
		payloadID = uuid.New().String()
	}

	metaEvt := evt
	metaEvt.Content = string(data)
	metaJSON := BuildPayloadMeta(payloadKind, metaEvt)
	payload := store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		Data:      data,
		CreatedAt: now,
	}
	item.PayloadID = payloadID
	if summary != "" {
		item.Summary = summary
	}
	item.UpdatedAt = now
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	var err error
	if emit {
		err = r.persistItem(item, &payload)
	} else {
		err = r.persistItemQuiet(item, &payload)
	}
	if err != nil {
		return err
	}
	// Diff-kind full writes are the one attach path whose data blob is a
	// complete unified patch, so this is where the diff payload observer
	// fires for them (tool results notify from their own persist sites).
	// The append branch above never notifies: its content is a delta,
	// and spans keyed to a delta would match no reader's text.
	if payloadKind == "diff" {
		r.notifyDiffPayloadPersisted(evt.ThreadID, payloadID, ToolResultMeta{}, string(data))
	}
	return nil
}

func BuildPayloadMeta(payloadKind string, evt provider.ProviderEvent) string {
	switch payloadKind {
	case "diff":
		dm := ExtractDiffMeta(evt.Content)
		data, err := json.Marshal(dm)
		if err != nil {
			log.Printf("triage: marshal diff meta: %v", err)
			return "{}"
		}
		return string(data)
	case "command_output":
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(evt.Meta, &obj)
		return buildCommandOutputPayloadMeta(evt.Content, obj)
	case itemKindThinking, itemKindCompactionReasoning:
		// Compaction reasoning shares the thinking payload shape (preview +
		// optional signature); both render as a tail-clipped reasoning body.
		tm := ExtractThinkingMeta(evt.Content)
		tm.Signature = metaNestedString(evt.Meta, "signature")
		data, err := json.Marshal(tm)
		if err != nil {
			log.Printf("triage: marshal thinking meta: %v", err)
			return "{}"
		}
		return string(data)
	case "proposed_plan":
		pm := ExtractProposedPlanMeta(evt.Content)
		data, err := json.Marshal(pm)
		if err != nil {
			log.Printf("triage: marshal proposed plan meta: %v", err)
			return "{}"
		}
		return string(data)
	default:
		return "{}"
	}
}

func buildCommandOutputPayloadMeta(content string, obj map[string]json.RawMessage) string {
	parsed := commandOutputPayloadMetaFieldsObject(obj)
	cm := ExtractCommandOutputMetaWithError(content, parsed.Command, parsed.ExitCode, parsed.ErrorMessage)
	if cm.ErrorMessage == "" && parsed.IsError {
		cm.ErrorMessage = compactCommandErrorMessage(content)
	}
	data, err := json.Marshal(cm)
	if err != nil {
		log.Printf("triage: marshal command output meta: %v", err)
		return "{}"
	}
	return string(data)
}

type commandOutputPayloadMeta struct {
	Command      string
	ExitCode     int
	IsError      bool
	ErrorMessage string
}

func commandOutputPayloadMetaFields(raw json.RawMessage) commandOutputPayloadMeta {
	if len(raw) == 0 {
		return commandOutputPayloadMeta{}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return commandOutputPayloadMeta{}
	}
	return commandOutputPayloadMetaFieldsObject(obj)
}

func commandOutputPayloadMetaFieldsObject(obj map[string]json.RawMessage) commandOutputPayloadMeta {
	if obj == nil {
		return commandOutputPayloadMeta{}
	}
	return commandOutputPayloadMeta{
		Command:      commandOutputCommandFromMetaObject(obj),
		ExitCode:     commandOutputExitCodeFromMetaObject(obj),
		IsError:      commandOutputBoolField(obj, "is_error") || commandOutputBoolField(obj, "isError"),
		ErrorMessage: commandOutputErrorMessageFromMetaObject(obj),
	}
}

func commandOutputCommandFromMetaObject(obj map[string]json.RawMessage) string {
	if command := strings.TrimSpace(commandOutputStringField(obj, "command")); command != "" {
		return command
	}
	if input, ok := obj["input"]; ok {
		var inputObj map[string]json.RawMessage
		if json.Unmarshal(input, &inputObj) == nil {
			return strings.TrimSpace(commandOutputStringField(inputObj, "command"))
		}
	}
	return ""
}

func commandOutputExitCodeFromMetaObject(obj map[string]json.RawMessage) int {
	if code, ok := commandOutputIntField(obj, "exitCode"); ok {
		return code
	}
	if code, ok := commandOutputIntField(obj, "exit_code"); ok {
		return code
	}
	return 0
}

func commandOutputErrorMessageFromMetaObject(obj map[string]json.RawMessage) string {
	for _, key := range []string{"errorMessage", "err_msg", "message"} {
		if value := strings.TrimSpace(commandOutputStringField(obj, key)); value != "" {
			return value
		}
	}
	if value := commandOutputStringAtPath(obj, "tool_use_result", "stderr"); value != "" {
		return value
	}
	if value := commandOutputStringAtPath(obj, "tool_use_result", "stdout"); value != "" {
		return value
	}
	if value := commandOutputStringAtPath(obj, "tool_result", "content"); value != "" {
		return value
	}
	return ""
}

func commandOutputStringAtPath(obj map[string]json.RawMessage, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	raw, ok := obj[path[0]]
	if !ok {
		return ""
	}
	if len(path) == 1 {
		return commandOutputStringFromRaw(raw)
	}
	var child map[string]json.RawMessage
	if json.Unmarshal(raw, &child) != nil || child == nil {
		return ""
	}
	return commandOutputStringAtPath(child, path[1:]...)
}

func commandOutputStringField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	return commandOutputStringFromRaw(raw)
}

func commandOutputStringFromRaw(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, 2)
		for i := len(blocks) - 1; i >= 0 && len(parts) < 2; i-- {
			if strings.TrimSpace(blocks[i].Text) != "" {
				parts = append(parts, blocks[i].Text)
			}
		}
		for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
			parts[left], parts[right] = parts[right], parts[left]
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func commandOutputIntField(obj map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := obj[key]
	if !ok {
		return 0, false
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}

func commandOutputBoolField(obj map[string]json.RawMessage, key string) bool {
	raw, ok := obj[key]
	if !ok {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

func buildThinkingPayloadMeta(preview string, totalBytes int, signature string) string {
	tm := ThinkingMeta{
		TokenCount: totalBytes / 4,
		Preview:    preview,
		Signature:  signature,
	}
	data, err := json.Marshal(tm)
	if err != nil {
		log.Printf("triage: marshal thinking meta: %v", err)
		return "{}"
	}
	return string(data)
}

// BuildThinkingPayloadMeta builds the persisted thinking payload header when
// the caller already has the signature. It avoids decoding an entire provider
// envelope merely to read that one field.
func BuildThinkingPayloadMeta(content, signature string) string {
	meta := ExtractThinkingMeta(content)
	meta.Signature = signature
	data, err := json.Marshal(meta)
	if err != nil {
		log.Printf("triage: marshal thinking meta: %v", err)
		return "{}"
	}
	return string(data)
}

// buildSummary creates a short human-readable summary from meta.
func buildSummary(payloadKind, metaJSON string) string {
	switch payloadKind {
	case "diff":
		var dm DiffMeta
		if json.Unmarshal([]byte(metaJSON), &dm) == nil {
			return fmt.Sprintf("%s: +%d/-%d %s", dm.ChangeKind, dm.Insertions, dm.Deletions, dm.FilePath)
		}
	case "command_output":
		var cm CommandOutputMeta
		if json.Unmarshal([]byte(metaJSON), &cm) == nil {
			return fmt.Sprintf("$ %s (exit %d, %d lines)", cm.Command, cm.ExitCode, cm.LineCount)
		}
	case "thinking":
		var tm ThinkingMeta
		if json.Unmarshal([]byte(metaJSON), &tm) == nil {
			return tm.Preview
		}
	case "proposed_plan":
		var pm ProposedPlanMeta
		if json.Unmarshal([]byte(metaJSON), &pm) == nil && pm.Title != "" {
			return pm.Title
		}
	}
	return ""
}
