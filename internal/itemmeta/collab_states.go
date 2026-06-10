package itemmeta

import (
	"bytes"
	"encoding/json"
)

// Codex collab tool items (spawn_agent / wait_agent / send_input /
// close_agent / resume_agent) carry an agentsStates map on the wire:
// child thread id → CollabAgentState {status, message}, where message
// is the child agent's full final output (codex-rs app-server-protocol
// v2 `CollabAgentState`). The provider adapter copies the map verbatim
// onto meta.input because triage's terminal-evidence readers
// (resolveSubagentsForWait, hasRunningChild in
// internal/triage/codex_background*.go) decode the EVENT meta, which
// persist shaping never mutates — shaping rewrites only the
// store.Item's Meta copy, so those readers see full messages
// regardless of execution order.
//
// Persisting the messages onto items.meta duplicates heavy bytes: the
// same text is the wait completion's lazy tool_call_result payload
// data and each spawn sibling row's payload preview. items.meta ships
// with every windowed list read and stays resident in frontend pane
// memory per loaded item (Core Principle 4), so a subagent's full
// final report would otherwise ride along with the thread forever —
// on the wait carrier, the standalone wait completion, AND any spawn
// row whose completion envelope echoed the map.
//
// TrimCollabAgentStateMessages drops `message` from every object-form
// agentsStates entry at persist time, keeping `status` (and any other
// field). Bare-string entries — the older wire shape, just a status —
// are untouched. Nothing reads messages off a persisted row: recovery
// paths read receiverThreadIds + child_terminal_statuses, and the
// frontend renders agent labels only.
//
// The trim is a fixed point: running it on its own output reports
// changed=false, so completion-merge re-shape paths cannot churn rows.

var agentsStatesKey = []byte(`"agentsStates"`)

// TrimCollabAgentStateMessages returns raw with the agentsStates
// messages dropped as described above, plus whether anything changed.
// Malformed metas are returned unchanged — the caller persists what it
// has rather than dropping state.
func TrimCollabAgentStateMessages(raw []byte) ([]byte, bool) {
	// Cheap pre-check: only Codex collab tool metas carry the map.
	if len(raw) == 0 || !bytes.Contains(raw, agentsStatesKey) {
		return raw, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return raw, false
	}
	inputRaw, ok := top["input"]
	if !ok {
		return raw, false
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &input); err != nil || input == nil {
		return raw, false
	}
	statesRaw, ok := input["agentsStates"]
	if !ok {
		return raw, false
	}
	var states map[string]json.RawMessage
	if err := json.Unmarshal(statesRaw, &states); err != nil || states == nil {
		return raw, false
	}

	changed := false
	for id, entryRaw := range states {
		var entry map[string]json.RawMessage
		if json.Unmarshal(entryRaw, &entry) != nil || entry == nil {
			// Bare-string status (older wire shape) or malformed entry:
			// nothing heavy to drop.
			continue
		}
		if _, hasMessage := entry["message"]; !hasMessage {
			continue
		}
		delete(entry, "message")
		encodedEntry, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		states[id] = encodedEntry
		changed = true
	}
	if !changed {
		return raw, false
	}

	encodedStates, err := json.Marshal(states)
	if err != nil {
		return raw, false
	}
	input["agentsStates"] = encodedStates
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return raw, false
	}
	top["input"] = encodedInput
	out, err := json.Marshal(top)
	if err != nil {
		return raw, false
	}
	return out, true
}
