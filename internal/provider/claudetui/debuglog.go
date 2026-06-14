package claudetui

import (
	"encoding/json"
	"log"

	"agent-overflow/internal/logging"
)

// debuglog.go holds the AGENT_OVERFLOW_DEBUG=provider diagnostics for the
// interactive provider. The headless path logs raw stdio (internal/provider/
// process.go); the TUI speaks no stdio protocol, so its analog is two streams,
// both written to the same provider-events NDJSON the headless provider uses:
//
//   - direction "in":       every reconstructed stream-json envelope fed to the
//                           shared parser — exactly what becomes ProviderEvents
//                           and therefore items. This is the literal analog of
//                           the headless raw-stdout log.
//   - direction "classify": the per-request /v1/messages classification, the
//                           TUI-specific decision headless never makes. It makes
//                           the dropped auxiliary calls (title gen, preflight)
//                           visible alongside the surfaced agent turns, so a
//                           phantom turn can be traced to the request that
//                           produced it and the class it was given.
//   - direction "decision": the routing/reconstruction decision per classAgent
//                           request — the route taken (main vs subagent vs
//                           unmatched-and-forwarded), the Agent card a subagent
//                           resolved to (and via cache vs content match), and
//                           what emitBackgroundCompletions did with an injected
//                           <task-notification> (emitted / deduped / skipped).
//                           These are the branches the "in" feed cannot show on
//                           its own: an unmatched subagent or a skipped
//                           completion emits no envelope at all. See decisionLog.
//
// All three are gated on a non-nil EventLogger (NewProviderEventLogger returns
// nil unless the env opts in), so a production session pays nothing.
//
// SECURITY: every value logged here is derived from a RESPONSE envelope, a
// REQUEST BODY, or the X-Claude-Code-Agent-Id header — the last an opaque
// per-subagent identifier, not a credential. The credential headers
// (Authorization, x-api-key, the OAuth bearer) are never read by any of these
// paths: the envelope feed is body-only, logClassify is handed only the request
// body, and logDecision records only the already-extracted agent id and the
// reconstruction outcome. This is the dev-only, local-only, short-retention
// capture the area guide sanctions; the produced file must never be committed.

// logProvider is the provider tag on debug log entries for this package.
const logProvider = "claude-tui"

// classifyLogPrefixRunes bounds the system / last-user text excerpts so one
// request line stays readable; the full prose still lands in the "in" envelope
// stream when the request is a real agent turn.
const classifyLogPrefixRunes = 200

// logEnvelope records one reconstructed envelope fed to the parser (direction
// "in"). No-op when the debug logger is disabled.
func (s *Session) logEnvelope(line json.RawMessage) {
	if s.evlog == nil {
		return
	}
	if err := s.evlog.LogProviderEvent(logging.ProviderEventEntry{
		ThreadID:  s.threadID,
		Direction: "in",
		Provider:  logProvider,
		Data:      string(line),
	}); err != nil {
		log.Printf("claudetui: envelope log failed: %v", err)
	}
}

// classifySummary is the compact, credential-free shape logged for each
// /v1/messages request.
type classifySummary struct {
	Class      string   `json:"class"`
	Status     int      `json:"status"`
	Model      string   `json:"model"`
	MaxTokens  int      `json:"max_tokens"`
	NumTools   int      `json:"n_tools"`
	Tools      []string `json:"tools,omitempty"`
	NumMsgs    int      `json:"n_msgs"`
	IsSubagent bool     `json:"is_subagent,omitempty"`
	System     string   `json:"system_prefix,omitempty"`
	LastUser   string   `json:"last_user_prefix,omitempty"`
}

// logClassify records the classification and a credential-free shape of one
// /v1/messages request body (direction "classify"). No-op when the debug
// logger is disabled (the gateway only wires it in when it is live).
func (s *Session) logClassify(class requestClass, status int, body []byte) {
	if s.evlog == nil {
		return
	}
	var req messagesRequest
	// Best-effort: an unparseable body still logs its class and status.
	_ = json.Unmarshal(body, &req)
	sum := classifySummary{
		Class:      class.String(),
		Status:     status,
		Model:      req.Model,
		MaxTokens:  req.MaxTokens,
		NumTools:   len(req.Tools),
		Tools:      capStrings(req.toolNames(), 8),
		NumMsgs:    len(req.Messages),
		IsSubagent: isSubagentSystem(req.System),
		System:     runePrefix(blockText(req.System), classifyLogPrefixRunes),
		LastUser:   runePrefix(lastUserText(req.Messages), classifyLogPrefixRunes),
	}
	data, err := json.Marshal(sum)
	if err != nil {
		log.Printf("claudetui: classify log marshal failed: %v", err)
		return
	}
	if err := s.evlog.LogProviderEvent(logging.ProviderEventEntry{
		ThreadID:  s.threadID,
		Direction: "classify",
		Provider:  logProvider,
		Data:      string(data),
	}); err != nil {
		log.Printf("claudetui: classify log failed: %v", err)
	}
}

// decisionLog is the credential-free record of one routing/reconstruction
// decision (direction "decision"). It is heterogeneous by Event — omitempty
// keeps each line to the fields that decision actually set:
//
//   - Event "route":         a classAgent request entered reconstruction.
//     Route is "main" (no agent-id header), "subagent" (resolved to Parent, Via
//     "cache" or "content-match"), or "subagent-unmatched" (no launch matched →
//     forwarded WITHOUT reconstruction, the silent degradation). Init reports
//     whether a main request (re)opened a settled loop (emitted system:init).
//   - Event "bg_completion":  emitBackgroundCompletions saw a <task-notification>.
//     Action is "emitted" (TaskID/ToolUseID completion synthesized), "deduped"
//     (already seen — it recurs in history), "skipped-statusless" (a stall
//     progress ping, still running), or "unroutable" (no <task-id>).
//   - Event "turn_close":     a main request closed the turn with a result
//     (Stop = the wire stop_reason); the loop settles.
//   - Event "subagent_end":   a subagent request finished nesting under Parent.
type decisionLog struct {
	Event     string `json:"event"`
	Route     string `json:"route,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Via       string `json:"via,omitempty"`
	NumMsgs   int    `json:"n_msgs,omitempty"`
	Init      bool   `json:"init,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Action    string `json:"action,omitempty"`
	Stop      string `json:"stop,omitempty"`
}

// logDecision records one routing/reconstruction decision (direction
// "decision"). Wired onto the reconstructor (reconstructor.debug) only when the
// event logger is live, so a production session never builds a decisionLog.
func (s *Session) logDecision(d decisionLog) {
	if s.evlog == nil {
		return
	}
	data, err := json.Marshal(d)
	if err != nil {
		log.Printf("claudetui: decision log marshal failed: %v", err)
		return
	}
	if err := s.evlog.LogProviderEvent(logging.ProviderEventEntry{
		ThreadID:  s.threadID,
		Direction: "decision",
		Provider:  logProvider,
		Data:      string(data),
	}); err != nil {
		log.Printf("claudetui: decision log failed: %v", err)
	}
}

// runePrefix returns up to n runes of s, appending an ellipsis when truncated.
// Rune-based so a multibyte character is never split mid-encoding.
func runePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// capStrings caps a slice to at most n entries (for the tool-name list).
func capStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}
