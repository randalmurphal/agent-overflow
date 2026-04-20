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
	store                 *store.Store
	emit                  func(eventName string, data any) // wraps app.Event.Emit
	checkpoints           CheckpointCapture                // nil-safe; no-op when nil
	tracer                trace.Tracer
	metrics               TurnMetrics
	mu                    sync.Mutex
	pendingCommandDiffs   map[string]pendingCommandInlineDiff
	pendingApprovals      map[string]pendingApprovalState
	pendingApprovalItems  map[string]string
	interruptQueue        map[string][]queuedPersistence
	openTurns             map[string]int
	segmentIndexByScope   map[string]int
	blockIndexByScope     map[string]int
	activeTextBlocks      map[string]bool
	activeThinkingBlocks  map[string]bool
	streamingItemCounts   map[string]int
	errorSeqByScope       map[string]int
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
		pendingCommandDiffs:   make(map[string]pendingCommandInlineDiff),
		pendingApprovals:      make(map[string]pendingApprovalState),
		pendingApprovalItems:  make(map[string]string),
		interruptQueue:        make(map[string][]queuedPersistence),
		openTurns:             make(map[string]int),
		segmentIndexByScope:   make(map[string]int),
		blockIndexByScope:     make(map[string]int),
		activeTextBlocks:      make(map[string]bool),
		activeThinkingBlocks:  make(map[string]bool),
		streamingItemCounts:   make(map[string]int),
		errorSeqByScope:       make(map[string]int),
		capturedTurns:         make(map[string]bool),
		turnSpans:             make(map[string]trace.Span),
		stoppedThreads:        make(map[string]struct{}),
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
	case provider.EventSessionStatus,
		provider.EventRateLimits:
		return r.emitInline(evt)
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
		r.closeTurnSpan(evt.ThreadID, errors.New(firstNonEmptyString(evt.Content, "provider error")))
	}

	scope := strings.TrimSpace(evt.ParentToolUseID)
	item := store.Item{
		ID:        nextErrorID(turnIndex, scope, r.nextErrorSequence(evt.ThreadID, turnIndex, scope)),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      "error",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   firstNonEmptyString(strings.TrimSpace(evt.Content), "Provider error"),
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

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
