package claudetui

import "encoding/json"

// reorder.go restores the headless invariant "a tool's start is fed before its
// completion" that claude-tui's two split sources can invert.
//
// Why it inverts (claude-tui only): the gateway forwards each SSE chunk to the
// CLI before teeing it to reconstruction (gateway.stream), and the assembled
// `assistant` envelope — the sole source of EventToolStart — is emitted only at
// request end(). A fast tool (e.g. `echo`) therefore runs, and its PostToolUse
// hook enqueues the `user`/tool_result envelope (EventToolComplete) on the feed
// channel, before the gateway's end() enqueues that `assistant`. Fed in that
// order, triage drops the completion (no launch row yet — see
// tool_lifecycle.persistToolCallCompletion) and the turn-complete force-close
// then marks the orphaned running tool_call errored: a successful command shown
// as failed, its output lost.
//
// Headless never inverts (stdout serializes assistant-before-user in-process),
// so this correction lives entirely in the TUI provider — the shared
// claude.Parser and triage stay untouched.
//
// feedReorder holds a hook tool_result until the `assistant` carrying its
// tool_use_id has been fed, then releases it immediately after. It is driven
// only by feedLoop (a single goroutine), so it needs no lock. Per-turn state is
// reset on the `result` envelope (turn close) so the maps stay bounded.
type feedReorder struct {
	started map[string]bool              // tool_use_ids whose assistant start has been fed
	pending map[string][]json.RawMessage // hook tool_results waiting for their start
}

func newFeedReorder() *feedReorder {
	return &feedReorder{
		started: map[string]bool{},
		pending: map[string][]json.RawMessage{},
	}
}

// streamEventPrefix is the marshaled lead of every streamEventLine envelope
// (struct field order puts "type" first). Live deltas are the hot path and never
// take part in start/complete ordering, so feedLoop uses this prefix to skip
// both the reorder classification and admit's per-envelope parse. reorder_test.go
// asserts streamEventLine still produces this prefix so the coupling can't drift.
var streamEventPrefix = []byte(`{"type":"stream_event"`)

// admit returns the envelopes to feed to the parser NOW, in order. Most pass
// straight through; a hook tool_result whose start has not been fed is held
// (returns nil) and replayed right after the assistant that starts its
// tool_use_id arrives. feedLoop short-circuits the high-frequency stream_event
// envelopes before calling admit, so the full parse here only runs on the
// low-frequency structural envelopes.
func (fr *feedReorder) admit(line json.RawMessage) []json.RawMessage {
	switch envelopeType(line) {
	case "assistant":
		// The assistant envelope is the sole EventToolStart source. Feed it,
		// then release any completion that raced ahead of the tool_use ids it
		// starts.
		out := []json.RawMessage{line}
		for _, id := range assistantToolUseIDs(line) {
			fr.started[id] = true
			if held := fr.pending[id]; len(held) > 0 {
				out = append(out, held...)
				delete(fr.pending, id)
			}
		}
		return out

	case "user":
		// Only hook tool_result envelopes carry a tool_result block; any other
		// `user` envelope passes through. Hold the completion until its start has
		// been fed.
		id, ok := userToolResultID(line)
		if !ok || fr.started[id] {
			return []json.RawMessage{line}
		}
		fr.pending[id] = append(fr.pending[id], line)
		return nil

	case "result":
		// Turn close. Release any straggler — a tool_use_id whose start never
		// arrived (e.g. a gateway error aborted the request before end()) — so
		// nothing is held forever, then reset per-turn state. Feeding a genuine
		// orphan is harmless: triage already drops a completion with no launch
		// row, which is exactly the pre-fix behavior for that (rare) case.
		out := make([]json.RawMessage, 0, len(fr.pending)+1)
		for _, held := range fr.pending {
			out = append(out, held...)
		}
		out = append(out, line)
		fr.started = map[string]bool{}
		fr.pending = map[string][]json.RawMessage{}
		return out

	default:
		return []json.RawMessage{line}
	}
}

// envelopeType reads the top-level "type" of a reconstructed stream-json
// envelope. Returns "" for an unparseable line (which then passes through).
func envelopeType(line json.RawMessage) string {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) != nil {
		return ""
	}
	return head.Type
}

// assistantToolUseIDs returns the ids of the tool_use blocks an assembled
// assistant envelope carries — the ids whose EventToolStart this envelope
// produces in the parser.
func assistantToolUseIDs(line json.RawMessage) []string {
	var env struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &env) != nil {
		return nil
	}
	var ids []string
	for _, block := range env.Message.Content {
		if block.Type == "tool_use" && block.ID != "" {
			ids = append(ids, block.ID)
		}
	}
	return ids
}

// userToolResultID returns the tool_use_id of a hook `user`/tool_result
// envelope, or ok=false for any other `user` envelope. Hook envelopes carry
// exactly one tool_result block (hookmap.postToolUse*Envelope); the first one
// wins.
func userToolResultID(line json.RawMessage) (string, bool) {
	var env struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &env) != nil {
		return "", false
	}
	for _, block := range env.Message.Content {
		if block.Type == "tool_result" && block.ToolUseID != "" {
			return block.ToolUseID, true
		}
	}
	return "", false
}
