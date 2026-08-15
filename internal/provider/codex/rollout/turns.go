package rollout

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// openTurn is the turn currently accepting events.
//
// Codex's rollout only stamps `turn_id` on some records — `task_started`,
// `task_complete`, `turn_aborted`, `turn_context`, the end events, and
// function calls' passthrough metadata all carry it, while `user_message`,
// `agent_message`, `agent_reasoning` and `token_count` carry none. So turn
// attribution is positional: whatever turn is open when a record is read owns
// it, which is the same rule Codex's own replay uses.
type openTurn struct {
	id            string
	index         int
	startedAt     time.Time
	model         string
	effort        string
	cwd           string
	contextWindow int
	// cumulative is the most recent thread-cumulative usage seen during
	// this turn, or nil when the turn reported none.
	cumulative *tokenUsageWire
	// synthetic marks a turn opened because content arrived with no
	// `task_started` — see ensureTurn.
	synthetic bool
	// startEvent is the emitted EventTurnStart index. Codex has shipped both
	// task_started→turn_context and turn_context→task_started orderings, so a
	// later context record must be able to enrich the already-emitted event.
	startEvent int
}

// applyTurnContext records the per-turn configuration Codex persists around
// each real user turn. Released CLIs have written it on both sides of the
// matching `task_started`, so it both seeds a future turn and enriches an
// already-open one. The latest value also becomes ParseResult.Profile.
func (c *converter) applyTurnContext(env envelope) {
	var p turnContextPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	c.pendingCtx = p
	c.profile.observeTurnContext(p)
	if c.turn != nil && (c.turn.id == "" || c.turn.id == strings.TrimSpace(p.TurnID)) {
		applyContextToTurn(c.turn, p)
		c.refreshTurnStartMeta()
	}
}

func applyContextToTurn(turn *openTurn, ctx turnContextPayload) {
	if ctx.Model != "" {
		turn.model = ctx.Model
	}
	if ctx.Effort != "" {
		turn.effort = ctx.Effort
	}
	if ctx.Cwd != "" {
		turn.cwd = ctx.Cwd
	}
}

// startTurn opens a turn on `task_started`.
func (c *converter) startTurn(env envelope) {
	var p taskStartedPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	c.profile.observeTaskStarted(p)
	turnID := strings.TrimSpace(p.TurnID)
	if c.turn != nil {
		if c.turn.id == turnID {
			// The turn is already open under this id — a duplicate
			// task_started, or content before the boundary opened it
			// through ensureTurn (whose id came from the same
			// turn_context). Adopt in place: closing and re-opening
			// would claim the id twice in the writer.
			c.turn.synthetic = false
			if started := secondsToTime(p.StartedAt); !started.IsZero() {
				c.turn.startedAt = started
			}
			if p.ModelContextWindow > 0 {
				c.turn.contextWindow = p.ModelContextWindow
			}
			applyContextToTurn(c.turn, c.pendingCtx)
			c.refreshTurnStartMeta()
			return
		}
		// A previous turn never reported its completion. Close it
		// synthetically so its rows still settle.
		c.closeTurn(nil, time.Time{})
	}
	c.openTurnWith(turnID, secondsToTime(p.StartedAt), false)
	c.turn.contextWindow = p.ModelContextWindow
	if strings.TrimSpace(c.pendingCtx.TurnID) == "" || strings.TrimSpace(c.pendingCtx.TurnID) == turnID {
		applyContextToTurn(c.turn, c.pendingCtx)
	}
	c.emitTurnStart()
}

// ensureTurn opens a synthetic turn when content arrives outside one.
//
// Every imported row has to belong to a turn — the store keys items by
// turn_index — and a rollout can legitimately carry content before its first
// `task_started` (a fork's inherited prefix, a file whose head was lost). A
// synthetic turn is closed as soon as a real `task_started` arrives.
func (c *converter) ensureTurn() {
	if c.turn != nil {
		return
	}
	pending := strings.TrimSpace(c.pendingCtx.TurnID)
	if _, used := c.usedTurnIDs[pending]; used {
		// pendingCtx still names a turn this Parse already settled —
		// Codex writes trailing records (token_count,
		// thread_rolled_back) after a turn_aborted, and no fresh
		// turn_context precedes them. Mint a synthetic id instead of
		// re-claiming the settled one.
		pending = ""
	}
	c.openTurnWith(pending, c.lastTimestamp, true)
	applyContextToTurn(c.turn, c.pendingCtx)
	c.emitTurnStart()
}

func (c *converter) openTurnWith(turnID string, startedAt time.Time, synthetic bool) {
	c.turnIndex++
	if turnID == "" {
		// Synthetic ids enter turns.turn_id, whose key space is global. The
		// parse-local index alone would make every rollout's first inferred
		// turn collide at `import-turn-1`; include the authoritative session
		// id so independent imports remain independent while a tail parse of
		// this same rollout deterministically re-mints the same identity and
		// can be rejected as a re-open by the writer.
		turnID = fmt.Sprintf("import-turn:%s:%d", c.opts.SessionID, c.turnIndex)
	}
	c.usedTurnIDs[turnID] = struct{}{}
	if startedAt.IsZero() {
		startedAt = c.lastTimestamp
	}
	c.turn = &openTurn{
		id:         turnID,
		index:      c.turnIndex,
		startedAt:  startedAt,
		model:      c.pendingCtx.Model,
		effort:     c.pendingCtx.Effort,
		cwd:        c.pendingCtx.Cwd,
		synthetic:  synthetic,
		startEvent: -1,
	}
}

// turnStartMeta carries the per-turn configuration onto the turn-start event.
// ProviderEvent has no field for it (a live session learns the model from the
// session handshake, not from the turn), so it rides in Meta — see the Meta
// key table in this package's AGENTS.md.
func (c *converter) emitTurnStart() {
	c.turn.startEvent = len(c.events)
	c.emit(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		TurnID:    c.turn.id,
		TurnIndex: c.turn.index,
		Meta:      c.turnStartMeta(),
		Timestamp: c.turn.startedAt,
	})
}

func (c *converter) turnStartMeta() json.RawMessage {
	meta := map[string]any{}
	if c.turn.model != "" {
		meta["model"] = c.turn.model
	}
	if c.turn.effort != "" {
		meta["effort"] = c.turn.effort
	}
	if c.turn.cwd != "" {
		meta["cwd"] = c.turn.cwd
	}
	if c.turn.contextWindow > 0 {
		meta["contextWindow"] = c.turn.contextWindow
	}
	if c.turn.synthetic {
		meta["import_synthetic_turn"] = true
	}
	return metaJSON(meta)
}

func (c *converter) refreshTurnStartMeta() {
	if c.turn == nil || c.turn.startEvent < 0 || c.turn.startEvent >= len(c.events) {
		return
	}
	c.events[c.turn.startEvent].Meta = c.turnStartMeta()
}

// completeTurn settles the open turn on `task_complete`.
func (c *converter) completeTurn(env envelope) {
	var p taskCompletePayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	if c.turn == nil {
		turnID := strings.TrimSpace(p.TurnID)
		if _, used := c.usedTurnIDs[turnID]; used && turnID != "" {
			// A task_complete for a turn this Parse already settled — an
			// abort landed first and closed it. Re-opening would claim
			// the id a second time; the abort's settle stands.
			return
		}
		// A completion with no open turn: open one so the turn row and its
		// usage still land, then settle it immediately.
		c.openTurnWith(turnID, secondsToTime(p.StartedAt), true)
		c.emitTurnStart()
	}
	completedAt := secondsToTime(p.CompletedAt)
	if completedAt.IsZero() {
		completedAt = c.lastTimestamp
	}
	// `last_agent_message` is the assistant text, not an id, and the
	// assistant_text row already carries it — the wire has no final message
	// id on this envelope, exactly as on a live turn/completed.
	c.closeTurn(&provider.WireTurnCompleteMeta{
		StopReason: "end_turn",
		Usage:      nil, // filled by closeTurn from the cumulative delta
	}, completedAt)
}

// abortTurn settles the open turn on `turn_aborted`.
func (c *converter) abortTurn(env envelope) {
	var p turnAbortedPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	if c.turn == nil {
		return
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = "aborted"
	}
	completedAt := secondsToTime(p.CompletedAt)
	if completedAt.IsZero() {
		completedAt = c.lastTimestamp
	}
	c.closeTurn(&provider.WireTurnCompleteMeta{
		StopReason:   "aborted",
		Aborted:      true,
		ErrorMessage: "Turn " + reason,
	}, completedAt)
}

// closeTurn settles the open turn, force-completing anything still running.
//
// complete==nil means the file ended (or a new turn started) without a
// completion record; the turn is settled as truncated, which is the same
// shape the live path uses for a turn that never produced a wire boundary.
func (c *converter) closeTurn(complete provider.TurnCompleteMeta, at time.Time) {
	if c.turn == nil {
		return
	}
	completedAt := c.lastTimestamp
	if !at.IsZero() {
		completedAt = at
	}
	c.closeUnresolvedTools(completedAt)

	if complete == nil {
		complete = &provider.TruncatedTurnCompleteMeta{
			Synthetic:    true,
			ErrorMessage: "",
		}
	}
	if wire, ok := complete.(*provider.WireTurnCompleteMeta); ok {
		usage := c.turnUsage()
		if usage != nil {
			wire.Usage = usage
			model := c.turn.model
			if model == "" {
				model = c.pendingCtx.Model
			}
			wire.ModelUsage = []provider.ModelTokenUsage{{Model: model, TokenUsage: *usage}}
		}
	}
	c.emit(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		TurnID:       c.turn.id,
		TurnIndex:    c.turn.index,
		Timestamp:    completedAt,
		TurnComplete: complete,
	})
	c.turn = nil
}

// applyTokenCount records the thread-cumulative usage snapshot.
//
// `token_count` carries no turn id, so it belongs to the open turn, and a
// turn emits many of them — the last one wins. The wire values are CUMULATIVE
// for the whole thread (Codex has no per-turn usage signal), so the turn's own
// usage is the difference against the previous turn's closing total; that is
// the same derivation `usage_accounting.go` performs live.
func (c *converter) applyTokenCount(env envelope) {
	var p tokenCountPayload
	if json.Unmarshal(env.Payload, &p) != nil {
		c.corrupt++
		return
	}
	c.profile.observeTokenCount(p)
	if p.Info == nil || p.Info.TotalTokenUsage == nil {
		return
	}
	c.ensureTurn()
	snapshot := *p.Info.TotalTokenUsage
	c.turn.cumulative = &snapshot
	if p.Info.ModelContextWindow > 0 && c.turn.contextWindow == 0 {
		c.turn.contextWindow = p.Info.ModelContextWindow
		c.refreshTurnStartMeta()
	}
}

// turnUsage converts the open turn's cumulative snapshot into that turn's own
// delta and advances the accounted baseline.
func (c *converter) turnUsage() *provider.TokenUsage {
	if c.turn == nil || c.turn.cumulative == nil {
		return nil
	}
	current := *c.turn.cumulative
	base := c.accounted
	if current.TotalTokens < base.TotalTokens {
		// The cumulative reset — Codex restarts it after a
		// ContextWindowExceeded. Treat the snapshot as the new baseline
		// rather than emitting a negative delta.
		base = tokenUsageWire{}
	}
	delta := provider.TokenUsage{
		InputTokens:              nonNegative(current.InputTokens-current.CachedInputTokens) - nonNegative(base.InputTokens-base.CachedInputTokens),
		OutputTokens:             current.OutputTokens - base.OutputTokens,
		CacheReadInputTokens:     current.CachedInputTokens - base.CachedInputTokens,
		CacheCreationInputTokens: current.CacheWriteInputTokens - base.CacheWriteInputTokens,
		ReasoningOutputTokens:    current.ReasoningOutputTokens - base.ReasoningOutputTokens,
	}
	delta.InputTokens = nonNegative(delta.InputTokens)
	delta.OutputTokens = nonNegative(delta.OutputTokens)
	delta.CacheReadInputTokens = nonNegative(delta.CacheReadInputTokens)
	delta.CacheCreationInputTokens = nonNegative(delta.CacheCreationInputTokens)
	delta.ReasoningOutputTokens = nonNegative(delta.ReasoningOutputTokens)
	c.accounted = current
	if delta.IsZero() {
		return nil
	}
	return &delta
}

func nonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// closeUnresolvedTools settles every tool call still open when the turn ends.
//
// Two different things end up here, and conflating them would be a lie in one
// direction or the other:
//
//   - A call that already told us how it ended — a `web_search_call`, which
//     has no separate output record at all and carries its own terminal
//     `status`, or a call an end event already described. Those settle
//     normally, on the wire's own status.
//   - A call the file simply never resolved (the turn was interrupted, or the
//     process died mid-tool). Its completion carries `import_unresolved` and
//     no result: this package does NOT invent a status, and the writer decides
//     how such a row renders (the live path's answer is the force-close
//     summary suffix). Leaving the row `running` forever is the only worse
//     option.
func (c *converter) closeUnresolvedTools(at time.Time) {
	if len(c.toolOrder) == 0 {
		return
	}
	order := append([]string(nil), c.toolOrder...)
	unresolved := 0
	for _, callID := range order {
		tool, ok := c.tools[callID]
		if !ok {
			continue
		}
		if tool.selfCompleting || tool.enrich != nil {
			c.finishToolAt(tool, "", tool.wireStatus, false, at)
			continue
		}
		unresolved++
		c.emit(provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			TurnID:    tool.turnID,
			TurnIndex: tool.turnIndex,
			ItemID:    tool.itemID,
			ItemType:  tool.itemType,
			Meta: metaJSON(map[string]any{
				"toolName":          tool.toolName,
				"input":             json.RawMessage(tool.input),
				"import_unresolved": true,
			}),
			ParentToolUseID: tool.parentToolUseID,
			Timestamp:       at,
		})
		c.releaseTool(callID)
	}
	if unresolved > 0 {
		c.warnings = append(c.warnings, importir.Warning{
			Code:    WarnUnresolvedTool,
			Message: fmt.Sprintf("%d tool call(s) in this Codex session ended without a recorded result.", unresolved),
		})
	}
}

// secondsToTime converts Codex's epoch-second turn stamps. Zero means the
// field was absent.
func secondsToTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
