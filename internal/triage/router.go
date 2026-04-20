// Package triage classifies provider events and routes them to the frontend
// (small/inline) or SQLite (heavy payloads like diffs and command output).
package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ErrUnhandledEventKind is returned by Handle when the switch lands in its
// default branch — i.e. a new EventKind was added to the provider package
// without a matching case here. The event is still emitted to the frontend
// under "provider:event" as a best-effort passthrough; the sentinel lets the
// exhaustiveness test flag the drift.
var ErrUnhandledEventKind = errors.New("triage: unhandled event kind")

// CheckpointCapture is the subset of checkpoint.Store that the router calls
// at turn-start. Kept as an interface so tests can inject a stub.
type CheckpointCapture interface {
	IsGitRepository(ctx context.Context, workspace string) bool
	CaptureBaseline(ctx context.Context, workspace, threadID string, turnIndex int) (string, error)
	DeleteRef(ctx context.Context, workspace, ref string) error
}

// TurnMetrics is the subset of OTel counters the router records. Kept as a
// struct of interfaces so the router never has to nil-check each instrument
// (we fill in noop instances when telemetry is disabled).
type TurnMetrics struct {
	TurnsStarted      metric.Int64Counter
	TurnsCompleted    metric.Int64Counter
	TurnsErrored      metric.Int64Counter
	ItemsPersisted    metric.Int64Counter
	PayloadsPersisted metric.Int64Counter
}

// Router classifies provider events and routes them.
type Router struct {
	store                *store.Store
	emit                 func(eventName string, data any) // wraps app.Event.Emit
	checkpoints          CheckpointCapture                // nil-safe; no-op when nil
	tracer               trace.Tracer
	metrics              TurnMetrics
	mu                   sync.Mutex
	pendingCommandDiffs  map[string]pendingCommandInlineDiff
	pendingApprovals     map[string]pendingApprovalState
	pendingApprovalItems map[string]string
	interruptQueue       map[string][]queuedPersistence
	openTurns            map[string]int
	segmentIndexByScope  map[string]int
	blockIndexByScope    map[string]int
	activeTextBlocks     map[string]bool
	activeThinkingBlocks map[string]bool
	streamingItemCounts  map[string]int
	errorSeqByScope      map[string]int
	// capturedTurns guards against double-capture when a provider emits
	// multiple EventTurnStart events for the same (thread, turn) — which
	// happens when Claude re-sends a system.init after an interrupt.
	capturedTurns map[string]bool // key = threadID|turnIndex
	// turnSpans holds the active span for each in-flight turn so we can
	// close it when the matching EventTurnComplete arrives. Keyed by
	// threadID since the provider treats each thread as its own turn
	// stream.
	turnSpans map[string]trace.Span
	// stoppedThreads remembers thread IDs that CleanupThread has
	// explicitly stopped. While the flag is set, Handle drops events
	// that would persist to the store so late-arriving readLoop lines
	// from the torn-down subprocess do not leave orphan rows on the
	// stopped thread (Bug B5). The flag is cleared when a fresh session
	// re-enters the thread via EventInit.
	stoppedThreads map[string]struct{}
	// unknownSessionStatusLogged throttles the "unknown session-status
	// content" log to one line per distinct value. EventSessionStatus
	// carries provider-specific Content strings ("disconnected",
	// "retrying", "session_state_changed", "error"); the triage router
	// only cares about persistent failures, so unknown values are
	// expected but the first sighting is worth flagging so a new
	// provider subtype doesn't silently disappear.
	unknownSessionStatusLogged map[string]struct{}
	// inflight counts Handle() calls currently in progress. Wait drains
	// this to zero so app shutdown can flush observability + persistence
	// without racing against events mid-persist. Counter is bumped at
	// the top of Handle and decremented via defer so a panic still
	// releases the wait.
	inflight sync.WaitGroup
}

// NewRouter creates a triage router. Telemetry is off by default; wire a
// tracer and metrics via SetTelemetry to enable spans and counters.
func NewRouter(st *store.Store, emit func(eventName string, data any)) *Router {
	noopMeter := metricnoop.NewMeterProvider().Meter("triage/router")
	ts, _ := noopMeter.Int64Counter("turns.started")
	tc, _ := noopMeter.Int64Counter("turns.completed")
	te, _ := noopMeter.Int64Counter("turns.errored")
	ip, _ := noopMeter.Int64Counter("items.persisted")
	pp, _ := noopMeter.Int64Counter("payloads.persisted")
	return &Router{
		store:  st,
		emit:   emit,
		tracer: tracenoop.NewTracerProvider().Tracer("triage/router"),
		metrics: TurnMetrics{
			TurnsStarted:      ts,
			TurnsCompleted:    tc,
			TurnsErrored:      te,
			ItemsPersisted:    ip,
			PayloadsPersisted: pp,
		},
		pendingCommandDiffs:        make(map[string]pendingCommandInlineDiff),
		pendingApprovals:           make(map[string]pendingApprovalState),
		pendingApprovalItems:       make(map[string]string),
		interruptQueue:             make(map[string][]queuedPersistence),
		openTurns:                  make(map[string]int),
		segmentIndexByScope:        make(map[string]int),
		blockIndexByScope:          make(map[string]int),
		activeTextBlocks:           make(map[string]bool),
		activeThinkingBlocks:       make(map[string]bool),
		streamingItemCounts:        make(map[string]int),
		errorSeqByScope:            make(map[string]int),
		capturedTurns:              make(map[string]bool),
		turnSpans:                  make(map[string]trace.Span),
		stoppedThreads:             make(map[string]struct{}),
		unknownSessionStatusLogged: make(map[string]struct{}),
	}
}

// SetCheckpointStore wires an external checkpoint store into the router.
// Must be called before Handle is invoked for the first time. Nil is a
// valid argument — it disables checkpointing without breaking triage.
func (r *Router) SetCheckpointStore(c CheckpointCapture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkpoints = c
}

// SetTelemetry wires tracer + metric instruments. Safe to call with nil
// tracer (falls back to noop). Zero-valued TurnMetrics is accepted only if
// every instrument is non-nil — we don't silently promote nil counters to
// noop here because that would mask wiring mistakes.
func (r *Router) SetTelemetry(tracer trace.Tracer, m TurnMetrics) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("triage/router")
	}
	r.tracer = tracer
	if m.TurnsStarted != nil {
		r.metrics.TurnsStarted = m.TurnsStarted
	}
	if m.TurnsCompleted != nil {
		r.metrics.TurnsCompleted = m.TurnsCompleted
	}
	if m.TurnsErrored != nil {
		r.metrics.TurnsErrored = m.TurnsErrored
	}
	if m.ItemsPersisted != nil {
		r.metrics.ItemsPersisted = m.ItemsPersisted
	}
	if m.PayloadsPersisted != nil {
		r.metrics.PayloadsPersisted = m.PayloadsPersisted
	}
}

// Handle processes a provider event: persists heavy payloads, forwards inline events.
func (r *Router) Handle(evt provider.ProviderEvent) error {
	r.inflight.Add(1)
	defer r.inflight.Done()

	// EventInit means a fresh session is (re)starting for this thread;
	// clear any lingering stopped marker so subsequent events persist
	// under the new session.
	if evt.Kind == provider.EventInit {
		r.markThreadActive(evt.ThreadID)
	} else if r.isThreadStopped(evt.ThreadID) {
		// Drop silently. The readLoop could still be draining in-flight
		// lines after StopSession returned; persisting them under the
		// stopped thread would pollute the timeline.
		log.Printf("triage: dropping %s event for stopped thread %s", evt.Kind, evt.ThreadID)
		return nil
	}
	switch evt.Kind {
	case provider.EventTextDelta:
		return r.handleTextDelta(evt)
	case provider.EventToolStart:
		return r.handleToolStart(evt)
	case provider.EventToolComplete:
		return r.handleToolComplete(evt)
	case provider.EventTurnStart:
		return r.handleTurnStart(evt)
	case provider.EventApprovalRequest:
		return r.handleApprovalRequest(evt)
	case provider.EventApprovalResolved:
		return r.handleApprovalResolved(evt)
	case provider.EventCompactBoundary:
		return r.handleCompaction(evt)
	case provider.EventContentBlockStart:
		return r.handleContentBlockStart(evt)
	case provider.EventContentBlockStop:
		return r.handleContentBlockStop(evt)
	case provider.EventSessionStatus:
		return r.handleSessionStatus(evt)
	case provider.EventRateLimits:
		return r.handleRateLimits(evt)
	case provider.EventError:
		return r.handleError(evt)
	case provider.EventTokenUsage:
		return r.handleTokenUsage(evt)
	case provider.EventInit:
		return r.handleInit(evt)
	case provider.EventModelRerouted:
		return r.handleThreadModelUpdate(evt)
	case provider.EventThreadRenamed:
		return r.handleThreadRename(evt)
	case provider.EventTurnComplete:
		return r.handleTurnComplete(evt)
	case provider.EventDiff:
		return r.handleDiff(evt)
	case provider.EventCommandOutput:
		return r.handleCommandOutput(evt)
	case provider.EventThinking:
		return r.handleThinking(evt)
	case provider.EventProposedPlan:
		return r.handleProposedPlan(evt)
	default:
		r.emit("provider:event", evt)
		return fmt.Errorf("%w: %s", ErrUnhandledEventKind, evt.Kind)
	}
}

func (r *Router) handleToolStart(evt provider.ProviderEvent) error {
	if err := r.settleStreamingScope(evt.ThreadID, evt.ParentToolUseID); err != nil {
		log.Printf("triage: settle streaming scope before tool start: %v", err)
	}
	// Lifecycle row first so the file-change / command-mutation helpers
	// below find an existing item to attach their rich payload onto via
	// UpdateItemPayload — otherwise they'd race to AppendItem with the
	// same evt.ItemID and trip the UNIQUE id constraint.
	if err := r.persistToolCallLaunch(evt); err != nil {
		return err
	}
	if err := r.persistFileChangeToolResult(evt); err != nil {
		return err
	}
	if err := r.capturePendingCommandInlineDiff(evt); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func (r *Router) handleToolComplete(evt provider.ProviderEvent) error {
	// Same ordering rationale as handleToolStart: keep the lifecycle row
	// authoritative on id ownership, let the rich-payload helpers update
	// it in place, then flip status last so the final summary reflects
	// any payload-derived label (e.g. file-change preview) rather than
	// the generic "Bash: ls" we wrote at start.
	if err := r.persistFileChangeToolResult(evt); err != nil {
		return err
	}
	if err := r.persistCommandInlineDiffToolResult(evt); err != nil {
		return err
	}
	if err := r.persistToolCallCompletion(evt); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func (r *Router) handleInit(evt provider.ProviderEvent) error {
	// provider:event fanout is retained here for the test harness that
	// uses capturedEventBus.nextProviderEventOfKind as a completion
	// signal — the live frontend does NOT listen on this channel (it
	// reacts to provider:item_upsert, provider:approval, and
	// provider:status instead). When the test harness is migrated, this
	// can drop.
	r.emit("provider:event", evt)
	if evt.Meta == nil {
		return nil
	}

	var info provider.SessionInfo
	if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
		if err := r.store.UpdateSessionRef(evt.ThreadID, info.SessionID); err != nil {
			log.Printf("triage: update session ref: %v", err)
		}
	}
	return nil
}

func (r *Router) handleThreadModelUpdate(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateModel(evt.ThreadID, evt.Content); err != nil {
		r.emit("provider:event_error", map[string]any{
			"threadId": evt.ThreadID,
			"kind":     string(evt.Kind),
			"error":    err.Error(),
		})
		return fmt.Errorf("update thread model: %w", err)
	}
	r.emitThreadUpdated(evt.ThreadID)
	return nil
}

func (r *Router) handleThreadRename(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateTitle(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread title: %w", err)
	}
	r.emitThreadUpdated(evt.ThreadID)
	return nil
}

func (r *Router) handleTokenUsage(evt provider.ProviderEvent) error {
	usage, ok := parseTokenUsage(evt.Meta)
	if ok {
		model, err := r.lookupThreadModel(evt.ThreadID)
		if err != nil {
			log.Printf("triage: lookup thread model: %v", err)
		} else if model != "" {
			usage.TotalCostUSD = provider.CalculateCost(model, usage)
			if meta, merr := json.Marshal(usage); merr == nil {
				evt.Meta = meta
			} else {
				log.Printf("triage: marshal token usage: %v", merr)
			}
		}
	}

	window, ok := decodeContextWindow(evt.Meta)
	if !ok {
		return nil
	}
	if err := r.store.UpdateLastTokenUsage(evt.ThreadID, encodeContextWindow(window)); err != nil {
		return fmt.Errorf("token usage persist: %w", err)
	}
	r.emit("provider:usage", provider.UsageEvent{
		Action:         "usage",
		ThreadID:       evt.ThreadID,
		UsedTokens:     window.UsedTokens,
		MaxTokens:      window.MaxTokens,
		ContextPercent: window.UsedPercentage,
	})
	return nil
}

// handleRateLimits folds EventRateLimits onto the provider:usage channel so
// the context-meter popover can surface rate-limit state alongside token
// usage (chat-rewrite spec, Channels section). The snapshot lives in the
// event Meta; a missing / malformed snapshot is tolerated (the frontend
// just renders nothing for the rate-limits row).
func (r *Router) handleRateLimits(evt provider.ProviderEvent) error {
	usage := provider.UsageEvent{
		Action:   "rate_limits",
		ThreadID: evt.ThreadID,
	}
	if len(evt.Meta) > 0 {
		var snap provider.RateLimitsSnapshot
		if err := json.Unmarshal(evt.Meta, &snap); err != nil {
			log.Printf("triage: unmarshal rate limits: %v", err)
		} else {
			usage.RateLimits = &snap
		}
	}
	r.emit("provider:usage", usage)
	return nil
}

// handleSessionStatus routes EventSessionStatus onto the provider:status
// banner channel — but only for persistent kinds the spec calls out
// (binary_missing, unauthenticated, version_incompatible,
// rate_limited_retrying, ok). Transient values ("disconnected",
// "session_state_changed") drop silently; anything we don't recognize is
// logged once per distinct content string so a new provider subtype
// surfaces without polluting steady-state logs.
func (r *Router) handleSessionStatus(evt provider.ProviderEvent) error {
	kind, known, persistent := classifySessionStatusContent(evt.Content)
	if !known {
		r.logUnknownSessionStatusOnce(evt.Content)
		return nil
	}
	if !persistent {
		return nil
	}

	providerName, err := r.lookupThreadProvider(evt.ThreadID)
	if err != nil {
		// Non-fatal — the banner just won't be provider-scoped. Log so
		// a missing thread row is still visible.
		log.Printf("triage: lookup thread provider for session-status: %v", err)
	}

	r.emit("provider:status", provider.ProviderStatusEvent{
		Kind:     kind,
		Message:  strings.TrimSpace(evt.Content),
		Provider: providerName,
		ThreadID: evt.ThreadID,
	})
	return nil
}

// classifySessionStatusContent maps a provider-specific EventSessionStatus
// Content string to a ProviderStatusEventKind. The return shape is a
// three-tuple: the kind value (meaningful only when both known and
// persistent are true), a known flag (true when the string is recognized
// by either the persistent or transient set), and a persistent flag (true
// when the kind should surface on provider:status).
func classifySessionStatusContent(content string) (provider.ProviderStatusEventKind, bool, bool) {
	switch strings.TrimSpace(content) {
	case "retrying":
		// Claude's api_retry lands here. The spec vocabulary is
		// rate_limited_retrying; we accept any retry as persistent
		// because the banner copy is the same regardless of the
		// upstream cause.
		return provider.ProviderStatusRateLimitedRetrying, true, true
	case "ok", "ready":
		// Either vocabulary clears the banner. "ready" is what the
		// legacy detect flow uses; accept it so a migration in Agent 2's
		// adapter rewrite doesn't have to touch this file to flip a
		// pass-through kind.
		return provider.ProviderStatusOK, true, true
	case "disconnected", "session_state_changed", "error":
		// Transient — the working-indicator handles connection churn
		// and EventError handles terminal failures. Known but not
		// persistent.
		return "", true, false
	default:
		return "", false, false
	}
}

func (r *Router) lookupThreadProvider(threadID string) (string, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	return thread.Provider, nil
}

// unknownSessionStatusCap bounds the throttle set so a long-running
// process that sees a stream of novel session-status strings — a
// provider regression, a wire-format drift, or a fuzzed input — cannot
// grow the map without bound. When the cap is hit the oldest entries
// are dropped wholesale (map reset) which re-admits one extra log line
// per distinct value after a cap rollover. That is acceptable because
// the cap is orders of magnitude higher than the known-good enumeration
// (~6 persistent kinds today).
const unknownSessionStatusCap = 256

func (r *Router) logUnknownSessionStatusOnce(content string) {
	key := strings.TrimSpace(content)
	r.mu.Lock()
	if _, seen := r.unknownSessionStatusLogged[key]; seen {
		r.mu.Unlock()
		return
	}
	if len(r.unknownSessionStatusLogged) >= unknownSessionStatusCap {
		r.unknownSessionStatusLogged = make(map[string]struct{})
	}
	r.unknownSessionStatusLogged[key] = struct{}{}
	r.mu.Unlock()
	log.Printf("triage: unknown session-status content %q — dropping", key)
}

func (r *Router) handleError(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		turnIndex, err = r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			turnIndex = 0
		}
	}

	if isFatalProviderError(evt.Meta) {
		if err := r.markTurnItemsErrored(evt.ThreadID, turnIndex, now); err != nil {
			return err
		}
		if err := r.drainInterruptQueue(evt.ThreadID, true); err != nil {
			return err
		}
		r.clearOpenTurn(evt.ThreadID)
		r.closeTurnSpan(evt.ThreadID, errors.New(stringsx.FirstNonEmptyTrimmed(evt.Content, "provider error")))
	}

	scope := strings.TrimSpace(evt.ParentToolUseID)
	item := store.Item{
		ID:        nextErrorID(turnIndex, scope, r.nextErrorSequence(evt.ThreadID, turnIndex, scope)),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "error",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Provider error"),
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

// emitInline forwards the raw provider event on the provider:event
// channel. The live frontend does NOT listen on this channel (see
// handleInit's note); the emission is retained for the Go test harness
// that uses nextProviderEventOfKind as a completion signal. A future
// refactor can drop this once the harness migrates to
// provider:item_upsert-driven synchronization.
func (r *Router) emitInline(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	return nil
}

func (r *Router) emitThreadUpdated(threadID string) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		log.Printf("triage: load updated thread %s: %v", threadID, err)
		return
	}
	r.emit("thread:updated", thread)
}

func (r *Router) persistItem(item store.Item, payload *store.Payload) error {
	// parent_id invariant (spec invariant 7): items.parent_id must point
	// to an existing tool_call row. Drop invalid / cyclic refs here rather
	// than rejecting the whole persistence — a dangling parent would only
	// hide the row from the UI, not corrupt the thread.
	if item.ParentID != "" {
		if dropped, reason := r.shouldDropParentID(item.ThreadID, item.ID, item.ParentID); dropped {
			log.Printf("triage: dropping parent_id %q on item %s: %s", item.ParentID, item.ID, reason)
			item.ParentID = ""
		}
	}

	persisted, err := r.store.UpsertItem(item, payload)
	if err != nil {
		return err
	}
	r.emitItemUpsert(persisted)
	r.metrics.ItemsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", persisted.Kind)))
	if payload != nil {
		r.metrics.PayloadsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", payload.Kind)))
	}
	return nil
}

// shouldDropParentID decides whether an item's parent_id should be
// dropped before persistence. The spec invariant is that parent_id
// ultimately points to a tool_call row, but text/thinking deltas from
// a subagent can arrive before the parent Task tool_call is persisted
// — so a missing parent is NOT grounds for dropping the link. Instead
// we guard against two real corruption patterns:
//
//  1. Self-reference (parent_id == item.id).
//  2. A cycle discovered by walking existing parent_id links back to
//     the same row.
//  3. A parent row that EXISTS but is not a tool_call (the invariant
//     violation the spec actually cares about: a text item attached
//     to another text item).
//
// Returns (true, reason) on drop; (false, "") when the link is either
// valid or refers to a yet-unseen row that may arrive later. Lookup
// failures downgrade to (false, "") — a transient store error never
// blocks persistence.
func (r *Router) shouldDropParentID(threadID, itemID, parentID string) (bool, string) {
	if parentID == itemID {
		return true, "self reference"
	}
	seen := map[string]struct{}{itemID: {}}
	current := parentID
	for hops := 0; hops < 16; hops++ {
		if _, cycle := seen[current]; cycle {
			return true, "cycle detected"
		}
		seen[current] = struct{}{}
		parent, found, err := r.store.GetThreadItem(threadID, current)
		if err != nil {
			// Transient lookup error — keep the link, the store write
			// below will surface any hard error.
			return false, ""
		}
		if !found {
			// Parent hasn't been persisted yet (common for subagent
			// text deltas arriving before the Task tool_call row).
			// Leave the link — the row may materialise shortly.
			return false, ""
		}
		if parent.Kind != itemKindToolCall {
			return true, fmt.Sprintf("parent kind %q is not tool_call", parent.Kind)
		}
		if parent.ParentID == "" {
			return false, ""
		}
		current = parent.ParentID
	}
	return true, "parent chain too deep"
}

// PersistItem is the public chokepoint for non-provider callers that
// need the same UpsertItem-then-emit semantics the router uses
// internally (user-typed messages, send-failure errors). Routing every
// persistence through this guarantees parent_id validation, emit order,
// and metrics stay consistent regardless of source.
func (r *Router) PersistItem(item store.Item, payload *store.Payload) error {
	return r.persistItem(item, payload)
}

// NextErrorSequence returns the next per-turn error sequence number for
// the given thread + turn + scope. Exposed so app-layer callers (e.g.
// send-failure persistence) can build error IDs via the same counter
// EventError uses, preventing collisions when a provider error lands on
// the same turn as a user-visible send failure.
func (r *Router) NextErrorSequence(threadID string, turnIndex int, scope string) int {
	return r.nextErrorSequence(threadID, turnIndex, scope)
}

// NewErrorID builds a deterministic error:<turnIndex>[:<scope>]:<seq>
// id. Exposed alongside NextErrorSequence so callers outside triage can
// build rows that slot in next to provider-sourced errors without
// reimplementing the id format.
func NewErrorID(turnIndex int, scope string, seq int) string {
	return nextErrorID(turnIndex, scope, seq)
}
