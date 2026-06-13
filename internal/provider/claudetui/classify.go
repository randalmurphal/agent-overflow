package claudetui

import (
	"encoding/json"
	"regexp"
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
	// classAgent: a real main-agent-loop turn (populated tools, real budget).
	classAgent
)

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
