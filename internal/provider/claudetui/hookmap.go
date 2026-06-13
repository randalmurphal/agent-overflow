package claudetui

import "encoding/json"

// hookmap.go converts Claude Code hook payloads into the stream-json envelopes
// the shared claude.Parser consumes. These are pure functions; the relay in
// hookrelay.go owns transport, the capability token, and the blocking
// answer-back. See docs/architecture/claude-tui-provider.md §The hook relay.

// hookPayload is the subset of a hook's stdin JSON the reconstruction reads.
// Field set spans every event we register; absent fields stay zero.
type hookPayload struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	Error         string          `json:"error"`
	IsInterrupt   bool            `json:"is_interrupt"`

	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`

	// PreCompact / PostCompact.
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"custom_instructions"`
}

func parseHookPayload(raw []byte) (hookPayload, error) {
	var p hookPayload
	err := json.Unmarshal(raw, &p)
	return p, err
}

// --- envelope shapes (what claude.Parser reads) ---------------------------

type userEnvelope struct {
	Type          string          `json:"type"` // "user"
	Message       userMessage     `json:"message"`
	ToolUseResult json.RawMessage `json:"tool_use_result,omitempty"`
}

type userMessage struct {
	Role    string            `json:"role"` // "user"
	Content []json.RawMessage `json:"content"`
}

type toolResultBlock struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// --- mappings -------------------------------------------------------------

// postToolUseEnvelope reconstructs the `user` tool_result envelope from a
// successful PostToolUse. tool_response is carried verbatim as the
// tool_use_result sibling so parse_user.go gets the rich enrichment
// (exit_code, structuredPatch, task) the same as the headless path.
func postToolUseEnvelope(p hookPayload) json.RawMessage {
	block := toolResultBlock{
		Type:      "tool_result",
		ToolUseID: p.ToolUseID,
		Content:   toolResponseText(p.ToolResponse),
	}
	return mustMarshal(userEnvelope{
		Type:          "user",
		Message:       userMessage{Role: "user", Content: []json.RawMessage{mustMarshal(block)}},
		ToolUseResult: nonEmptyObject(p.ToolResponse),
	})
}

// postToolUseFailureEnvelope reconstructs the tool_result envelope for a
// PostToolUseFailure. There is no tool_response on failure; the error string
// becomes the result content and is_error flags the failed completion.
func postToolUseFailureEnvelope(p hookPayload) json.RawMessage {
	block := toolResultBlock{
		Type:      "tool_result",
		ToolUseID: p.ToolUseID,
		Content:   p.Error,
		IsError:   true,
	}
	return mustMarshal(userEnvelope{
		Type:    "user",
		Message: userMessage{Role: "user", Content: []json.RawMessage{mustMarshal(block)}},
	})
}

// compactBoundaryEnvelope reconstructs a system:compact_boundary from a
// PostCompact hook. The hook carries only the trigger; richer compactMetadata
// (preTokens/postTokens) is a wire-side enrichment that v1 does not fold in, so
// the boundary marks the timeline with the trigger alone.
func compactBoundaryEnvelope(p hookPayload) json.RawMessage {
	md := map[string]any{}
	if p.Trigger != "" {
		md["trigger"] = p.Trigger
	}
	return mustMarshal(map[string]json.RawMessage{
		"type":            mustMarshal("system"),
		"subtype":         mustMarshal("compact_boundary"),
		"compactMetadata": mustMarshal(md),
	})
}

// askUserQuestionControlRequest reconstructs the can_use_tool control_request
// for an AskUserQuestion PreToolUse, so parse_control.go emits the same
// EventUserInputRequest the headless path produces. requestID correlates the
// surfaced request to the blocked hook awaiting the human's answer.
func askUserQuestionControlRequest(requestID string, p hookPayload) json.RawMessage {
	req := map[string]json.RawMessage{
		"subtype":   mustMarshal("can_use_tool"),
		"tool_name": mustMarshal("AskUserQuestion"),
		"input":     nonEmptyObject(p.ToolInput),
	}
	if p.ToolUseID != "" {
		req["tool_use_id"] = mustMarshal(p.ToolUseID)
	}
	return mustMarshal(map[string]json.RawMessage{
		"type":       mustMarshal("control_request"),
		"request_id": mustMarshal(requestID),
		"request":    mustMarshal(req),
	})
}

// --- helpers --------------------------------------------------------------

// toolResponseText derives the display text for a tool_result block from a
// PostToolUse tool_response. A JSON string is used directly; an object with a
// `stdout` field (Bash and friends) surfaces stdout; anything else falls back
// to the compact JSON so nothing is silently lost. The structured fields the UI
// needs (exit_code, diffs) ride on the tool_use_result sibling regardless.
func toolResponseText(resp json.RawMessage) string {
	if len(resp) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(resp, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(resp, &obj) == nil {
		if out, ok := decodeString(obj["stdout"]); ok {
			if errOut, ok := decodeString(obj["stderr"]); ok && errOut != "" {
				if out != "" {
					return out + "\n" + errOut
				}
				return errOut
			}
			return out
		}
	}
	return string(resp)
}

func decodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

// nonEmptyObject returns raw when it is a non-empty JSON value, else nil so the
// omitempty fields drop out instead of emitting a literal `null`.
func nonEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}
