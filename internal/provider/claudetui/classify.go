package claudetui

import (
	"encoding/json"
	"regexp"
	"strings"
)

// requestClass partitions an outbound /v1/messages request body. Only
// classAgent requests carry a real main-agent-loop turn; the rest are
// auxiliary traffic the interactive CLI emits that the headless stream-json
// surface never shows, so AO must drop them or it renders phantom turns.
//
// This is the Go port of probe_compact.py's is_agent() / ao_transform.py's
// classify(). See docs/architecture/claude-tui-provider.md §Auxiliary-call
// filtering.
type requestClass int

const (
	// classUnparseable: body wasn't valid JSON. Treated as non-agent.
	classUnparseable requestClass = iota
	// classPreflight: the 1-token quota probe (max_tokens <= 1).
	classPreflight
	// classAuxiliary: title / topic generation — no tools.
	classAuxiliary
	// classNestedSubcall: every tool is a dated server tool, so this is a
	// client tool's internal API sub-call (e.g. WebSearch → web_search_*),
	// not a main-loop turn.
	classNestedSubcall
	// classSuggestion: the TUI's next-message autocomplete ("suggestion mode").
	// It carries the full tool set and budget — indistinguishable from a real
	// turn by tools/max_tokens — but its synthetic last user message opens with
	// suggestionMarker. Its response is the model's *prediction of the user's
	// next input*, so surfacing it renders a phantom assistant turn.
	// LIVE-confirmed on 2.1.170.
	classSuggestion
	// classAgent: a real main-agent-loop turn (populated tools, real budget).
	classAgent
)

// String renders the class for the debug classify log (debuglog.go). Kept in
// sync with the const block above.
func (c requestClass) String() string {
	switch c {
	case classPreflight:
		return "preflight"
	case classAuxiliary:
		return "auxiliary"
	case classNestedSubcall:
		return "nested-subcall"
	case classSuggestion:
		return "suggestion"
	case classAgent:
		return "agent"
	default:
		return "unparseable"
	}
}

// serverToolType matches Anthropic server-side tool type discriminators, which
// carry a dated suffix: "web_search_20250305", "bash_20250124". A request whose
// tools are ALL server tools is a nested sub-call, never a main-loop turn.
var serverToolType = regexp.MustCompile(`^[a-z_]+_\d{8}$`)

// toolDef is the subset of a /v1/messages `tools[]` entry we classify on.
type toolDef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (t toolDef) isServerTool() bool {
	return serverToolType.MatchString(t.Type)
}

// messagesRequest is the subset of a /v1/messages request body the
// reconstruction reads: enough to classify the call and to seed the synthetic
// system:init envelope (model + tool names).
type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Tools     []toolDef         `json:"tools"`
	Messages  []json.RawMessage `json:"messages"`
	System    json.RawMessage   `json:"system"`
}

// toolNames returns the declared tool names, for the synthetic system:init.
func (r *messagesRequest) toolNames() []string {
	names := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names
}

// classifyRequest parses a /v1/messages request body and classifies it. The
// parsed body is returned for agent requests (nil otherwise) so callers don't
// re-unmarshal.
func classifyRequest(body []byte) (requestClass, *messagesRequest) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return classUnparseable, nil
	}
	if req.MaxTokens <= 1 {
		return classPreflight, nil
	}
	if len(req.Tools) == 0 {
		return classAuxiliary, nil
	}
	if allServerTools(req.Tools) {
		return classNestedSubcall, nil
	}
	// Suggestion mode looks exactly like a real turn (full tools, full budget);
	// only the marker on its synthetic last user message tells them apart, so
	// this check must come last, after the cheaper tool/budget gates.
	if isSuggestionRequest(req.Messages) {
		return classSuggestion, nil
	}
	return classAgent, &req
}

func allServerTools(tools []toolDef) bool {
	for _, t := range tools {
		if !t.isServerTool() {
			return false
		}
	}
	return len(tools) > 0
}

// suggestionMarker is the prefix Claude Code's next-message autocomplete
// ("suggestion mode") puts on the synthetic last user message of its
// /v1/messages request. The opening "[SUGGESTION MODE: …]" bracket tag is the
// stable, structural part (the full sentence observed on 2.1.170 is
// "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude
// Code.]"); matching the bracket prefix survives wording tweaks to the rest of
// the directive. Confirmed in the 2.1.170 binary and live on the wire
// (spike/claude-mitm). It is the ONLY bracketed-MODE marker in the binary.
const suggestionMarker = "[SUGGESTION MODE:"

// isSuggestionRequest reports whether a /v1/messages body is a suggestion-mode
// autocomplete call rather than a real turn, by the marker on its last user
// message.
func isSuggestionRequest(messages []json.RawMessage) bool {
	return strings.HasPrefix(strings.TrimSpace(lastUserText(messages)), suggestionMarker)
}

// blockText extracts plain text from a /v1/messages content value, which is
// either a JSON string or an array of content blocks ({"type":"text","text":..}).
// Non-text blocks (tool_use, image, …) contribute nothing.
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// lastUserText returns the text of the last role:"user" message — where
// suggestion mode plants its marker, and (for the debug classify log) the
// strongest discriminator between a real turn and an auxiliary call.
func lastUserText(messages []json.RawMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(messages[i], &m) != nil {
			continue
		}
		if m.Role == "user" {
			return blockText(m.Content)
		}
	}
	return ""
}

// firstUserText returns the text of the first role:"user" message. For a
// subagent request that is its task prompt — the Agent tool_use input.prompt
// delivered verbatim — which reconstructor.resolveSubagentParent matches against
// the registered launches to nest the subagent. (The CLI prepends a
// system-reminder, so the match is a substring, not equality.)
func firstUserText(messages []json.RawMessage) string {
	for _, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Role == "user" {
			return blockText(m.Content)
		}
	}
	return ""
}

// subagentSystemMarker is the flag Claude Code sets in a subagent's system
// prompt. The authoritative operational signal that a /v1/messages request is a
// subagent's is the X-Claude-Code-Agent-Id HTTP header (gateway.go) — that also
// yields the agent id needed for correlation. This body-side mirror exists only
// for the credential-free debug classify log, which never sees headers. Both are
// present together on 2.1.170 (spike/claude-mitm).
const subagentSystemMarker = "cc_is_subagent=true"

// isSubagentSystem reports whether a request body's system prompt marks it as a
// subagent. Body-only, for the debug log; routing uses the header.
func isSubagentSystem(system json.RawMessage) bool {
	return strings.Contains(blockText(system), subagentSystemMarker)
}
