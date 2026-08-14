package sessionimport

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// turnState assembles the `turns` rows of one imported branch.
//
// Both providers describe turns, differently: Codex says so explicitly
// (task_started / task_complete / turn_aborted), while a Claude
// transcript only implies them — a top-level user message opens one and
// the assistant's final result closes it. The writer therefore allocates
// turn indices itself rather than trusting the events, and every
// unsettled turn is closed before the batch is sealed: a completed_at of
// NULL means "in flight right now" to the rest of the app, and the boot
// sweep would flip an imported thread's history to "interrupted".
type turnState struct {
	thread store.Thread

	turns       []store.Turn
	completions []store.TurnCompletion
	usage       []store.UsageLedgerRow

	nextIndex int

	openIndex  int
	openTurnID string
	// openEventTurnID is the reader's correlation id for the open turn. It
	// is normally Codex's wire id, but inferred rollout turns also receive an
	// internal id even though provider_turn_id stays empty. This is what keeps
	// a mid-turn user message inside the turn the reader says it belongs to.
	openEventTurnID string
	openSettled     bool
	lastActivity    map[int]int64
	turnIDByIdx     map[int]string

	// taken is every turn id that is already on the thread, plus every one
	// this batch has claimed. `turns.turn_id` is a primary key and
	// ApplyImportBatch INSERTs, so a duplicate is a transaction failure —
	// and PlanUpdate promises a refresh will apply before it runs.
	taken map[string]struct{}
	// err is the first id collision. It is sticky and surfaces from
	// Writer.Build, which is the one exit: a batch that would fail on
	// INSERT must never reach the caller as a plan.
	err error
}

func newTurnState(thread store.Thread, firstIndex int, taken map[string]struct{}) turnState {
	claimed := make(map[string]struct{}, len(taken))
	for id := range taken {
		claimed[id] = struct{}{}
	}
	return turnState{
		thread:       thread,
		nextIndex:    firstIndex,
		lastActivity: map[int]int64{},
		turnIDByIdx:  map[int]string{},
		taken:        claimed,
	}
}

// failure reports the first turn-id collision this batch hit, if any.
func (t *turnState) failure() error { return t.err }

// claim records a turn id, refusing one the thread already holds.
func (t *turnState) claim(turnID string) {
	if _, clash := t.taken[turnID]; clash {
		if t.err == nil {
			t.err = fmt.Errorf(
				"the session file re-opens turn %q, which this thread already imported "+
					"(import the session again to pick up its newer messages)", turnID)
		}
		return
	}
	t.taken[turnID] = struct{}{}
}

// current returns the open turn index, or 0 when no turn is open.
func (t *turnState) current() int { return t.openIndex }

// currentFor returns the turn an item-producing event belongs to,
// opening one when the session file gave no explicit boundary. Imports
// see this on Claude transcripts (which have no turn-start row at all)
// and for anything a provider emits before its first task_started.
func (t *turnState) currentFor(evt importir.Event) int {
	if t.openIndex != 0 {
		return t.openIndex
	}
	now := evt.Timestamp.UnixMilli()
	return t.open(evt, now)
}

// continuesOpenTurn reports whether an event names the turn that is
// already open. The provider's own turn id is authoritative when it has
// one: a second `turns` row under one wire id is a primary-key failure,
// and the two rows would split one turn's history in the UI.
func (t *turnState) continuesOpenTurn(evt importir.Event) bool {
	id := strings.TrimSpace(evt.TurnID)
	return id != "" && id == t.openEventTurnID
}

// startTurnForUserText opens the turn a top-level user prompt drives.
//
// Two open turns are reused rather than closed: one that has produced no
// rows yet (Codex opens the turn before delivering the message), and one
// the prompt itself names — a user message arriving mid-turn is a STEER,
// which Codex records inside the turn it interrupted.
func (t *turnState) startTurnForUserText(evt importir.Event, now int64, openHasRows bool) int {
	if t.openIndex != 0 && (!openHasRows || t.continuesOpenTurn(evt)) {
		return t.openIndex
	}
	t.closeOpen(now)
	return t.open(evt, now)
}

// startExplicit opens the turn an explicit provider turn-start names.
func (t *turnState) startExplicit(evt importir.Event, now int64, openHasRows bool) int {
	if t.openIndex != 0 && t.continuesOpenTurn(evt) {
		// A repeat boundary for the turn already open.
		return t.openIndex
	}
	if t.openIndex != 0 && !openHasRows {
		// A turn was opened implicitly for content that arrived before
		// this boundary; adopt it rather than stranding an empty row.
		t.turnIDByIdx[t.openIndex] = t.adoptTurnID(evt, t.openIndex)
		return t.openIndex
	}
	t.closeOpen(now)
	return t.open(evt, now)
}

func (t *turnState) open(evt importir.Event, startedAt int64) int {
	index := t.nextIndex
	t.nextIndex++
	eventTurnID := strings.TrimSpace(evt.TurnID)
	turnID := resolveTurnID(t.thread.ID, eventTurnID, index)
	t.claim(turnID)
	t.turns = append(t.turns, store.Turn{
		TurnID:         turnID,
		ThreadID:       t.thread.ID,
		TurnIndex:      index,
		StartedAt:      startedAt,
		ProviderTurnID: providerTurnID(evt),
	})
	t.openIndex = index
	t.openTurnID = turnID
	t.openEventTurnID = eventTurnID
	t.openSettled = false
	t.turnIDByIdx[index] = turnID
	t.lastActivity[index] = startedAt
	return index
}

// adoptTurnID rewrites an implicitly-opened turn's ids once the provider
// names it. The row has not been written yet, so this is a fix-up of the
// pending batch, not an update.
func (t *turnState) adoptTurnID(evt importir.Event, index int) string {
	eventTurnID := strings.TrimSpace(evt.TurnID)
	if eventTurnID == "" {
		return t.turnIDByIdx[index]
	}
	turnID := resolveTurnID(t.thread.ID, eventTurnID, index)
	for i := range t.turns {
		if t.turns[i].TurnIndex != index {
			continue
		}
		// The synthesized id this row was opened with is released and the
		// provider's own id claimed in its place — an adopt is a rename of
		// a row that has not been written yet, not a second turn.
		delete(t.taken, t.turns[i].TurnID)
		t.claim(turnID)
		t.turns[i].TurnID = turnID
		t.turns[i].ProviderTurnID = providerTurnID(evt)
		t.openTurnID = turnID
		t.openEventTurnID = eventTurnID
		return turnID
	}
	return t.turnIDByIdx[index]
}

func (t *turnState) touch(turnIndex int, at int64) {
	if at > t.lastActivity[turnIndex] {
		t.lastActivity[turnIndex] = at
	}
}

// closeOpen settles a turn the session file never explicitly finished.
// completed_at is the last timestamp the turn produced — the sidebar
// orders on that column, so restamping it would float the thread — and
// stop_reason stays empty because nothing in the file reported one.
func (t *turnState) closeOpen(fallbackAt int64) {
	if t.openIndex == 0 || t.openSettled {
		t.openIndex = 0
		return
	}
	completedAt := t.lastActivity[t.openIndex]
	if completedAt == 0 {
		completedAt = fallbackAt
	}
	t.completions = append(t.completions, store.TurnCompletion{
		TurnID:      t.openTurnID,
		CompletedAt: completedAt,
	})
	t.openIndex = 0
	t.openSettled = true
}

// settle records the provider's own turn completion.
func (t *turnState) settle(evt importir.Event, now int64, meta turnCompletion) {
	if t.openIndex == 0 {
		return
	}
	completedAt := now
	if completedAt == 0 {
		completedAt = t.lastActivity[t.openIndex]
	}
	t.completions = append(t.completions, store.TurnCompletion{
		TurnID:             t.openTurnID,
		CompletedAt:        completedAt,
		StopReason:         meta.stopReason,
		AssistantMessageID: meta.assistantMessageID,
		TokenUsageJSON:     meta.tokenUsageJSON,
		ErrorMessage:       meta.errorMessage,
	})
	t.usage = append(t.usage, usageRows(t.thread, t.openTurnID, completedAt, meta.modelUsage)...)
	t.openSettled = true
	t.openIndex = 0
}

// resolveTurnID mirrors triage's rule: the durable row id is always scoped to
// its thread, while turns.provider_turn_id preserves the provider's wire id.
func resolveTurnID(threadID, providerTurnID string, turnIndex int) string {
	return store.ScopedTurnID(threadID, providerTurnID, turnIndex)
}

// providerTurnID returns only an id the provider can accept again. The Codex
// rollout reader assigns an internal id to inferred turns so later events can
// correlate with them, but marks those starts explicitly; persisting that
// invented id as a provider fork/revert anchor would promise an RPC identity
// Codex never issued.
func providerTurnID(evt importir.Event) string {
	id := strings.TrimSpace(evt.TurnID)
	if id == "" || len(evt.Meta) == 0 {
		return id
	}
	var meta struct {
		Synthetic bool `json:"import_synthetic_turn"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err == nil && meta.Synthetic {
		return ""
	}
	return id
}

// turnCompletion is the decoded slice of an EventTurnComplete the turn
// row needs. It is the import-side twin of triage's turnCompleteMeta,
// reading the same typed provider payloads.
type turnCompletion struct {
	stopReason         string
	assistantMessageID string
	tokenUsageJSON     string
	errorMessage       string
	modelUsage         []provider.ModelTokenUsage
}

func decodeTurnCompletion(evt importir.Event) (turnCompletion, error) {
	switch meta := evt.TurnComplete.(type) {
	case *provider.WireTurnCompleteMeta:
		if meta == nil {
			return turnCompletion{}, fmt.Errorf("turn_complete carries a nil wire payload")
		}
		usage := ""
		if meta.Usage != nil {
			encoded, err := json.Marshal(meta.Usage)
			if err != nil {
				return turnCompletion{}, fmt.Errorf("encode turn usage: %w", err)
			}
			usage = string(encoded)
		}
		return turnCompletion{
			stopReason:         triage.CanonicalStopReason(meta.StopReason, meta.Aborted),
			assistantMessageID: meta.AssistantMessageID,
			tokenUsageJSON:     usage,
			errorMessage:       meta.ErrorMessage,
			modelUsage:         meta.ModelUsage,
		}, nil
	case *provider.SoftRoundCloseMeta:
		if meta == nil {
			return turnCompletion{}, fmt.Errorf("turn_complete carries a nil soft-close payload")
		}
		return turnCompletion{
			stopReason:         triage.CanonicalStopReason(meta.StopReason, false),
			assistantMessageID: meta.AssistantMessageID,
		}, nil
	case *provider.TruncatedTurnCompleteMeta:
		if meta == nil {
			return turnCompletion{}, fmt.Errorf("turn_complete carries a nil truncated payload")
		}
		return turnCompletion{
			stopReason:   triage.CanonicalStopReason("", true),
			errorMessage: meta.ErrorMessage,
		}, nil
	case nil:
		return turnCompletion{}, fmt.Errorf("turn_complete carries no typed payload")
	default:
		return turnCompletion{}, fmt.Errorf("turn_complete payload type %T is not supported", evt.TurnComplete)
	}
}

func (b *builder) turnStart(evt importir.Event) error {
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	open := b.turns.current()
	b.closeStreams(open, "")
	b.turns.startExplicit(evt, now, b.itemIndex[open] > 0)
	return nil
}

func (b *builder) turnComplete(evt importir.Event) error {
	meta, err := decodeTurnCompletion(evt)
	if err != nil {
		return err
	}
	now, err := timestamp(evt)
	if err != nil {
		return err
	}
	open := b.turns.current()
	if open == 0 {
		b.warn("import.orphan-turn-complete",
			"a turn completion arrived with no turn open and was dropped")
		return nil
	}
	b.closeStreams(open, "")
	b.forceCloseOrphanTools(open, now)
	b.turns.settle(evt, now, meta)
	return nil
}
