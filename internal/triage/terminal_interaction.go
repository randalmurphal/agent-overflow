package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// terminal_interaction.go — Codex-only background terminal interaction row
// persistence.
//
// Codex emits `TerminalInteractionNotification` whenever the model calls
// `write_stdin` against a backgrounded unified-exec PTY. The signal has
// two variants on the wire:
//
//   - `stdin == ""`: the model polled the PTY without sending input.
//     Codex's own TUI keeps this as a status streak. Agent Overflow adds a
//     visible carrier row, so raw write_stdin and canonical typed
//     notifications for the same process reuse one carrier until output lands
//     or another timeline boundary makes the wait stale.
//   - `stdin != ""`: keystrokes were forwarded. We persist an
//     "Interacted with background terminal" marker, but never persist
//     stdin bytes because interactive input can contain secrets.
//
// Empty polls always leave a visible wait carrier. Command item/completed owns
// final output/status; if it arrives while a carrier is pending, the command
// completion row is linked back with `wait_carrier_id` so the frontend can
// indent the completion under the wait. Interactive markers can still carry
// completed output payloads for legacy/non-poll signals.

// terminalInteractionMeta is the Meta shape populated by
// buildTerminalInteractionMeta in the Codex parser. Only the fields we
// actually read are listed; unknown keys are tolerated.
type terminalInteractionMeta struct {
	ProcessID  string `json:"process_id"`
	Stdin      string `json:"stdin"`
	Source     string `json:"source"`
	WaitResult string `json:"wait_result"`
	ToolCallID string `json:"tool_call_id"`
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
// events. Polling variants persist timeline rows while the command is still
// running; when a poll observes completed output, the wait carrier is settled
// and the command completion row is persisted under it. The interactive
// variant records only `has_stdin` and never stores stdin text.
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
//   - Stable ids use `waited:<processID>:<turn_index>:<seq>` for wait
//     carriers and `interacted:<processID>:<turn_index>:<seq>` for forwarded
//     stdin. Empty polls for the same process can reuse their prior id so
//     repeated raw/typed wait signals update the same visible row until output
//     arrives or a later timeline boundary settles it as stale.
func (r *Router) handleTerminalInteraction(evt provider.ProviderEvent) error {
	meta := decodeTerminalInteractionMeta(evt.Meta)
	isPoll := evt.Content == "" && meta.Stdin == ""
	isRawWaitStart := isPoll && meta.Source == "rawResponseItem/function_call"
	isRawWaitOutput := isPoll && meta.Source == "rawResponseItem/function_call_output"
	if isRawWaitOutput {
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
	itemID := ""
	if isRawWaitStart {
		// Raw write_stdin is emitted before Codex waits on the PTY. Seeing
		// it is typed proof that the referenced unified exec is now being
		// resumed as a background terminal, even if no assistant text or
		// follow-up tool boundary has happened yet.
		r.markCodexUnifiedExecProcessBackgrounded(evt.ThreadID, meta.ProcessID)
	}
	if isPoll {
		itemID = r.codexTerminalWaitItemIDForProcess(evt.ThreadID, meta.ProcessID, turnIndex)
	}
	if itemID == "" {
		seq := r.nextTerminalInteractionSequence(evt.ThreadID, turnIndex, meta.ProcessID)
		itemID = terminalInteractionID(meta.ProcessID, turnIndex, seq, isPoll)
	}
	metaMap := map[string]any{
		"process_id": meta.ProcessID,
		"kind":       "terminal_interaction",
	}
	if command := r.codexTerminalCommandForProcess(evt.ThreadID, meta.ProcessID); command != "" {
		metaMap["command"] = command
	}
	if !isPoll {
		metaMap["has_stdin"] = true
	}
	if meta.Source != "" {
		metaMap["source"] = meta.Source
	}
	if meta.WaitResult != "" {
		metaMap["wait_result"] = meta.WaitResult
	}
	if meta.ToolCallID != "" {
		metaMap["tool_call_id"] = meta.ToolCallID
	}
	metaBlob, err := json.Marshal(metaMap)
	if err != nil {
		// Unreachable — we're marshaling a literal map — but fall back
		// to a bare default rather than drop the row.
		metaBlob = json.RawMessage(`{}`)
	}

	summary := "Waited for background terminal"
	if !isPoll {
		summary = "Interacted with background terminal"
	}
	commandSummary := r.codexTerminalSummaryForProcess(evt.ThreadID, meta.ProcessID)
	if commandSummary != "" {
		summary += ": " + commandSummary
	}

	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemTerminalInteraction),
		Role:      "assistant",
		Status:    terminalInteractionStatus(isPoll),
		Summary:   summary,
		ParentID:  eventParentID(evt),
		Meta:      string(metaBlob),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID); err == nil && found {
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
		item.Meta = validJSONObjectString(mergeRawJSONObject(json.RawMessage(existing.Meta), metaBlob))
		existingSettledPoll := isPoll && existing.Status != statusRunning && existing.Status != statusStreaming
		if existingSettledPoll {
			item.Status = existing.Status
		}
		if isPoll && commandSummary == "" && strings.TrimSpace(existing.Summary) != "" {
			item.Summary = existing.Summary
		}
		if existingSettledPoll && meta.WaitResult == "" {
			if err := r.persistItem(item, nil); err != nil {
				return err
			}
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("terminal_interaction existing lookup %s: %w", item.ID, err)
	}
	if isPoll {
		r.rememberCodexTerminalWaitCarrier(evt.ThreadID, meta.ProcessID, item.ID, turnIndex, now)
		if meta.WaitResult == provider.TerminalWaitResultRunning {
			item.Status = statusCompleted
			if err := r.persistItem(item, nil); err != nil {
				return err
			}
			if err := r.clearCodexPendingTerminalWait(evt.ThreadID, meta.ProcessID); err != nil {
				return err
			}
			return nil
		}
		if tracker, ok := r.codexCompletedUnifiedExecTrackerForProcess(evt.ThreadID, meta.ProcessID); ok {
			item.Status = statusCompleted
			if err := r.persistItem(item, nil); err != nil {
				return err
			}
			if err := r.persistCodexUnifiedExecCompletion(evt, tracker, turnIndex, item.ID); err != nil {
				return err
			}
			r.clearCodexCompletedOutputTracker(evt.ThreadID, meta.ProcessID)
			return nil
		}
		if meta.WaitResult == provider.TerminalWaitResultExited {
			item.Status = statusCompleted
		}
	}
	var payload *store.Payload
	var outputSummary string
	if !isPoll {
		payload, outputSummary = r.codexCompletedOutputPayloadForProcess(evt.ThreadID, meta.ProcessID, item.ID, evt.Meta, now)
	}
	if outputSummary != "" {
		if !isPoll {
			outputSummary = strings.Replace(outputSummary, "Waited for background terminal", "Interacted with background terminal", 1)
		}
		item.Summary = outputSummary
	}
	if payload != nil {
		r.clearCodexCompletedOutputTracker(evt.ThreadID, meta.ProcessID)
	} else if isPoll {
		if !r.trackCodexPendingTerminalWait(evt.ThreadID, meta.ProcessID, item.ID, turnIndex, now) {
			item.Status = statusCompleted
		}
	}
	if err := r.persistItem(item, payload); err != nil {
		return err
	}
	return nil
}

func terminalInteractionStatus(isPoll bool) string {
	if isPoll {
		return statusRunning
	}
	return statusCompleted
}

// terminalInteractionID builds the id for a persisted terminal interaction
// row after the caller has chosen a sequence. Shape:
// `<kind>:<processID>:<turn_index>:<seq>`. Empty poll reuse is owned by
// codexTerminalWaitItemIDForProcess; forwarded stdin interactions always take
// a fresh sequence.
//
// processID CAN be empty on the wire (older Codex revisions omitted it
// from some builds). In that case we substitute the literal "-" so the
// id stays well-formed; multiple waits in the same turn with empty
// processIDs still differentiate by seq.
func terminalInteractionID(processID string, turnIndex, seq int, poll bool) string {
	if processID == "" {
		processID = "-"
	}
	prefix := "interacted"
	if poll {
		prefix = "waited"
	}
	return fmt.Sprintf("%s:%s:%d:%d", prefix, processID, turnIndex, seq)
}

// nextTerminalInteractionSequence returns a monotonically-increasing counter
// per (thread, turn, processID). Stored on the Router so it survives
// clearOpenTurn and multi-result boundaries; CleanupThread / selective
// turn re-init reset sweep the key by prefix.
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
