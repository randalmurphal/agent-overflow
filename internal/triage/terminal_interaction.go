package triage

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// terminal_interaction.go — Codex-only "Waited for background terminal"
// row persistence.
//
// Codex emits `TerminalInteractionNotification` whenever the model calls
// `write_stdin` against a backgrounded unified-exec PTY. The signal has
// two variants on the wire:
//
//   - `stdin == ""`: the model polled the PTY without sending input.
//     Codex's own TUI renders a "Waited for background terminal" cell
//     (chatwidget.rs:618). This is the variant Phase 6 persists.
//   - `stdin != ""`: keystrokes were forwarded. Phase 6 drops these —
//     a future phase can render "Interacted with background terminal"
//     without a parser change.
//
// Each empty-stdin event persists one row. If Codex polls ten times we
// show ten cells; matching Codex's TUI behavior (the
// `unified_exec_wait_streak` tracker only collapses runs visually, not
// at the event level). Preemptive grouping / deduplication is a
// separate UX decision.

// terminalInteractionMeta is the Meta shape populated by
// buildTerminalInteractionMeta in the Codex parser. Only the fields we
// actually read are listed; unknown keys are tolerated.
type terminalInteractionMeta struct {
	ProcessID string `json:"process_id"`
	Stdin     string `json:"stdin"`
}

func decodeTerminalInteractionMeta(raw json.RawMessage) terminalInteractionMeta {
	var decoded terminalInteractionMeta
	if len(raw) == 0 {
		return decoded
	}
	_ = json.Unmarshal(raw, &decoded)
	return decoded
}

// handleTerminalInteraction routes Codex's `TerminalInteractionNotification`
// events. Only the empty-stdin (polling) variant persists a timeline row
// — the non-empty variant stays observable via the event stream but
// doesn't leave a persisted marker until a future phase decides what to
// render for it.
//
// Correlation rules:
//
//   - Requires an ACTIVE open turn on the router. A live Codex session
//     only emits terminal interaction while a turn is in flight (the
//     model is producing the write_stdin call inside the turn), so
//     "no open turn" means the event arrived out-of-order or after
//     turn_complete — neither is a valid home for a "waited" row. We
//     log and drop rather than fabricate one. We intentionally do NOT
//     fall back to store.LastTurnIndex: attaching a live
//     terminal_interaction to a CLOSED turn would make the completion
//     divider appear below a row that happened AFTER it.
//   - Stable id `waited:<processID>:<turn_index>:<seq>` so replay of
//     the same notification at the same position upserts in place
//     instead of double-inserting. `seq` starts at 0 and increments
//     per event — within a turn, multiple polls on the SAME processId
//     produce distinct ids so each one is its own row.
func (r *Router) handleTerminalInteraction(evt provider.ProviderEvent) error {
	meta := decodeTerminalInteractionMeta(evt.Meta)

	// Phase 6 only renders the polling variant. Drop anything with
	// non-empty stdin — the event itself has been observed (via
	// SetEventHook in tests, or a future frontend subscriber) but no
	// row is persisted. Strict empty-check (no TrimSpace) matches
	// Codex's own wire-level classifier (see
	// codex-rs/tui/src/chatwidget.rs: `stdin.as_deref().map(str::is_empty)`).
	// Whitespace-only stdin means the model genuinely forwarded those
	// bytes; that's an interaction, not a poll.
	if evt.Content != "" || meta.Stdin != "" {
		return nil
	}

	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		log.Printf("triage: terminal_interaction on %s with no open turn; dropping", evt.ThreadID)
		return nil
	}

	// A terminal interaction is a timeline boundary even though Codex does
	// not emit a normal tool-start event for it. Close the current assistant
	// text/thinking block first so any post-wait assistant text starts a new
	// row instead of appending onto the pre-wait sentence.
	r.settleStreamingBeforeTimelineBoundary(evt, "terminal interaction")

	now := eventTimestampMillis(evt)
	seq := r.nextTerminalInteractionSequence(evt.ThreadID, turnIndex, meta.ProcessID)
	itemID := terminalInteractionID(meta.ProcessID, turnIndex, seq)

	// Store only process_id in the persisted meta. The raw stdin bytes
	// from the wire event MUST NOT land in SQLite — Phase 6 only
	// persists the empty-stdin variant anyway, but being explicit about
	// what goes to the store prevents a future change here from
	// accidentally writing keystrokes that could contain credentials or
	// other sensitive input.
	metaBlob, err := json.Marshal(map[string]any{
		"process_id": meta.ProcessID,
	})
	if err != nil {
		// Unreachable — we're marshaling a literal map — but fall back
		// to a bare default rather than drop the row.
		metaBlob = json.RawMessage(`{}`)
	}

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemTerminalInteraction),
		Role:      "assistant",
		Status:    statusCompleted,
		Summary:   "Waited for background terminal",
		ParentID:  eventParentID(evt),
		Meta:      string(metaBlob),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID); err == nil && found {
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
	} else if err != nil {
		return fmt.Errorf("terminal_interaction existing lookup %s: %w", item.ID, err)
	}
	payload, outputSummary := r.codexCompletedOutputPayloadForProcess(evt.ThreadID, meta.ProcessID, item.ID, evt.Meta, now)
	if outputSummary != "" {
		item.Summary = outputSummary
	}
	if err := r.persistItem(item, payload); err != nil {
		return err
	}
	if payload != nil {
		r.clearCodexCompletedOutputTracker(evt.ThreadID, meta.ProcessID)
	} else {
		r.trackCodexPendingTerminalWait(evt.ThreadID, meta.ProcessID, item.ID, turnIndex, now)
	}
	return nil
}

// terminalInteractionID builds the stable id for a persisted "waited"
// row. Shape: `waited:<processID>:<turn_index>:<seq>`. Encoding
// turn_index and seq in the id keeps replays idempotent — the same
// (processID, turn_index, seq) always maps to the same row, so an
// accidental double-dispatch (session reconnect, event bus fan-out)
// upserts in place rather than inserting a duplicate.
//
// processID CAN be empty on the wire (older Codex revisions omitted it
// from some builds). In that case we substitute the literal "-" so the
// id stays well-formed; multiple waits in the same turn with empty
// processIDs still differentiate by seq.
func terminalInteractionID(processID string, turnIndex, seq int) string {
	if processID == "" {
		processID = "-"
	}
	return fmt.Sprintf("waited:%s:%d:%d", processID, turnIndex, seq)
}

// nextTerminalInteractionSequence returns a monotonically-increasing
// counter per (thread, turn, processID). Stored on the Router so it
// survives across events within a turn but resets on CleanupThread /
// clearOpenTurn via the same prefix sweep that handles other per-turn
// maps.
func (r *Router) nextTerminalInteractionSequence(threadID string, turnIndex int, processID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalInteractionSeq == nil {
		r.terminalInteractionSeq = make(map[string]int)
	}
	key := terminalInteractionSeqKey(threadID, turnIndex, processID)
	seq := r.terminalInteractionSeq[key]
	r.terminalInteractionSeq[key] = seq + 1
	return seq
}

// terminalInteractionSeqKey builds the map key the sequence counter is
// stored under. Mirrors the `<thread>|<turn>|<scope>` shape used by
// other per-turn counters so CleanupThread's prefix-sweep keeps these
// bounded alongside the rest.
func terminalInteractionSeqKey(threadID string, turnIndex int, processID string) string {
	return fmt.Sprintf("%s|%d|%s", threadID, turnIndex, processID)
}
