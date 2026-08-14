// Package itemmeta holds shaping helpers for the persisted items.meta
// JSON column — logic shared by the triage write path and the store
// migration chain, which cannot import each other (triage imports
// store). Stdlib-only so either side can depend on it without a cycle.
package itemmeta

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Claude closes every tool_use with a user-side tool_result block, and
// the CLI enriches it with a provider-specific tool_use_result sibling
// (stdout/stderr for Bash, file contents + structuredPatch for edits,
// the full final report for Task/Agent subagents).
// internal/provider/claude/parse_user.go forwards both verbatim on the
// completion event's Meta because triage extractors need the full bytes
// at completion time (file-change diff extraction, command payload meta,
// payload data assembly).
//
// Persisting that echo onto items.meta is a different story: items.meta
// ships with every windowed list read and stays resident in frontend
// pane memory for every loaded item (Core Principle 4 — frontend memory
// is bounded by the visible thread). A subagent-heavy session measured
// ~54 MB of items.meta strings, almost entirely these two fields. The
// full result body is not lost — it lives in the lazy tool_call_result /
// command_output payload row keyed by items.payload_id and loads on
// expand.
//
// TrimToolResultEcho bounds the echo at persist time:
//
//   - Success rows drop both fields entirely. Nothing reads them off a
//     persisted success row: rendering uses items.summary plus the
//     payload meta/data.
//   - Failure rows keep a tail-capped excerpt of tool_use_result.stderr
//     / .stdout and tool_result.content, because the frontend's
//     commandErrorForItem legacy fallback chain
//     (frontend/src/lib/components/chat/commandDisplay.ts) reads exactly
//     those paths when no command_output payload meta exists. Tail wins
//     over head because compactErrorMessage renders the LAST lines.
//   - User-input echo tools (AskUserQuestion, request_user_input) are
//     exempt: AskUserQuestionCard reads meta.tool_result.content as the
//     answers echo, and that payload is bounded by construction (the
//     selected options).
//
// The trim is a fixed point: running it on its own output reports
// changed=false, so re-shape on completion merge paths cannot churn
// rows.

// ToolResultExcerptCap bounds each preserved failure excerpt
// (tool_result.content, tool_use_result.stderr, tool_use_result.stdout)
// in bytes. The frontend renders at most the last two lines / 240 chars
// of these; 2 KiB keeps full fidelity for any realistic error tail
// while capping the worst case per item to a few KB.
const ToolResultExcerptCap = 2048

// toolResultEchoExempt reports whether toolName's tool_result echo is
// read in full by the frontend and must survive untrimmed.
func toolResultEchoExempt(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "AskUserQuestion", "request_user_input":
		return true
	default:
		return false
	}
}

var (
	toolResultKey    = []byte(`"tool_result"`)
	toolUseResultKey = []byte(`"tool_use_result"`)
)

// TrimToolResultEcho returns raw with the completion-echo fields bounded
// as described above, plus whether anything changed. Malformed metas are
// returned unchanged — the caller persists what it has rather than
// dropping state.
func TrimToolResultEcho(toolName string, raw []byte) ([]byte, bool) {
	if len(raw) == 0 || toolResultEchoExempt(toolName) {
		return raw, false
	}
	// Cheap pre-check: most metas never carried the echo fields, and
	// already-trimmed success rows carry neither key.
	if !bytes.Contains(raw, toolResultKey) && !bytes.Contains(raw, toolUseResultKey) {
		return raw, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return raw, false
	}
	if !TrimToolResultEchoObject(toolName, top) {
		return raw, false
	}

	out, err := json.Marshal(top)
	if err != nil {
		return raw, false
	}
	return out, true
}

// TrimToolResultEchoObject is the decoded-object form of
// TrimToolResultEcho. It mutates top in place and reports whether it changed.
// Callers that already decoded a completion envelope should use this form so
// multi-megabyte result echoes are not parsed again merely to discard them.
func TrimToolResultEchoObject(toolName string, top map[string]json.RawMessage) bool {
	if top == nil || toolResultEchoExempt(toolName) {
		return false
	}

	keepExcerpt := metaIndicatesFailure(top)
	changed := false
	if value, ok := top["tool_result"]; ok {
		replacement, replaced := trimmedToolResultValue(value, keepExcerpt)
		if replaced {
			changed = true
			if len(replacement) == 0 {
				delete(top, "tool_result")
			} else {
				top["tool_result"] = replacement
			}
		}
	}
	if value, ok := top["tool_use_result"]; ok {
		replacement, replaced := trimmedToolUseResultValue(value, keepExcerpt)
		if replaced {
			changed = true
			if len(replacement) == 0 {
				delete(top, "tool_use_result")
			} else {
				top["tool_use_result"] = replacement
			}
		}
	}
	return changed
}

// metaIndicatesFailure reads the failure signals the completion parser
// stamps at the top level (is_error, exit_code), falling back to the
// nested tool_result.is_error for rows that predate the top-level flag.
func metaIndicatesFailure(top map[string]json.RawMessage) bool {
	if value, ok := top["is_error"]; ok {
		var isError bool
		if json.Unmarshal(value, &isError) == nil && isError {
			return true
		}
	}
	if value, ok := top["exit_code"]; ok {
		var code float64
		if json.Unmarshal(value, &code) == nil && code != 0 {
			return true
		}
	}
	if value, ok := top["tool_result"]; ok {
		var block struct {
			IsError bool `json:"is_error"`
		}
		if json.Unmarshal(value, &block) == nil && block.IsError {
			return true
		}
	}
	return false
}

// trimmedToolResultValue maps the full tool_result block to its bounded
// replacement: nil (drop) on success, `{"content":"<tail excerpt>"}` on
// failure. Returns replaced=false when the value already equals the
// replacement (fixed point for re-shape paths).
func trimmedToolResultValue(raw json.RawMessage, keepExcerpt bool) (json.RawMessage, bool) {
	if !keepExcerpt {
		return nil, true
	}
	excerpt := tailExcerpt(toolResultContentText(raw), ToolResultExcerptCap)
	if excerpt == "" {
		return nil, true
	}
	replacement, err := json.Marshal(map[string]string{"content": excerpt})
	if err != nil {
		return nil, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), replacement) {
		return raw, false
	}
	return replacement, true
}

// trimmedToolUseResultValue maps the full tool_use_result enrichment to
// its bounded replacement: nil (drop) on success, the subset
// `{"stderr":...,"stdout":...}` of tail excerpts on failure. Non-object
// shapes (string / array enrichments) carry no path the frontend reads
// and are dropped outright.
func trimmedToolUseResultValue(raw json.RawMessage, keepExcerpt bool) (json.RawMessage, bool) {
	if !keepExcerpt {
		return nil, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, true
	}
	kept := make(map[string]string, 2)
	for _, key := range []string{"stderr", "stdout"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			continue
		}
		if excerpt := tailExcerpt(text, ToolResultExcerptCap); excerpt != "" {
			kept[key] = excerpt
		}
	}
	if len(kept) == 0 {
		return nil, true
	}
	replacement, err := json.Marshal(kept)
	if err != nil {
		return nil, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), replacement) {
		return raw, false
	}
	return replacement, true
}

// toolResultContentText extracts the human-readable text from a Claude
// tool_result block: `content` is either a plain string or an array of
// content blocks whose text entries join with newlines — the same
// projection the frontend's readNestedString applies, so the preserved
// excerpt renders identically.
func toolResultContentText(raw json.RawMessage) string {
	var block struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &block) != nil || len(block.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(block.Content, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(block.Content, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, entry := range blocks {
		if entry.Text != "" {
			parts = append(parts, entry.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// tailExcerpt returns the trailing maxBytes of s, preferring to start
// just after a line boundary inside the window so consumers that render
// "the last N lines" see exactly the lines the full string produced.
// Falls back to a rune-aligned cut when the window is a single line.
func tailExcerpt(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	tail := s[len(s)-maxBytes:]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx+1 < len(tail) {
		return tail[idx+1:]
	}
	for i := 0; i < len(tail); i++ {
		if utf8.RuneStart(tail[i]) {
			return tail[i:]
		}
	}
	return ""
}
