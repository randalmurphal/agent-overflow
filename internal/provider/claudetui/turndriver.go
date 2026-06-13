package claudetui

import (
	"encoding/json"

	"agent-overflow/internal/provider/claude"
)

// turndriver.go owns the cross-request turn state that the pure assembler in
// reconstruct.go does not: emitting system:init once, accumulating usage across
// the several /v1/messages requests that make up one logical turn, and closing
// the turn with a synthesized result envelope.
//
// One reconstructor per session. The gateway calls beginAgentRequest for each
// classAgent request, feeds its SSE events, then ends it. emit feeds each
// reconstructed envelope through the session's single claude.Parser.

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// reconstructor holds per-session reconstruction state.
type reconstructor struct {
	emit func(json.RawMessage)

	// session identity, seeded from the SessionStart hook (may be empty until
	// it arrives — the first agent request still emits init with model/tools).
	sessionID string
	cwd       string
	version   string
	sawInit   bool

	// turn-usage accumulator, summed across a turn's requests for cost
	// accounting and reset when a done-stop-reason closes the turn.
	turn wireUsage
	// interrupted is set by interruptTurn when the user aborts a turn the TUI
	// has no control-ack channel for. It neutralizes the LATE end() of the
	// request that was in flight when the abort landed (the Esc cancels the
	// upstream request, so its stream ends after interruptTurn already closed
	// and reset the turn): a still-accumulating end() would otherwise bill the
	// aborted request's partial usage into the next turn. Cleared at the next
	// beginAgentRequest. Guarded by the session's recMu (begin/end/interrupt).
	interrupted bool
}

func newReconstructor(emit func(json.RawMessage)) *reconstructor {
	return &reconstructor{emit: emit}
}

// setSessionInfo records identity from the SessionStart hook so a later init
// (and resume bookkeeping) can carry it. Safe to call before or after the
// first agent request; the init is emitted at most once.
func (r *reconstructor) setSessionInfo(sessionID, cwd, version string) {
	if sessionID != "" {
		r.sessionID = sessionID
	}
	if cwd != "" {
		r.cwd = cwd
	}
	if version != "" {
		r.version = version
	}
}

// agentRequest reconstructs one /v1/messages agent response.
type agentRequest struct {
	r   *reconstructor
	asm *messageAssembler
}

// beginAgentRequest starts reconstruction for one classAgent request, emitting
// the one-time system:init from the first request's model/tools plus any hook
// session info seen so far.
func (r *reconstructor) beginAgentRequest(req *messagesRequest) *agentRequest {
	// A fresh request starts un-interrupted; the flag only neutralizes the late
	// end() of the one request the previous interrupt aborted.
	r.interrupted = false
	if !r.sawInit {
		r.emit(initLine(r.sessionID, req.Model, r.cwd, r.version, req.toolNames()))
		r.sawInit = true
	}
	return &agentRequest{r: r, asm: newMessageAssembler()}
}

// onSSE streams one raw SSE event: passthrough for live deltas, plus folding
// into the assembler for the end-of-response assistant envelope.
func (ar *agentRequest) onSSE(sse json.RawMessage) {
	ar.r.emit(streamEventLine(sse))
	ar.asm.consume(sse)
}

// end closes the response: emits the assembled assistant envelope (the sole
// source of EventToolStart), accumulates usage, and — when the model is done —
// closes the turn with a synthesized result envelope.
func (ar *agentRequest) end() {
	// Emit the (possibly partial) assistant envelope even on an interrupted
	// request, so triage sees and force-closes any orphaned tool_use start —
	// matching the headless interrupt path.
	ar.r.emit(ar.asm.assistantLine())
	if ar.r.interrupted {
		// interruptTurn already closed and reset the turn; billing this aborted
		// request's partial usage would bleed into the next turn.
		return
	}
	ar.r.accumulate(ar.asm.usage)
	if claude.IsSoftRoundCloseStopReason(ar.asm.stop) {
		ar.r.emit(resultLine(ar.asm.stop, ar.r.turnUsageJSON(), false))
		ar.r.turn = wireUsage{}
	}
}

// interruptTurn closes the current turn as a user abort, emitting the result
// shape parse_result.detectInterrupted classifies as an interrupt. Called by
// the session when it sees the wire/transcript interrupt marker, since the TUI
// path has no control-ack channel.
func (r *reconstructor) interruptTurn() {
	r.emit(resultLine("", nil, true))
	r.turn = wireUsage{}
	r.interrupted = true
}

func (r *reconstructor) accumulate(usageRaw json.RawMessage) {
	if len(usageRaw) == 0 {
		return
	}
	var u wireUsage
	if json.Unmarshal(usageRaw, &u) != nil {
		return
	}
	r.turn.InputTokens += u.InputTokens
	r.turn.OutputTokens += u.OutputTokens
	r.turn.CacheReadInputTokens += u.CacheReadInputTokens
	r.turn.CacheCreationInputTokens += u.CacheCreationInputTokens
}

func (r *reconstructor) turnUsageJSON() json.RawMessage {
	if r.turn == (wireUsage{}) {
		return nil
	}
	return mustMarshal(r.turn)
}
