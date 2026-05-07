package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// payloadKindToolCallInput tags the lazy-loaded sibling row that
// holds heavy fields promoted out of items.meta.input by
// applyToolMetaRule (Edit `old_string`/`new_string`, Write `content`,
// MultiEdit `edits`, NotebookEdit `new_source`, ...). The frontend
// fetches it on demand via GetPayloadData using
// items.input_payload_id.
const payloadKindToolCallInput = "tool_call_input"

// ToolMetaRule decides how a tool's `meta.input` map is shaped before
// it is persisted into items.meta and emitted to the frontend.
//
// Two operations apply, in order:
//
//  1. PromoteToPayload lists `input.*` keys whose values are extracted
//     into a payloads row of kind "tool_call_input" referenced by
//     items.input_payload_id. Promoted fields are removed from the
//     items.meta input map.
//  2. KeepInput is the allowlist of `input.*` keys that survive in
//     items.meta after promotion. nil means "keep every non-promoted
//     field" (used as the safe default for unknown tools / tools whose
//     inputs are already curated upstream).
//
// A rule with empty KeepInput AND empty PromoteToPayload is a no-op and
// behaves identically to "no rule". Top-level meta keys (`toolName`,
// `task_id`, `is_background`, `assistant_message_id`,
// `is_inline_subagent`, ...) are never touched — only meta.input.
type ToolMetaRule struct {
	KeepInput        []string
	PromoteToPayload []string
}

// toolMetaRules is the canonical per-tool registry. Lookup key is the
// tool name as it appears on `meta.toolName`:
//   - Claude: the literal `block.Name` (Edit, Write, Bash, ...).
//   - Codex: the toolName stamped by codexItemMetaExtras
//     (file_change, Bash, WebSearch, ...).
//
// Heavy-input tools (Edit/Write/MultiEdit/NotebookEdit) are the
// primary target — their `old_string` / `new_string` / `content` /
// `edits` / `new_source` fields can be megabytes individually and the
// frontend never reads them off items.meta directly. Promotion keeps
// items.meta a few KB while still preserving the original bytes for
// debugging / future surfaces via the payload row.
//
// Codex tool names whose inputs are already curated upstream (see
// internal/provider/codex/protocol.go fileChangeMetaExtras et al.) are
// intentionally absent: the registry's "no entry" fallback is
// pass-through, which is correct for them.
var toolMetaRules = map[string]ToolMetaRule{
	"Edit": {
		KeepInput:        []string{"file_path"},
		PromoteToPayload: []string{"old_string", "new_string", "replace_all"},
	},
	"Write": {
		KeepInput:        []string{"file_path"},
		PromoteToPayload: []string{"content"},
	},
	"MultiEdit": {
		KeepInput:        []string{"file_path"},
		PromoteToPayload: []string{"edits"},
	},
	"NotebookEdit": {
		KeepInput:        []string{"notebook_path", "cell_id", "edit_mode", "cell_type"},
		PromoteToPayload: []string{"new_source"},
	},
	"Bash": {
		KeepInput: []string{"command", "description", "run_in_background", "cwd", "timeout"},
	},
	"Read": {
		KeepInput: []string{"file_path", "limit", "offset"},
	},
	"Grep": {
		KeepInput: []string{
			"pattern", "path", "glob", "type", "output_mode",
			"head_limit", "multiline",
			"-n", "-i", "-A", "-B", "-C",
		},
	},
	"Glob": {
		KeepInput: []string{"pattern", "path"},
	},
	"AskUserQuestion": {
		KeepInput: []string{"questions"},
	},
	"WebSearch": {
		KeepInput: []string{"query", "allowed_domains", "blocked_domains"},
	},
	"WebFetch": {
		KeepInput: []string{"url", "prompt"},
	},
	"Task": {
		KeepInput: []string{"description", "prompt", "subagent_type"},
	},
	"Agent": {
		KeepInput: []string{"description", "prompt", "subagent_type"},
	},
}

// applyToolMetaRule trims `meta.input.*` per the registry entry for
// toolName and, if the rule promotes fields, returns a tool_call_input
// payload carrying the original bytes.
//
// Behavior:
//   - Empty/missing meta, missing rule, or missing meta.input → return
//     `meta` unchanged with a nil payload (zero work).
//   - meta.input is not a JSON object (e.g. string/array) → leave it
//     alone; we can't shape what we can't decode.
//   - Promotion key absent from input → no-op for that key.
//   - All non-promoted keys are kept when KeepInput is nil; otherwise
//     keys outside the allowlist are dropped.
//
// `now` stamps the payload's CreatedAt so it lines up with the
// owning item's created_at (matches the existing payload_items.go
// pattern of payload.CreatedAt == item.CreatedAt).
func applyToolMetaRule(toolName string, raw json.RawMessage, now int64) (json.RawMessage, *store.Payload, error) {
	if len(raw) == 0 {
		return raw, nil, nil
	}
	rule, ok := toolMetaRules[strings.TrimSpace(toolName)]
	if !ok {
		return raw, nil, nil
	}
	if len(rule.KeepInput) == 0 && len(rule.PromoteToPayload) == 0 {
		return raw, nil, nil
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, fmt.Errorf("decode tool meta for %s: %w", toolName, err)
	}
	if top == nil {
		return raw, nil, nil
	}

	inputRaw, hasInput := top["input"]
	if !hasInput || len(inputRaw) == 0 {
		return raw, nil, nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return raw, nil, nil
	}
	if input == nil {
		return raw, nil, nil
	}

	keepSet := stringSet(rule.KeepInput)
	promoteSet := stringSet(rule.PromoteToPayload)

	promoted := make(map[string]json.RawMessage, len(promoteSet))
	trimmed := make(map[string]json.RawMessage, len(input))
	for key, value := range input {
		if _, isPromoted := promoteSet[key]; isPromoted {
			promoted[key] = value
			continue
		}
		if rule.KeepInput == nil {
			trimmed[key] = value
			continue
		}
		if _, kept := keepSet[key]; kept {
			trimmed[key] = value
		}
	}

	encodedTrimmed, err := json.Marshal(trimmed)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal trimmed tool input for %s: %w", toolName, err)
	}
	top["input"] = encodedTrimmed
	out, err := json.Marshal(top)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal trimmed tool meta for %s: %w", toolName, err)
	}

	if len(promoted) == 0 {
		return out, nil, nil
	}

	data, err := json.Marshal(promoted)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal promoted tool input for %s: %w", toolName, err)
	}
	payload := &store.Payload{
		ID:        uuid.NewString(),
		Kind:      payloadKindToolCallInput,
		Meta:      buildToolCallInputPayloadMeta(toolName, promoted),
		Data:      data,
		CreatedAt: now,
	}
	return out, payload, nil
}

// buildToolCallInputPayloadMeta returns a small JSON object describing
// the promoted bytes (one entry per promoted key with its byte count
// plus a total). Mirrors the discipline diff/command_output meta uses:
// meta is cheap, data is heavy. The frontend can render
// "Input was X KB, click to expand" without fetching the data blob.
func buildToolCallInputPayloadMeta(toolName string, promoted map[string]json.RawMessage) string {
	bytesPerKey := make(map[string]int, len(promoted))
	total := 0
	for key, value := range promoted {
		size := len(value)
		bytesPerKey[key] = size
		total += size
	}
	meta := struct {
		ToolName string         `json:"toolName"`
		Bytes    map[string]int `json:"bytes,omitempty"`
		Total    int            `json:"total"`
	}{
		ToolName: toolName,
		Bytes:    bytesPerKey,
		Total:    total,
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// shapeToolItemMeta runs the per-tool meta rule against item.Meta in
// place and returns the optional tool_call_input payload to persist
// alongside the item.
//
// The function is idempotent and lifecycle-aware:
//
//   - Fresh launch (item.InputPayloadID == ""): the rule may produce a
//     payload, which the caller persists via persistItemWithInputPayload;
//     item.Meta is rewritten to the trimmed form.
//   - Re-discovered launch / completion merge / metaUpdateOnly path
//     (item.InputPayloadID != ""): item.Meta is rewritten to the
//     trimmed form so a re-merge of full input bytes can't re-bloat
//     the row, but any freshly-extracted payload is dropped — the
//     existing payload is canonical.
//
// Tool calls without a registry entry, with non-decodable meta, or
// without a meta.input field are returned unchanged.
//
// This is a Router method only because shape errors should be logged
// against the triage subsystem; the trim work itself lives in
// applyToolMetaRule and stays a pure function.
func (r *Router) shapeToolItemMeta(item *store.Item, now int64) *store.Payload {
	if item == nil {
		return nil
	}
	toolName := strings.TrimSpace(item.ToolName)
	if toolName == "" {
		return nil
	}
	raw := json.RawMessage(item.Meta)
	if len(raw) == 0 {
		return nil
	}
	trimmed, payload, err := applyToolMetaRule(toolName, raw, now)
	if err != nil {
		// Failure here is non-fatal: keep the launch event going with
		// the un-shaped meta, but log loudly so we notice and can fix
		// the registry entry.
		log.Printf("triage: shape tool meta for %s/%s: %v", toolName, item.ID, err)
		return nil
	}
	if len(trimmed) > 0 {
		item.Meta = string(trimmed)
	}
	if item.InputPayloadID != "" {
		// Already has a canonical input payload from the launch row;
		// drop the freshly-extracted one to avoid duplicate writes.
		return nil
	}
	return payload
}
