package claudetui

import (
	"bytes"
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider/claude"
)

// turndriver.go owns the cross-request turn state that the pure assembler in
// reconstruct.go does not: emitting system:init on every main-loop (re)start,
// emitting the user{isReplay} echo that confirms each AO Send, accumulating
// usage across the several /v1/messages requests that make up one logical turn,
// and closing the turn with a synthesized result envelope.
//
// Two independent signals drive the turn-boundary envelopes, mirroring headless:
//   - system:init fires whenever a main request (re)opens a SETTLED loop
//     (turnSettled). Headless re-emits init on every main-loop restart — a new
//     user turn AND a backgrounded-task resume after the interim end_turn (see
//     the local_agent_outlives fixture, init #2). triage's handleInit then opens
//     a turn (when a pending send waits) or re-arms the settled round.
//   - the replay echo fires whenever an AO Send awaits confirmation (userEchoes),
//     independent of turnSettled, so a mid-turn queued steer confirms without
//     opening a new turn and a backgrounded resume (no Send) emits none.
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

	// debug, when non-nil, records a credential-free decisionLog at each
	// routing/reconstruction branch the "in" envelope feed cannot show on its
	// own: the route a request took, the Agent card a subagent resolved to (and
	// via cache vs content match), and what emitBackgroundCompletions did with an
	// injected <task-notification> — including the silent unmatched-subagent and
	// skipped-completion cases. nil in production (the Session wires it only when
	// the event logger is live) → zero cost. See debuglog.go.
	debug func(decisionLog)

	// session identity, seeded from the SessionStart hook (may be empty until
	// it arrives — the first agent request still emits init with model/tools).
	sessionID string
	cwd       string
	version   string

	// turn-usage accumulator, summed across a turn's requests for cost
	// accounting and reset when a done-stop-reason closes the turn.
	turn wireUsage
	// turnSettled reports whether the main agent loop is between turns: true
	// initially and after end()/interruptTurn() emits a result, false once a main
	// request (re)opens the loop. A main request that finds it true emits
	// system:init — the signal triage's handleInit keys on to open a turn (pending
	// send present) or re-arm a settled round (none). A continuation finds it false
	// and emits no init, so the turn stays open across its several requests.
	// Guarded by the session's recMu (begin/end/interrupt).
	turnSettled bool
	// interrupted is set by interruptTurn when the user aborts a turn the TUI
	// has no control-ack channel for. It neutralizes the LATE end() of the
	// request that was in flight when the abort landed (the Esc cancels the
	// upstream request, so its stream ends after interruptTurn already closed
	// and reset the turn): a still-accumulating end() would otherwise bill the
	// aborted request's partial usage into the next turn. Cleared at the next
	// beginAgentRequest. Guarded by the session's recMu (begin/end/interrupt).
	interrupted bool

	// userEchoes is the FIFO of AO Sends awaiting their wire confirmation. Send
	// pushes one (content + uuid) per call; the next main request pops it and
	// emits the user{isReplay:true} echo that consumes triage's pending-send FIFO
	// and stamps provider_item_id onto the optimistic user row. Decoupled from
	// turnSettled (see file header). Guarded by the session's recMu.
	userEchoes []pendingUserEcho

	// seenTaskTerminals dedups reconstructed background-task completions by
	// task_id. A terminal <task-notification> stays in the conversation history,
	// so it recurs in every later request body; without this we'd re-stash and
	// re-drain the same completion on each request. One entry per backgrounded
	// task that completed (bounded by a session's background-task count). Guarded
	// by the session's recMu (begin/end/interrupt). See emitBackgroundCompletions.
	seenTaskTerminals map[string]struct{}

	// Subagent correlation (Claude `Agent`/`Task` tool). A subagent's
	// /v1/messages requests carry no parent linkage in their body, so we rebuild
	// it from two facts established before the subagent runs:
	//   - launches: every Agent/Task tool_use seen on the MAIN wire, recorded
	//     (prompt → tool_use_id) at that request's end(). The launch happens-before
	//     the subagent request (Claude needs the full main response, then spawns,
	//     then the subagent makes its first call — separated by a network round
	//     trip), so the registry is always populated in time.
	//   - byAgentID: X-Claude-Code-Agent-Id → resolved parent tool_use_id, so a
	//     subagent's later requests skip the content match.
	// resolveSubagentParent content-matches a subagent's first user message
	// against an unclaimed launch's prompt (verified a verbatim substring on
	// 2.1.170) and claims it. All registry access is under the session's recMu
	// (registered in end(), read in beginSubagentTurn). See
	// docs/architecture/claude-tui-provider.md §Subagents.
	launches  []agentLaunch
	byAgentID map[string]string
}

// agentLaunch is one Agent/Task tool_use seen on the main wire, awaiting the
// subagent it spawned. claimed flips once a subagent request matches it, so two
// parallel launches each bind to a distinct subagent.
type agentLaunch struct {
	toolUseID string
	prompt    string
	claimed   bool
}

// pendingUserEcho is one AO Send awaiting its wire confirmation echo. uuid is
// the app-minted UserMessageUUID (direct sends) or a Send-minted id (queued
// sends); content is the user's text.
type pendingUserEcho struct {
	content string
	uuid    string
}

const (
	// maxLaunches caps the unclaimed-launch registry; claimed entries are
	// compacted out first, so this is only hit by a flood of never-matched
	// launches (pathological). maxByAgentID caps the resolved cache, reset
	// wholesale on overflow like the parser's correlation maps.
	maxLaunches  = 256
	maxByAgentID = 1024
	// maxUserEchoes caps the pending-echo FIFO. Only hit if Sends never produce a
	// matching wire request (pathological); the oldest is dropped so a stuck head
	// can't strand every later send behind it.
	maxUserEchoes = 256
)

func newReconstructor(emit func(json.RawMessage)) *reconstructor {
	// turnSettled starts true so the first main request emits init.
	return &reconstructor{emit: emit, turnSettled: true, seenTaskTerminals: map[string]struct{}{}}
}

// setSessionInfo records identity from the SessionStart hook so each init (and
// resume bookkeeping) can carry it. Safe to call before or after the first agent
// request; an init is emitted on every main-loop (re)start (see beginAgentRequest).
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

// agentRequest reconstructs one /v1/messages agent response. parent is empty
// for a main-loop request and the launching Agent tool_call's tool_use_id for a
// subagent request; it threads onto every envelope so the parser nests the
// subagent's work under that Agent card.
type agentRequest struct {
	r      *reconstructor
	asm    *messageAssembler
	parent string
}

// beginAgentRequest starts reconstruction for one classAgent request, emitting
// the two turn-boundary signals headless does (see file header):
//
//   - system:init when the loop is settled (turnSettled) — a new user turn or a
//     backgrounded-task resume. It carries the request's model/tools plus any
//     hook session info, and drives triage's handleInit (open a turn when a
//     pending send waits, else re-arm the settled round).
//   - the user{isReplay} echo when an AO Send awaits confirmation (userEchoes),
//     consuming triage's pending-send FIFO and stamping provider_item_id.
//
// A tool-continuation request finds turnSettled false and no pending echo, so it
// emits neither and the turn stays open across its several requests.
func (r *reconstructor) beginAgentRequest(req *messagesRequest) *agentRequest {
	// A fresh request starts un-interrupted; the flag only neutralizes the late
	// end() of the one request the previous interrupt aborted.
	r.interrupted = false
	initEmitted := r.turnSettled
	if r.turnSettled {
		r.emit(initLine(r.sessionID, req.Model, r.cwd, r.version, req.toolNames()))
		r.turnSettled = false
	}
	if r.debug != nil {
		r.debug(decisionLog{Event: "route", Route: "main", NumMsgs: len(req.Messages), Init: initEmitted})
	}
	if echo, ok := r.takeUserEcho(); ok {
		r.emit(replayUserLine(echo.uuid, echo.content))
	}
	// After init re-opened the loop (or a continuation kept it open), fold in any
	// backgrounded command/agent that finished since the last request — the only
	// place its completion crosses the wire is this body.
	r.emitBackgroundCompletions(req.Messages)
	return &agentRequest{r: r, asm: newMessageAssembler()}
}

// pushUserEcho records an AO Send so the next main request can confirm it with a
// replay echo. uuid is a stable, non-empty handle (the caller mints one for
// queued sends, whose flush path supplies none). Caller holds the session recMu.
func (r *reconstructor) pushUserEcho(content, uuid string) {
	if len(r.userEchoes) >= maxUserEchoes {
		r.userEchoes = r.userEchoes[1:]
	}
	r.userEchoes = append(r.userEchoes, pendingUserEcho{content: content, uuid: uuid})
}

// takeUserEcho pops the oldest pending echo (FIFO), matching the order triage
// registered its pending sends. Caller holds the session recMu.
func (r *reconstructor) takeUserEcho() (pendingUserEcho, bool) {
	if len(r.userEchoes) == 0 {
		return pendingUserEcho{}, false
	}
	echo := r.userEchoes[0]
	r.userEchoes = r.userEchoes[1:]
	return echo, true
}

// taskNotificationProbe is the cheap byte-scan needle that gates the full parse in
// emitBackgroundCompletions: only a request message whose raw bytes contain it can
// carry a <task-notification>, so the common case (no background completion in this
// request) skips the per-message unmarshal entirely.
var taskNotificationProbe = []byte("<task-notification")

// emitBackgroundCompletions reconstructs the background-task completion events
// headless emits, from the only signal claude-tui gets for them: a
// <task-notification> the CLI injects into a /v1/messages request body when a
// backgrounded command or agent finishes. The stream-json system/task_updated +
// system/task_notification headless emits are CLI-internal and never cross the
// /v1/messages wire; a foreground tool returns its result inline and injects no
// notification, so this fires only for genuinely backgrounded work — an inline run
// never produces a separate completion row.
//
// For each terminal notification not seen before, it emits the same pair headless
// does, in order: task_updated (parser EventBackgroundTaskTerminal → triage stashes
// the host-side exit) then task_notification (EventBackgroundTaskNotification →
// drains that stash → writes the tool_completion sibling at the current write head).
// Both are system/user-string envelopes that pass through feedReorder untouched, and
// the feed channel is FIFO, so the stash-before-drain order holds.
//
// A statusless <task-notification> is a stall progress ping, not a terminal: the
// task is still running, so it is skipped (matching headless, where print.ts treats
// the absence of <status> as non-terminal). Dedup is by task_id — the notification
// stays in history and recurs in every later request body. Covers backgrounded Bash
// commands and backgrounded agents alike; both share the tag shape.
//
// Caller holds the session recMu (beginAgentRequest).
func (r *reconstructor) emitBackgroundCompletions(messages []json.RawMessage) {
	eachTaskNotification(messages, func(fields claude.TaskNotificationFields, ok bool) bool {
		if !ok {
			r.logBgDecision(decisionLog{Event: "bg_completion", Action: "unroutable"})
			return true // missing or malformed task-id — unroutable
		}
		if claude.NormalizeTaskTerminalStatus(fields.Status) == "" {
			r.logBgDecision(decisionLog{Event: "bg_completion", TaskID: fields.TaskID, ToolUseID: fields.ToolUseID, Status: fields.Status, Action: "skipped-statusless"})
			return true // statusless stall ping — the task is still running
		}
		if _, seen := r.seenTaskTerminals[fields.TaskID]; seen {
			r.logBgDecision(decisionLog{Event: "bg_completion", TaskID: fields.TaskID, ToolUseID: fields.ToolUseID, Status: fields.Status, Action: "deduped"})
			return true
		}
		r.seenTaskTerminals[fields.TaskID] = struct{}{}
		r.emit(taskUpdatedLine(fields.TaskID, fields.ToolUseID, fields.Status))
		r.emit(taskNotificationLine(fields.TaskID, fields.ToolUseID, fields.Status, fields.OutputFile, fields.Summary))
		r.logBgDecision(decisionLog{Event: "bg_completion", TaskID: fields.TaskID, ToolUseID: fields.ToolUseID, Status: fields.Status, Action: "emitted"})
		return true // process every notification in the body
	})
}

// eachTaskNotification invokes fn for every user message carrying a
// <task-notification>, passing the extracted fields and whether a usable task-id
// was found (ok=false marks a missing or malformed task-id — unroutable). fn
// returns false to stop early. It is the single definition of "what counts as a
// task-notification
// message," shared by the routing discriminator (requestReportsAgentCompletion)
// and the emitter (emitBackgroundCompletions) so a routing decision can never
// disagree with what actually gets emitted. The byte-probe skips the common
// no-notification message without a parse.
func eachTaskNotification(messages []json.RawMessage, fn func(fields claude.TaskNotificationFields, ok bool) bool) {
	for _, raw := range messages {
		if !bytes.Contains(raw, taskNotificationProbe) {
			continue
		}
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Role != "user" {
			continue
		}
		fields, ok := claude.ExtractTaskNotificationFields(blockText(m.Content))
		if !fn(fields, ok) {
			return
		}
	}
}

// requestReportsAgentCompletion reports whether a /v1/messages body carries a
// <task-notification> whose task-id equals agentID — i.e. this header-bearing
// request is the MAIN loop waking to observe that backgrounded subagent's
// completion, NOT a genuine subagent turn.
//
// It is the gateway's main-vs-subagent discriminator for a request carrying the
// X-Claude-Code-Agent-Id header. The header alone is ambiguous: Claude attaches
// the just-completed subagent's agent-id (and cc_is_subagent=true) to the main
// loop's resume that drains its <task-notification>, so routing on the header
// misroutes that observation as a subagent continuation — dropping the completion
// (emitBackgroundCompletions only runs off the main path) and nesting the main
// response under the Agent card (spike/claude-mitm 2026-06-13: the second of two
// backgrounded subagents never surfaced).
//
// The deterministic tell is structural, not prose: a backgrounded subagent's
// task_id IS its agent_id (2.1.170: agent afb0472fd557c6a6e ⇒ task
// afb0472fd557c6a6e), and a genuine subagent turn never reports its OWN
// completion (a subagent can't background itself). So a self-referential
// task-notification — task-id == this request's agent-id — uniquely marks the
// main loop's observation. A subagent that backgrounds a CHILD and polls carries
// the child's task-id (≠ its own agent-id), so it correctly stays on the subagent
// path. Status is not checked: a still-running progress ping for this agent is
// equally the main loop, not the subagent's own turn.
//
// Shares eachTaskNotification with emitBackgroundCompletions so the routing
// decision can't drift from what that method later emits.
func requestReportsAgentCompletion(messages []json.RawMessage, agentID string) bool {
	if agentID == "" {
		return false
	}
	found := false
	eachTaskNotification(messages, func(fields claude.TaskNotificationFields, ok bool) bool {
		if ok && fields.TaskID == agentID {
			found = true
			return false // self-referential notification is conclusive — stop scanning
		}
		return true
	})
	return found
}

// logBgDecision records one emitBackgroundCompletions branch when the debug hook
// is wired (no-op otherwise). Keeps the per-branch call sites in
// emitBackgroundCompletions to one line each.
func (r *reconstructor) logBgDecision(d decisionLog) {
	if r.debug != nil {
		r.debug(d)
	}
}

// beginSubagentRequest starts reconstruction for one subagent /v1/messages
// request, with parent already resolved to its launching Agent tool_call. Unlike
// the main path it emits no system:init and arms no interrupt state — a subagent
// is nested machinery, not a top-level turn.
func (r *reconstructor) beginSubagentRequest(parent string) *agentRequest {
	return &agentRequest{r: r, asm: newMessageAssembler(), parent: parent}
}

// onSSE streams one raw SSE event: passthrough for live deltas, plus folding
// into the assembler for the end-of-response assistant envelope. Each delta
// carries ar.parent so a subagent's stream nests under its Agent card.
func (ar *agentRequest) onSSE(sse json.RawMessage) {
	ar.r.emit(streamEventLine(sse, ar.parent))
	ar.asm.consume(sse)
}

// end closes the response: emits the assembled assistant envelope (the sole
// source of EventToolStart), accumulates usage, and — when the model is done —
// closes the turn with a synthesized result envelope.
func (ar *agentRequest) end() {
	// Emit the (possibly partial) assistant envelope even on an interrupted
	// request, so triage sees and force-closes any orphaned tool_use start —
	// matching the headless interrupt path.
	ar.r.emit(ar.asm.assistantLine(ar.parent))

	if ar.parent != "" {
		// Subagent request. Its assistant + tool_use starts nest under the Agent
		// card via parent_tool_use_id; its inner tool completions arrive on hooks
		// keyed by tool_use_id and the parser re-derives their parent from the
		// start we just emitted — so nothing else is needed here. Deliberately NO
		// result (it is not a top-level turn; emitting one would force-close the
		// real turn) and NO usage folded into the main turn (subagent tokens are
		// the model's private accounting). A subagent cannot itself spawn (Claude
		// forbids recursive Agent), so it registers no launches.
		if ar.r.debug != nil {
			ar.r.debug(decisionLog{Event: "subagent_end", Route: "subagent", Parent: ar.parent})
		}
		return
	}

	// Main-loop request. Capture any Agent/Task launches so a following subagent
	// request can resolve its parent — done before the interrupt short-circuit
	// since the launch stands even if this turn is then aborted.
	ar.r.registerAgentLaunches(ar.asm)
	if ar.r.interrupted {
		// interruptTurn already closed and reset the turn; billing this aborted
		// request's partial usage would bleed into the next turn.
		return
	}
	ar.r.accumulate(ar.asm.usage)
	if claude.IsSoftRoundCloseStopReason(ar.asm.stop) {
		ar.r.emit(resultLine(ar.asm.stop, ar.r.turnUsageJSON(), false))
		ar.r.turn = wireUsage{}
		// Loop settled: the next main request (a new turn or a backgrounded
		// resume) re-emits init. The reconstructor also settles at the interim
		// end_turn of a backgrounded turn; the resume's init re-arms via
		// handleInit, matching headless (which re-inits there too).
		ar.r.turnSettled = true
		if ar.r.debug != nil {
			ar.r.debug(decisionLog{Event: "turn_close", Route: "main", Stop: ar.asm.stop})
		}
	}
}

// interruptTurn closes the current turn as a user abort, emitting the result
// shape parse_result.detectInterrupted classifies as an interrupt. Called by
// the session when it sees the wire/transcript interrupt marker, since the TUI
// path has no control-ack channel.
func (r *reconstructor) interruptTurn() {
	r.emit(resultLine("", nil, true))
	r.turn = wireUsage{}
	// Loop settled by the abort: a queued send flushed on the interrupt boundary
	// opens a fresh turn (init + echo) rather than resuming the aborted one.
	r.turnSettled = true
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

// --- subagent correlation -------------------------------------------------

// registerAgentLaunches records the Agent/Task tool_use launches in a finished
// main assistant so a following subagent request can resolve its parent. Called
// from the main path's end() under the session's recMu.
func (r *reconstructor) registerAgentLaunches(asm *messageAssembler) {
	launches := asm.agentLaunches()
	if len(launches) == 0 {
		return
	}
	if len(r.launches)+len(launches) > maxLaunches {
		r.compactLaunches()
	}
	r.launches = append(r.launches, launches...)
}

// compactLaunches drops already-claimed launches, then bounds the slice to its
// most recent entries if a flood of never-matched launches remains.
func (r *reconstructor) compactLaunches() {
	kept := make([]agentLaunch, 0, len(r.launches))
	for _, l := range r.launches {
		if !l.claimed {
			kept = append(kept, l)
		}
	}
	if len(kept) > maxLaunches {
		kept = kept[len(kept)-maxLaunches:]
	}
	r.launches = kept
}

// resolveSubagentParent maps a subagent request to the Agent tool_call that
// launched it. A cached agent id resolves directly; otherwise the subagent's
// first user message is matched against an unclaimed launch's prompt (a verbatim
// substring on 2.1.170) and that launch is claimed so a parallel sibling can't
// also take it. Returns "" when no launch matches — the caller then forwards the
// request without reconstructing it (the Agent card still completes via its
// PostToolUse hook), degrading to "subagent internals not shown" rather than
// mis-attributing them to the main thread. Caller holds recMu.
func (r *reconstructor) resolveSubagentParent(agentID, firstUserText string) string {
	if agentID != "" {
		if parent, ok := r.byAgentID[agentID]; ok {
			r.logBgDecision(decisionLog{Event: "route", Route: "subagent", AgentID: agentID, Parent: parent, Via: "cache"})
			return parent
		}
	}
	for i := range r.launches {
		l := &r.launches[i]
		if l.claimed || l.prompt == "" || !strings.Contains(firstUserText, l.prompt) {
			continue
		}
		l.claimed = true
		r.cacheAgent(agentID, l.toolUseID)
		r.logBgDecision(decisionLog{Event: "route", Route: "subagent", AgentID: agentID, Parent: l.toolUseID, Via: "content-match"})
		return l.toolUseID
	}
	r.logBgDecision(decisionLog{Event: "route", Route: "subagent-unmatched", AgentID: agentID, Via: "none"})
	return ""
}

// cacheAgent records a resolved X-Claude-Code-Agent-Id → parent mapping so the
// subagent's later requests skip the content match. Bounded like the parser's
// correlation maps: reset wholesale on overflow.
func (r *reconstructor) cacheAgent(agentID, parent string) {
	if agentID == "" || parent == "" {
		return
	}
	if r.byAgentID == nil {
		r.byAgentID = make(map[string]string)
	}
	if len(r.byAgentID) >= maxByAgentID {
		r.byAgentID = make(map[string]string)
	}
	r.byAgentID[agentID] = parent
}
