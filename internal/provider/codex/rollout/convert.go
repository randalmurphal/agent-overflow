package rollout

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// converter turns rollout lines into import events. One instance per Parse
// call; all correlation state is per file.
type converter struct {
	opts ParseOptions
	pre  preScanResult

	events   []importir.Event
	warnings []importir.Warning
	unknown  map[string]int
	corrupt  int

	endOffset           int64
	lineStart, lineNext int64
	lastTimestamp       time.Time
	profile             profileAccumulator

	// sawCompacted records that a durable `compacted` record has been read.
	// It is what suppresses the lightweight `event_msg/context_compacted`
	// twin, which would otherwise write a second divider for the same
	// compaction.
	//
	// A running flag is exact here, not an approximation of a file-global
	// one: every compaction path in codex-rs persists
	// RolloutItem::Compacted (Session::replace_compacted_history) and only
	// then emits the ContextCompaction item whose legacy event is the twin
	// (compact.rs / compact_remote.rs / compact_remote_v2.rs all await the
	// first before the second), so the record always precedes its twin in
	// file order. A twin seen with the flag still clear therefore belongs
	// to a file old enough to write no `compacted` records at all — which
	// is exactly the case the fallback exists for.
	sawCompacted bool

	// paginated is the file's declared history mode
	// (`session_meta.history_mode == "paginated"`, Codex >= 0.147). It
	// decides which of the two record sets owns the conversation: a
	// paginated rollout persists NO legacy `user_message` /
	// `agent_message` / `agent_reasoning` / `*_end` records and one
	// `event_msg/item_completed` per turn item instead
	// (codex-rs/rollout/src/policy.rs). Reading it off the header rather
	// than inferring it from the records seen so far is what makes a TAIL
	// REFRESH behave identically to a full parse: the header is always read
	// (preScan starts at offset 0), while a running flag would be clear for
	// a cursor that happens to land after the first item.
	paginated bool

	// itemRows holds call ids an `item_completed` record already emitted a
	// complete standalone tool row for. In a paginated file the item is
	// written one line BEFORE the `response_item` call carrying the same
	// id, so without this the response item would open a second row for a
	// tool the item already reported in full. See items.go
	// applyItemEndEvent and tools.go.
	itemRows map[string]struct{}

	// Turn state (turns.go).
	turnIndex  int
	turn       *openTurn
	pendingCtx turnContextPayload
	// usedTurnIDs is every turn id this Parse has already opened a turn
	// with. Codex writes records AFTER a turn settles that still name it —
	// a trailing `token_count` or `thread_rolled_back` behind a
	// `turn_aborted`, a `task_complete` racing an abort — and `pendingCtx`
	// still holds the settled turn's id. Re-opening under that id would
	// hand the writer two turns with one primary key; the writer's own
	// collision check exists for ids a PREVIOUS import committed, which the
	// converter cannot see, but an id this very Parse minted is knowable
	// here and must not be reused. See ensureTurn / startTurn /
	// completeTurn.
	usedTurnIDs map[string]struct{}
	// accounted is the cumulative usage already attributed to earlier
	// turns; Codex reports cumulative totals, so a turn's own usage is the
	// difference. See turns.go.
	accounted tokenUsageWire

	// Tool correlation (tools.go). tools/toolOrder hold calls awaiting an
	// output; toolItemIDs is the file-lifetime call_id → row id index the
	// collab records parent themselves by (collab.go).
	tools        map[string]*openTool
	toolOrder    []string
	toolItemIDs  map[string]string
	agentParents map[string]string
}

func newConverter(opts ParseOptions, pre preScanResult) *converter {
	c := &converter{
		opts: opts,
		pre:  pre,
		// The pre-scan reads from offset 0 and the conversion pass may start
		// past it, so a `compacted` record the pre-scan happened to see
		// before short-circuiting is carried over rather than re-derived.
		sawCompacted: pre.sawCompacted,
		paginated:    pre.meta.HistoryMode == HistoryModePaginated,
		itemRows:     map[string]struct{}{},
		unknown:      map[string]int{},
		usedTurnIDs:  map[string]struct{}{},
		tools:        map[string]*openTool{},
		toolItemIDs:  map[string]string{},
		agentParents: map[string]string{},
	}
	// Seed the clock from the session header, so an event read off a line
	// whose own timestamp is unparseable still carries one. Every imported
	// row must have a source timestamp — the writer refuses the whole
	// import rather than restamping with now() — and the session's own
	// creation time is the only honest floor the file offers. Without the
	// seed, one malformed timestamp on the first content line would cost
	// the entire session.
	c.lastTimestamp = pre.meta.CreatedAt
	return c
}

func (c *converter) result() ParseResult {
	res := ParseResult{
		Meta:         c.pre.meta,
		Events:       c.events,
		Profile:      c.profile.value,
		EndOffset:    c.endOffset,
		CorruptLines: c.corrupt,
		UnknownTypes: c.unknown,
		Warnings:     c.warnings,
	}
	if !c.pre.metaFound {
		res.Warnings = append(res.Warnings, importir.Warning{
			Code:    WarnMissingSessionID,
			Message: fmt.Sprintf("No session header for %s was found in the rollout file; workspace and fork details are unavailable.", c.opts.SessionID),
		})
	}
	if base := c.pre.meta.HistoryBase; base != nil {
		// The thread continues a DIFFERENT rollout file: everything before
		// `end_ordinal_exclusive` lives there and is not in this file at
		// all. AO does not follow the chain (SessionMeta.HistoryBase holds
		// the TODO with the field shape a follower needs), so the honest
		// answer is to say the earlier history is missing rather than
		// present a truncated thread as complete.
		res.Warnings = append(res.Warnings, importir.Warning{
			Code: WarnHistoryBase,
			Message: fmt.Sprintf(
				"This Codex session continues an earlier rollout (%s) that Agent Overflow does not follow; the conversation before it is not imported.",
				base.ThreadID),
		})
	}
	if c.corrupt > 0 {
		res.Warnings = append(res.Warnings, importir.Warning{
			Code:    WarnCorruptLines,
			Message: fmt.Sprintf("%s skipped in the Codex session file.", pluralLines(c.corrupt)),
		})
	}
	return res
}

func pluralLines(n int) string {
	if n == 1 {
		return "1 unreadable line was"
	}
	return strconv.Itoa(n) + " unreadable lines were"
}

// convert dispatches one decoded line.
func (c *converter) convert(env envelope) {
	switch env.Type {
	case typeSessionMeta:
		// Accepted in preScan; a second meta line (the source's, embedded
		// by a fork) is ignored here by construction.
	case typeTurnContext:
		c.applyTurnContext(env)
	case typeCompacted:
		c.convertCompacted(env)
	case typeEventMsg:
		c.convertEventMsg(env)
	case typeResponseItem:
		c.convertResponseItem(env)
	case typeInterAgent, typeInterAgentMet:
		c.convertInterAgent(env)
	case typeWorldState:
		// Recognised and dropped, NOT unknown. `world_state` is the engine's
		// resume baseline for model-visible context diffing (see
		// worldStatePayload); it has no transcript projection. Codex writes
		// one per turn on every modern thread, so counting it as unknown put
		// a `codex-unknown-types` warning on essentially every import — noise
		// that hides the warning's real job, which is naming genuine wire
		// drift. The decode is what makes this recognition rather than a
		// blanket skip: a payload that no longer matches 0.149.0's shape
		// falls through to the unknown counter and warns again.
		c.dropRecognised(env, func() bool {
			var p worldStatePayload
			return json.Unmarshal(env.Payload, &p) == nil && p.Full != nil && p.State != nil
		})
	case typeSecurityRisk:
		// Same rule, stronger reason: upstream requires these scores never
		// reach a user-visible thread item projection (see
		// securityRiskPayload). Importing one would be exactly that.
		c.dropRecognised(env, func() bool {
			var p securityRiskPayload
			return json.Unmarshal(env.Payload, &p) == nil && len(p.Scores) > 0
		})
	default:
		c.countUnknown(env)
	}
}

// dropRecognised skips a record type this package deliberately does not
// import, provided its payload still looks like the shape we verified against
// the Codex source. A payload that does not is counted under a drift-suffixed
// key so a changed shape still reaches the user as a warning instead of being
// silently discarded by a stale `case`.
func (c *converter) dropRecognised(env envelope, shapeMatches func() bool) {
	if shapeMatches() {
		return
	}
	c.unknown[qualifiedType(env)+" (unexpected shape)"]++
}

func (c *converter) countUnknown(env envelope) {
	c.unknown[qualifiedType(env)]++
}

// finish closes anything still open at end of file.
func (c *converter) finish() {
	c.closeTurn(nil, time.Time{})
	if total := len(c.unknown); total > 0 {
		c.warnings = append(c.warnings, importir.Warning{
			Code:    WarnUnknownTypes,
			Message: "Some Codex record types were not recognised and were skipped: " + strings.Join(sortedKeys(c.unknown), ", ") + ".",
		})
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Small maps; insertion sort keeps the output deterministic without
	// pulling in a comparator.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// emit appends one event, stamping the coordinates of the line being read and
// defaulting turn attribution to the open turn.
func (c *converter) emit(evt provider.ProviderEvent) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = c.lastTimestamp
	}
	if evt.TurnID == "" && c.turn != nil {
		evt.TurnID = c.turn.id
	}
	if evt.TurnIndex == 0 && c.turn != nil {
		evt.TurnIndex = c.turn.index
	}
	c.events = append(c.events, importir.Event{
		ProviderEvent: evt,
		SourceUUID:    lineUUID(c.lineStart),
		SourceOffset:  c.lineNext,
	})
}

// lineUUID is this package's per-event provenance handle: the byte offset of
// the line the event was read from, prefixed so it can never be mistaken for
// a number to do arithmetic on. A rollout is append-only, so a line's start
// offset is stable for the life of the file and unique within it — which is
// what makes it usable both as `items.meta.import_source_uuid` and as a
// human-traceable pointer back into the file.
func lineUUID(offset int64) string {
	return "line:" + strconv.FormatInt(offset, 10)
}

// ------------------------------------------------------------- content rows

// emitUserText emits the wire-confirmation shape a live Codex session
// produces for a user message (classifyItemCompleted's `userMessage` branch).
func (c *converter) emitUserText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventUserText,
		Role:           "user",
		Content:        text,
		ContentPresent: true,
	})
}

// emitAssistantText and emitThinking both emit EventContentBlockStop with the
// `blockType` meta, which is exactly what the live Codex adapter emits when an
// agentMessage / reasoning item completes. Using the same shape is what lets
// the import writer reuse triage's settled-block persistence unchanged.
func (c *converter) emitAssistantText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventContentBlockStop,
		Role:           "assistant",
		ItemType:       "agentMessage",
		Content:        text,
		ContentPresent: true,
		Meta:           json.RawMessage(`{"blockType":"text"}`),
	})
}

func (c *converter) emitThinking(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventContentBlockStop,
		ItemType:       "reasoning",
		Content:        text,
		ContentPresent: true,
		Meta:           json.RawMessage(`{"blockType":"thinking"}`),
	})
}

func (c *converter) emitNotification(summary string, meta map[string]any, parentToolUseID string) {
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:            provider.EventNotification,
		Role:            "system",
		Content:         summary,
		Meta:            metaJSON(meta),
		ParentToolUseID: parentToolUseID,
	})
}

// convertCompacted emits the compaction divider. `replacement_history` is
// deliberately not written: it restates history AO already has rows for.
func (c *converter) convertCompacted(env envelope) {
	c.sawCompacted = true
	var p compactedPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	meta := map[string]any{}
	if p.WindowID != "" {
		meta["windowId"] = p.WindowID
	}
	c.emitCompactionBoundary(p.Message, meta)
}

func (c *converter) emitCompactionBoundary(summary string, meta map[string]any) {
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:           provider.EventCompactBoundary,
		Role:           "system",
		ItemType:       "contextCompaction",
		Content:        summary,
		ContentPresent: summary != "",
		Meta:           metaJSON(meta),
	})
}

func metaJSON(meta map[string]any) json.RawMessage {
	if len(meta) == 0 {
		return nil
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return encoded
}
