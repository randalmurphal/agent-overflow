package codex

import (
	"sync"
	"time"

	"agent-overflow/internal/provider"
)

// BackgroundClassifier decides whether a command-execution tool-call ran in
// the background — i.e. the model moved on with assistant reasoning while
// the tool was still open — so downstream can render a distinct "launched,
// still running" affordance separate from a normal inline command.
//
// We classify stateful rather than purely from the wire because Codex does
// not set an explicit "this is backgrounded" flag on item/started. Forge's
// equivalent tracks the same state machine but triggers the background
// decision on "any later turn activity" (see
// apps/web/src/session-logic/backgroundSignals.ts,
// markCodexBackgroundCandidatesForTurnAdvance). That is too aggressive:
// when the agent queues several shell commands in a single turn, each
// command's start event flags every other open command as "background",
// even though the commands are simply serial and the model hasn't
// emitted any reasoning between them.
//
// Our trigger is assistant text emission. Text deltas only arrive when
// the model is genuinely interleaving prose with an in-flight tool, which
// is the exact condition that turns a synchronous inline command into a
// concurrent background command from the user's perspective. Sibling
// tool calls (without interleaved text) stay inline.
//
// Thread-safety: callers hand events in from the Codex read-loop goroutine.
// The classifier may also be interrogated by other goroutines during teardown,
// so the single mutex is not optional even though one goroutine drives the
// happy path.
type BackgroundClassifier struct {
	mu            sync.Mutex
	openToolCalls map[string]*openToolCall
}

// openToolCall is the per-item state tracked while a tool is live. The
// startedAt timestamp is not currently used in the classification rule —
// we keep it so a future rule (e.g. "open > N seconds") has the anchor it
// needs without another event-flow change.
type openToolCall struct {
	startedAt        time.Time
	markedBackground bool
}

// NewBackgroundClassifier returns a fresh per-session classifier. Construct
// one per Session — the state must not leak across sessions, and two
// sessions must not share a map.
func NewBackgroundClassifier() *BackgroundClassifier {
	return &BackgroundClassifier{
		openToolCalls: make(map[string]*openToolCall),
	}
}

// Classify inspects the event, mutates background state, and, when the
// event is an EventToolComplete for a tool we saw go concurrent with
// assistant text, rewrites evt.Meta to include {"is_background": true}.
// The caller should pass events in the order the read loop produces them.
//
// We rewrite Meta in place so the emission path downstream doesn't have
// to re-plumb the classifier decision through a separate field. Existing
// meta keys from the protocol layer (e.g. source, item_status, raw item
// payload) are preserved.
func (c *BackgroundClassifier) Classify(evt *provider.ProviderEvent) {
	if evt == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch evt.Kind {
	case provider.EventToolStart:
		// Register on the FIRST observation only. item/updated emits
		// EventToolStart with Replace=true for the same itemID (see
		// protocol.go, case "item/updated"); treating a replace as a
		// fresh start would reset the markedBackground flag and flip a
		// genuinely-backgrounded command back to inline. Keep the
		// existing entry so prior text-triggered marking survives.
		if evt.ItemID == "" {
			return
		}
		if _, exists := c.openToolCalls[evt.ItemID]; exists {
			return
		}
		c.openToolCalls[evt.ItemID] = &openToolCall{
			startedAt:        evt.Timestamp,
			markedBackground: false,
		}

	case provider.EventTextDelta:
		// Only assistant-role text flags tools as background. Reasoning
		// (thinking) is handled separately as EventThinking and is not a
		// user-visible interleave.
		if evt.Role != "assistant" {
			return
		}
		for _, state := range c.openToolCalls {
			state.markedBackground = true
		}

	case provider.EventToolComplete:
		if evt.ItemID == "" {
			return
		}
		state, ok := c.openToolCalls[evt.ItemID]
		if !ok {
			return
		}
		delete(c.openToolCalls, evt.ItemID)
		if state.markedBackground {
			evt.Meta = mergeMetaKeys(evt.Meta, map[string]any{
				"is_background": true,
			})
		}

	case provider.EventTurnComplete:
		// A turn boundary clears stale tool-call state so the next turn
		// starts from a clean slate. In normal flow every open tool
		// reaches EventToolComplete inside the turn that opened it, but
		// we cannot rely on that — interrupted or failed turns may drop
		// the completion, and letting entries leak across turns would
		// let turn N's assistant text wrongly flag turn N+1's tools as
		// background.
		c.openToolCalls = make(map[string]*openToolCall)
	}
}

// Reset drops all per-tool state. Used by Session.Close so a restarted
// session cannot inherit half-tracked tool calls. Callers holding a
// Session do not need to call this directly — Close handles it.
func (c *BackgroundClassifier) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openToolCalls = make(map[string]*openToolCall)
}

// The downstream contract is "Meta.is_background=true may appear on
// EventToolStart OR EventToolComplete — treat them the same way". Claude
// sets is_background at start-time; Codex almost always learns it at
// complete-time. If a future Codex field surfaces a start-time hint, the
// adapter can call mergeMetaKeys(evt.Meta, map[string]any{"is_background":
// true}) directly on the start event — the classifier will then see it
// and skip re-marking at complete time (idempotent merge).
