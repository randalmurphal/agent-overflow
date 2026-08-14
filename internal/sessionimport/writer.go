package sessionimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// items.kind values this writer produces. The column's CHECK enum is
// closed (see internal/store/AGENTS.md); an import never invents a kind.
const (
	kindUserText       = "user_text"
	kindAssistantText  = "assistant_text"
	kindThinking       = "thinking"
	kindToolCall       = "tool_call"
	kindToolCompletion = "tool_completion"
	kindCompaction     = "compaction"
	kindNotification   = "notification"
	kindCommandResult  = "command_result"
	kindError          = "error"
	kindAPIError       = "api_error"

	statusRunning   = "running"
	statusCompleted = "completed"
	statusErrored   = "errored"

	payloadKindToolCallResult = "tool_call_result"
	payloadKindCommandResult  = "command_result"
	payloadKindDiff           = "diff"
	payloadKindCommandOutput  = "command_output"
	payloadKindProposedPlan   = "proposed_plan"

	// proposedPlanToolName is the tool name triage files a proposed plan
	// under. It is not a real provider tool — the plan rides the timeline
	// as a tool_call because items.kind has no plan kind.
	proposedPlanToolName = "plan"
)

// importUnavailableMetaKey is the event-meta key a provider reader sets
// when the session file no longer contains an item's payload. The writer
// strips it from the stored provider meta and re-stamps it through
// itemmeta so the persisted representation has exactly one form — which
// is why it is itemmeta's constant rather than a matching literal: the
// two keys are the same key, and a second spelling could only ever be a
// way for the strip and the stamp to disagree. Both provider readers name
// the same constant. See AGENTS.md §Writer input contract.
const importUnavailableMetaKey = itemmeta.ImportUnavailableKey

// blockTypeMetaKey is the framing key a settled content block names its
// kind with. The live path READS it (blockTypeForStop) to pick which
// settle to run and never persists it, so neither may the writer — a
// row carrying it would differ from its live twin in items.meta.
const blockTypeMetaKey = "blockType"

// writerControlMetaKeys are the event-meta keys the writer ROUTES on.
// None of them is provider content, none of them reaches a live row, and
// providerMeta strips all of them — stripping centrally is what keeps a
// future caller from forwarding one into items.meta by forgetting.
var writerControlMetaKeys = [...]string{importUnavailableMetaKey, blockTypeMetaKey}

// Writer builds store rows for one imported session branch.
//
// One Writer serves one thread. Build is store-pure: it reads the thread
// to work out where its rows start (a refresh appends to an existing
// thread) and writes nothing — store.ApplyImportBatch commits the batch
// the caller gets back, in one transaction.
type Writer struct {
	store  *store.Store
	thread store.Thread
}

// NewWriter returns a Writer for thread. The thread row must already
// exist: its id scopes every row, and its Provider and ProjectID are
// what usage rows are attributed to.
func NewWriter(s *store.Store, thread store.Thread) *Writer {
	return &Writer{store: s, thread: thread}
}

// Build converts events into one batch of store rows, in chain order.
//
// Errors are reserved for structurally broken input — a tool completion
// with no launch, an item-producing event with no timestamp or no source
// coordinate — because those mean the reader handed us something it
// should not have, and a half-shaped thread is worse than a refused one.
// Everything recoverable (an event kind with no row mapping, a shaping
// hiccup on one row) comes back as a warning next to the rows that did
// convert.
func (w *Writer) Build(events []importir.Event) (store.ImportBatch, []importir.Warning, error) {
	if w == nil || w.store == nil {
		return store.ImportBatch{}, nil, fmt.Errorf("sessionimport: writer has no store")
	}
	if strings.TrimSpace(w.thread.ID) == "" {
		return store.ImportBatch{}, nil, fmt.Errorf("sessionimport: writer thread has no id")
	}
	b, err := newBuilder(w.store, w.thread)
	if err != nil {
		return store.ImportBatch{}, nil, err
	}
	for i := range events {
		if err := b.apply(events[i]); err != nil {
			return store.ImportBatch{}, nil, fmt.Errorf(
				"sessionimport: event %d (%s): %w", i, events[i].Kind, err)
		}
	}
	if err := b.turns.failure(); err != nil {
		return store.ImportBatch{}, nil, fmt.Errorf("sessionimport: %w", err)
	}
	return b.batch(), b.warnings, nil
}

// row is one timeline row under construction. A tool_call is upserted in
// place across its launch, its result payload, and its completion — the
// same single row a live session ends up with — so rows are addressable
// by item id until the batch is sealed.
type row struct {
	item         store.Item
	payload      *store.Payload
	inputPayload *store.Payload
}

type builder struct {
	thread store.Thread

	rows []*row
	byID map[string]*row

	turns turnState

	itemIndex map[int]int

	segmentSeq map[string]int
	blockSeq   map[string]int
	errorSeq   map[string]int
	compactSeq map[int]int
	notifySeq  map[int]int
	commandSeq map[int]int
	userSeq    map[int]int

	activeText     map[string]string
	activeThinking map[string]string

	warnings []importir.Warning
}

// firstTurnIndex is the turn index an import's first turn takes. 1-based,
// matching app_thread_from_pr.go's seeded thread: index 0 is what a
// thread's own first live send would allocate, and leaving it free keeps
// an imported thread's turns from colliding with one if the user later
// resumes the session in AO.
const firstTurnIndex = 1

func newBuilder(s *store.Store, thread store.Thread) (*builder, error) {
	// LastTurnIndex already unions items ∪ turns and answers 0 for an empty
	// thread, so it is the whole seed: a thread that holds turns but no
	// items (an import whose every row was skipped) continues past them
	// rather than restarting at 1 and colliding.
	lastTurn, err := s.LastTurnIndex(thread.ID)
	if err != nil {
		return nil, fmt.Errorf("sessionimport: seed turn index for %s: %w", thread.ID, err)
	}
	next := lastTurn + 1
	if next < firstTurnIndex {
		next = firstTurnIndex
	}
	// The ids the thread ALREADY holds. A provider file can name the same
	// wire turn twice across an import and a later refresh (Codex re-opens
	// `turn-1` when the import stopped before its `task_complete`), and
	// ApplyImportBatch inserts turns — so the collision has to be refused
	// while the batch is still store-pure, not discovered by the INSERT
	// after a check RPC promised the user it would apply.
	takenTurnIDs, err := s.TurnIDsForThread(thread.ID)
	if err != nil {
		return nil, fmt.Errorf("sessionimport: read existing turns of %s: %w", thread.ID, err)
	}
	return &builder{
		thread:         thread,
		byID:           map[string]*row{},
		turns:          newTurnState(thread, next, takenTurnIDs),
		itemIndex:      map[int]int{},
		segmentSeq:     map[string]int{},
		blockSeq:       map[string]int{},
		errorSeq:       map[string]int{},
		compactSeq:     map[int]int{},
		notifySeq:      map[int]int{},
		commandSeq:     map[int]int{},
		userSeq:        map[int]int{},
		activeText:     map[string]string{},
		activeThinking: map[string]string{},
	}, nil
}

// batch seals the build. Any turn the session file left open is settled
// first — see turnState for why a NULL completed_at must never reach an
// imported thread — and its unresolved tools settle with it.
func (b *builder) batch() store.ImportBatch {
	if open := b.turns.current(); open != 0 {
		b.forceCloseOrphanTools(open, b.turns.lastActivity[open])
	}
	b.turns.closeOpen(0)
	rows := make([]store.ImportRow, 0, len(b.rows))
	for _, r := range b.rows {
		rows = append(rows, store.ImportRow{
			Item:         r.item,
			Payload:      r.payload,
			InputPayload: r.inputPayload,
		})
	}
	return store.ImportBatch{
		Turns:           b.turns.turns,
		TurnCompletions: b.turns.completions,
		Rows:            rows,
		Usage:           b.turns.usage,
	}
}

// apply routes one event. The kind set is open — a provider that grows a
// new event kind must not fail an import — so an unmapped kind warns and
// is skipped rather than erroring.
func (b *builder) apply(evt importir.Event) error {
	switch evt.Kind {
	case provider.EventTurnStart:
		return b.turnStart(evt)
	case provider.EventTurnComplete:
		return b.turnComplete(evt)
	case provider.EventUserText:
		return b.userText(evt)
	case provider.EventTextDelta:
		return b.assistantText(evt)
	case provider.EventThinking:
		return b.thinking(evt)
	case provider.EventContentBlockStop:
		return b.contentBlockStop(evt)
	case provider.EventProposedPlan:
		return b.proposedPlan(evt)
	case provider.EventToolStart:
		return b.toolStart(evt)
	case provider.EventToolComplete:
		return b.toolComplete(evt)
	case provider.EventBackgroundTaskTerminal:
		return b.backgroundTaskTerminal(evt)
	case provider.EventDiff:
		return b.attachPayload(evt, payloadKindDiff)
	case provider.EventCommandOutput:
		return b.attachPayload(evt, payloadKindCommandOutput)
	case provider.EventCompactBoundary:
		return b.compaction(evt)
	case provider.EventError:
		return b.providerError(evt)
	case provider.EventNotification:
		return b.notification(evt)
	case provider.EventCommandResult:
		return b.commandResult(evt)
	case provider.EventTokenUsage, provider.EventInit, provider.EventSessionStatus,
		provider.EventRateLimits, provider.EventContentBlockStart:
		// Live-only signals with no durable row: the context meter, the
		// session handshake, and the block OPENING an imported event
		// already carries whole. (A block STOP is not framing — see
		// contentBlockStop.)
		return nil
	default:
		b.warn("import.unmapped-event", fmt.Sprintf(
			"skipped a %s event with no imported row shape", evt.Kind))
		return nil
	}
}

// forceCloseOrphanTools settles every still-running foreground tool_call
// of a turn that just ended. Invariant 23: a turn boundary settles the
// timeline whether or not the provider reported every tool_result — and
// a session file that lost one would otherwise import a card that spins
// forever, since an imported thread has no live session to settle it
// later. Backgrounded launches are exempt (invariant 24): their work
// legitimately outlives the turn, and the sibling completion row is what
// carries their outcome.
func (b *builder) forceCloseOrphanTools(turnIndex int, now int64) {
	for _, r := range b.rows {
		if r.item.TurnIndex != turnIndex || r.item.Kind != kindToolCall {
			continue
		}
		if r.item.Status != statusRunning || r.item.IsBackground {
			continue
		}
		r.item.Status = statusErrored
		r.item.Summary = triage.ForceCloseSummary(r.item.Summary)
		if now > r.item.UpdatedAt {
			r.item.UpdatedAt = now
		}
	}
}

func (b *builder) warn(code, message string) {
	b.warnings = append(b.warnings, importir.Warning{Code: code, Message: message})
}

// appendRow seals one new row: it takes the next item_index for its turn,
// stamps import provenance, and registers the row for later upserts.
//
// item_index is allocated PER TURN from 0, which is what store's live
// allocator (nextItemIndexTx) does; every ordering read sorts by
// (turn_index, item_index).
func (b *builder) appendRow(evt importir.Event, item store.Item, payload, inputPayload *store.Payload) (*row, error) {
	if _, exists := b.byID[item.ID]; exists {
		return nil, fmt.Errorf("item id %s already used by an earlier row", item.ID)
	}
	source, err := sourceCoordinate(evt)
	if err != nil {
		return nil, err
	}
	item.ThreadID = b.thread.ID
	item.ItemIndex = b.itemIndex[item.TurnIndex]
	b.itemIndex[item.TurnIndex]++

	meta, err := itemmeta.MarkImported(item.Meta, source)
	if err != nil {
		return nil, fmt.Errorf("stamp provenance on %s: %w", item.ID, err)
	}
	item.Meta = meta
	if payload != nil {
		item.PayloadID = payload.ID
	}
	if inputPayload != nil {
		item.InputPayloadID = inputPayload.ID
	}

	b.turns.touch(item.TurnIndex, item.UpdatedAt)
	r := &row{item: item, payload: payload, inputPayload: inputPayload}
	b.rows = append(b.rows, r)
	b.byID[item.ID] = r
	return r, b.markUnavailable(evt, r)
}

// markUnavailable stamps the reader's missing-payload marker onto the
// row an event addresses. Every path that resolves a row from an event
// calls it LAST, after the row's meta has settled: the marker most
// often rides a tool COMPLETION whose payload the session file no
// longer holds, and that event updates the launch row rather than
// creating one — stamping only on row creation would drop it.
func (b *builder) markUnavailable(evt importir.Event, r *row) error {
	return b.markUnavailableReason(importUnavailableReason(evt.Meta), r)
}

func (b *builder) markUnavailableReason(reason string, r *row) error {
	if reason == "" {
		return nil
	}
	meta, err := itemmeta.MarkImportUnavailable(r.item.Meta, reason)
	if err != nil {
		return fmt.Errorf("stamp unavailable payload on %s: %w", r.item.ID, err)
	}
	r.item.Meta = meta
	return nil
}

// timestamp is an item-producing event's row clock. Imported rows carry
// the provider's own timestamps end to end, so an event without one is
// refused rather than silently restamped with now() — a thread whose
// history claims it happened at import time is worse than no thread.
func timestamp(evt importir.Event) (int64, error) {
	if evt.Timestamp.IsZero() {
		return 0, fmt.Errorf("event carries no timestamp")
	}
	millis := evt.Timestamp.UnixMilli()
	if millis <= 0 {
		return 0, fmt.Errorf("event timestamp %s is not a usable row clock", evt.Timestamp)
	}
	return millis, nil
}

// sourceCoordinate is the provenance stamp for an imported row.
//
// SourceUUID is REQUIRED of both providers (see internal/importir): Claude
// hands over the transcript row's uuid, Codex mints `line:<offset>` for the
// rollout line. SourceOffset is a separate, optional resume position and is
// never a substitute — a stamp derived from it would be a second spelling of
// the same coordinate, and the two would disagree the moment a reader emitted
// one event per line for several lines.
//
// An event with no uuid is refused: a blank provenance stamp is
// indistinguishable from a bug at refresh time, which is the one reader that
// depends on it.
func sourceCoordinate(evt importir.Event) (string, error) {
	uuid := strings.TrimSpace(evt.SourceUUID)
	if uuid == "" {
		return "", fmt.Errorf("event carries no source uuid")
	}
	return uuid, nil
}

// importUnavailableReason reads the reader-supplied marker naming a
// payload the session file no longer holds.
func importUnavailableReason(raw json.RawMessage) string {
	if len(raw) == 0 || !bytes.Contains(raw, []byte(importUnavailableMetaKey)) {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return ""
	}
	return importUnavailableReasonObject(obj)
}

func importUnavailableReasonObject(obj map[string]json.RawMessage) string {
	var reason string
	if json.Unmarshal(obj[importUnavailableMetaKey], &reason) != nil {
		return ""
	}
	return strings.TrimSpace(reason)
}

// providerMeta is the event meta as it lands in items.meta: canonical
// JSON minus the writer's own control keys. `import_unavailable` is
// re-stamped through itemmeta so the marker has exactly one persisted
// representation; `blockType` is pure framing the live path never
// persists at all.
//
// Meta that will not decode as a JSON object stores as "" — the same
// answer triage's validJSONObjectString gives, so parity holds — but it is
// never SILENT. A reader that attached metadata and lost it is a reader
// bug, and the only trace of it would otherwise be a row that renders one
// field short. It is a WARNING rather than an error because the rest of
// the row is intact and refusing the whole session over one unreadable
// meta blob is the worse answer (AGENTS.md §Contract).
func (b *builder) providerMeta(evt importir.Event) json.RawMessage {
	if len(evt.Meta) == 0 {
		return nil
	}
	obj := b.providerMetaObject(evt)
	if obj == nil {
		return nil
	}
	return b.encodeProviderMetaObject(evt, obj, nil)
}

func (b *builder) encodeProviderMetaObject(evt importir.Event, obj map[string]json.RawMessage, extra map[string]string) json.RawMessage {
	shaped := make(map[string]json.RawMessage, len(obj)+len(extra))
	for key, value := range obj {
		shaped[key] = value
	}
	for _, key := range writerControlMetaKeys {
		delete(shaped, key)
	}
	for key, value := range extra {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			encoded, err := json.Marshal(trimmed)
			if err != nil {
				continue
			}
			shaped[key] = encoded
		}
	}
	encoded, err := json.Marshal(shaped)
	if err != nil {
		// Unreachable by construction — every value came out of the decode
		// above and is therefore valid JSON — which is exactly why it must
		// not be swallowed if it ever happens.
		b.warn("import.unreadable-meta", fmt.Sprintf(
			"a %s event's metadata could not be re-encoded (%v); the row imported without it", evt.Kind, err))
		return nil
	}
	return encoded
}

// providerMetaObject decodes a provider envelope without modifying it.
// Completion builders reuse this one object for every projection; some Claude
// results are megabytes, so reparsing it for status, diff, payload, and stored
// meta is material work rather than a convenience.
func (b *builder) providerMetaObject(evt importir.Event) map[string]json.RawMessage {
	if len(evt.Meta) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(evt.Meta, &obj); err != nil || obj == nil {
		b.warn("import.unreadable-meta", fmt.Sprintf(
			"a %s event carried metadata that is not a JSON object; the row imported without it", evt.Kind))
		return nil
	}
	return obj
}

// metaString reads one top-level string key off an event meta, returning
// "" when the meta is absent, not an object, or the key is missing or
// not a string.
func metaString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return ""
	}
	return metaStringObject(obj, key)
}

func metaStringObject(obj map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(obj[key], &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// providerMetaString is providerMeta as the string items.meta holds; ""
// for meta that is absent or not a JSON object, matching triage's
// validJSONObjectString.
func (b *builder) providerMetaString(evt importir.Event) string {
	return string(b.providerMeta(evt))
}

// mergeMetaKeys sets top-level keys on a meta object, preserving the
// rest. Used for the correlation keys triage stamps outside the
// provider's own meta (provider_item_id, provider_parent_uuid).
func mergeMetaKeys(existing string, keys map[string]string) (string, error) {
	set := map[string]string{}
	for key, value := range keys {
		if strings.TrimSpace(value) != "" {
			set[key] = value
		}
	}
	if len(set) == 0 {
		return existing, nil
	}
	merged := map[string]json.RawMessage{}
	if strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &merged); err != nil {
			return "", fmt.Errorf("decode item meta: %w", err)
		}
		if merged == nil {
			merged = map[string]json.RawMessage{}
		}
	}
	for key, value := range set {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		merged[key] = encoded
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func scopeKey(turnIndex int, scope string) string {
	return fmt.Sprintf("%d|%s", turnIndex, scope)
}

// streamKey identifies one text/thinking block. A provider item id makes
// blocks distinct within a turn+scope; without one, consecutive events
// accumulate into a single block exactly as consecutive live deltas do.
func streamKey(turnIndex int, scope, providerItemID string) string {
	if providerItemID == "" {
		return scopeKey(turnIndex, scope)
	}
	return fmt.Sprintf("%d|%s|provider:%s", turnIndex, scope, providerItemID)
}
