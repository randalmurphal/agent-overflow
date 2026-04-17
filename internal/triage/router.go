// Package triage classifies provider events and routes them to the frontend
// (small/inline) or SQLite (heavy payloads like diffs and command output).
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

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
	textAccumulators      map[string]*strings.Builder // threadID → accumulated assistant text
	reasoningAccumulators map[string]*strings.Builder // threadID → accumulated Codex reasoning
	pendingCommandDiffs   map[string]pendingCommandInlineDiff
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
		store: st,
		emit:  emit,
		tracer: tracenoop.NewTracerProvider().Tracer("triage/router"),
		metrics: TurnMetrics{
			TurnsStarted:      ts,
			TurnsCompleted:    tc,
			TurnsErrored:      te,
			ItemsPersisted:    ip,
			PayloadsPersisted: pp,
		},
		textAccumulators:      make(map[string]*strings.Builder),
		reasoningAccumulators: make(map[string]*strings.Builder),
		pendingCommandDiffs:   make(map[string]pendingCommandInlineDiff),
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
	case provider.EventApprovalRequest,
		provider.EventApprovalResolved,
		provider.EventSessionStatus,
		provider.EventToolProgress,
		provider.EventCompactBoundary,
		provider.EventRateLimits,
		provider.EventError,
		provider.EventBackgroundStart:
		return r.emitInline(evt)
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
	case provider.EventBackgroundDelta:
		return nil
	case provider.EventBackgroundComplete:
		return r.persistHeavy(evt, "full_text", string(provider.ItemBackgroundDone))
	case provider.EventDiff:
		return r.handleDiff(evt)
	case provider.EventCommandOutput:
		return r.persistHeavy(evt, "command_output", string(provider.ItemCommandExecution))
	case provider.EventThinking:
		return r.handleThinking(evt)
	case provider.EventProposedPlan:
		return r.persistHeavy(evt, "proposed_plan", string(provider.ItemProposedPlan))
	default:
		log.Printf("triage: unhandled event kind: %s", evt.Kind)
		r.emit("provider:event", evt)
		return nil
	}
}

func (r *Router) handleToolStart(evt provider.ProviderEvent) error {
	if err := r.persistFileChangeToolResult(evt); err != nil {
		return err
	}
	if err := r.capturePendingCommandInlineDiff(evt); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func (r *Router) handleToolComplete(evt provider.ProviderEvent) error {
	if err := r.persistFileChangeToolResult(evt); err != nil {
		return err
	}
	if err := r.persistCommandInlineDiffToolResult(evt); err != nil {
		return err
	}
	return r.emitInline(evt)
}

func (r *Router) handleDiff(evt provider.ProviderEvent) error {
	if err := r.persistHeavy(evt, "diff", string(provider.ItemDiff)); err != nil {
		return err
	}

	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		return nil
	}
	return r.upgradeSummaryOnlyToolResults(evt.ThreadID, turnIndex, evt.Content)
}

func (r *Router) handleTextDelta(evt provider.ProviderEvent) error {
	r.accumulate(r.textAccumulators, evt.ThreadID, evt.Content)
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
		r.emit("provider:event", evt)
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
	r.emit("provider:event", evt)
	return nil
}

func (r *Router) handleThreadRename(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateTitle(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread title: %w", err)
	}
	return nil
}

// handleTurnStart forwards the event inline (so the UI gets the turn marker)
// and, if a checkpoint store is wired up, captures a baseline snapshot of the
// workspace. Checkpoint failure is NOT fatal to the turn — it surfaces as a
// `provider:event_error` event the frontend can display, but the turn proceeds.
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	// Emit the turn-start event first so the UI keeps its existing behaviour
	// even if checkpoint capture stalls (it shouldn't — capture is ~50 ms for
	// 500 files — but we don't want to couple triage latency to git).
	r.emit("provider:event", evt)
	r.openTurnSpan(evt)
	r.captureBaselineForTurn(context.Background(), evt.ThreadID)
	return nil
}

// openTurnSpan begins a turn.lifecycle span for the incoming turn. Any
// existing span for the thread is closed first — the provider sometimes
// re-sends EventTurnStart (e.g. after a Claude interrupt/re-init) and we
// don't want to leak orphan spans.
func (r *Router) openTurnSpan(evt provider.ProviderEvent) {
	r.mu.Lock()
	tracer := r.tracer
	if existing, ok := r.turnSpans[evt.ThreadID]; ok {
		delete(r.turnSpans, evt.ThreadID)
		r.mu.Unlock()
		existing.End()
		r.mu.Lock()
	}
	thread, err := r.store.GetThread(evt.ThreadID)
	r.mu.Unlock()
	if err != nil {
		// We don't know the provider/model without the thread; drop the
		// span rather than record misleading attributes.
		return
	}
	turnIndex, _ := r.store.LastTurnIndex(evt.ThreadID)
	_, span := tracer.Start(context.Background(), "turn.lifecycle",
		trace.WithAttributes(
			attribute.String("thread.id", evt.ThreadID),
			attribute.String("provider", thread.Provider),
			attribute.String("model", thread.Model),
			attribute.Int("turn.index", turnIndex),
		),
	)
	r.mu.Lock()
	r.turnSpans[evt.ThreadID] = span
	r.mu.Unlock()
	r.metrics.TurnsStarted.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", thread.Provider),
		),
	)
}

// captureBaselineForTurn runs checkpoint capture + SQLite persistence for the
// current turn. Errors are logged and surfaced as activity events so the UI
// can show "checkpoints unavailable" without blocking the turn.
//
// The operation is idempotent by design. A provider can fire EventTurnStart
// more than once for the same (thread, turn) — e.g. Claude resending
// system.init after an interrupt — and an earlier capture attempt can have
// partially succeeded (git ref written but DB insert failed, or vice versa).
// Before writing a fresh pair we tear down any stale row + ref so we never
// have to reconcile drift between git and SQLite later. If SaveCheckpoint
// fails after the new ref is written, we remove the new ref so the two
// sides stay in lockstep — either both exist, or neither does.
func (r *Router) captureBaselineForTurn(ctx context.Context, threadID string) {
	cap := r.checkpointStore()
	if cap == nil {
		return
	}
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		log.Printf("triage: checkpoint load thread %s: %v", threadID, err)
		return
	}
	workspace := checkpointWorkspacePath(thread)
	if workspace == "" {
		return
	}
	if !cap.IsGitRepository(ctx, workspace) {
		r.emit("checkpoint:unavailable", map[string]any{
			"threadId": threadID,
			"reason":   "not-a-git-repo",
		})
		return
	}
	turnIndex, err := r.store.LastTurnIndex(threadID)
	if err != nil {
		log.Printf("triage: checkpoint turn index %s: %v", threadID, err)
		return
	}
	if r.markTurnCaptured(threadID, turnIndex) {
		return // already captured for this (thread, turn)
	}

	// Idempotency guard: any pre-existing row for (thread, turn) means a
	// previous capture partially succeeded or we're re-capturing. Drop both
	// sides — DB row first (SQLite write is the cheap one to roll back),
	// then the backing git ref — so the upcoming capture starts clean.
	if staleRef, hadRow, err := r.store.DeleteCheckpointByThreadTurn(threadID, turnIndex); err != nil {
		log.Printf("triage: checkpoint stale row thread=%s turn=%d: %v", threadID, turnIndex, err)
		r.unmarkTurnCaptured(threadID, turnIndex)
		return
	} else if hadRow && staleRef != "" {
		if err := cap.DeleteRef(ctx, workspace, staleRef); err != nil {
			log.Printf("triage: checkpoint delete stale ref %s: %v", staleRef, err)
		}
	}

	ref, err := cap.CaptureBaseline(ctx, workspace, threadID, turnIndex)
	if err != nil {
		r.unmarkTurnCaptured(threadID, turnIndex)
		r.emit("checkpoint:error", map[string]any{
			"threadId":  threadID,
			"turnIndex": turnIndex,
			"error":     err.Error(),
		})
		log.Printf("triage: checkpoint capture thread=%s turn=%d: %v", threadID, turnIndex, err)
		return
	}
	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:            uuid.NewString(),
		ThreadID:      threadID,
		TurnIndex:     turnIndex,
		RefName:       ref,
		CapturedAt:    now,
		WorkspacePath: workspace,
	}
	if err := r.store.SaveCheckpoint(record); err != nil {
		log.Printf("triage: checkpoint persist thread=%s turn=%d: %v", threadID, turnIndex, err)
		// DB row never landed — don't let the git ref linger.
		if derr := cap.DeleteRef(ctx, workspace, ref); derr != nil {
			log.Printf("triage: checkpoint rollback ref %s: %v", ref, derr)
		}
		r.unmarkTurnCaptured(threadID, turnIndex)
		return
	}
	r.emit("checkpoint:captured", map[string]any{
		"threadId":   threadID,
		"turnIndex":  turnIndex,
		"refName":    ref,
		"capturedAt": now,
	})
}

func (r *Router) checkpointStore() CheckpointCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpoints
}

func (r *Router) markTurnCaptured(threadID string, turnIndex int) bool {
	key := fmt.Sprintf("%s|%d", threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capturedTurns[key] {
		return true
	}
	r.capturedTurns[key] = true
	return false
}

func (r *Router) unmarkTurnCaptured(threadID string, turnIndex int) {
	key := fmt.Sprintf("%s|%d", threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.capturedTurns, key)
}

// checkpointWorkspacePath picks the on-disk directory we should snapshot.
// Prefers worktree_path (where the agent actually edits) over workspace_path.
func checkpointWorkspacePath(t store.Thread) string {
	if t.WorktreePath != "" {
		return t.WorktreePath
	}
	return t.WorkspacePath
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	text, reasoning := r.drainBoth(evt.ThreadID)
	planMarkdown := provider.ExtractProposedPlanMarkdown(text)
	if planMarkdown != "" {
		text = provider.StripProposedPlanBlocks(text)
	}

	var persistErr error
	if planMarkdown != "" {
		err := r.persistHeavy(provider.ProviderEvent{
			Kind:            provider.EventProposedPlan,
			ThreadID:        evt.ThreadID,
			Content:         planMarkdown,
			ParentToolUseID: evt.ParentToolUseID,
			Timestamp:       evt.Timestamp,
		}, "proposed_plan", string(provider.ItemProposedPlan))
		if persistErr == nil && err != nil {
			persistErr = fmt.Errorf("persist proposed plan: %w", err)
		}
	}
	if text != "" {
		persistErr = r.persistTurnText(evt.ThreadID, text, evt.ParentToolUseID)
	}
	if reasoning != "" {
		err := r.persistHeavy(provider.ProviderEvent{
			Kind:            provider.EventThinking,
			ThreadID:        evt.ThreadID,
			Content:         reasoning,
			ParentToolUseID: evt.ParentToolUseID,
			Timestamp:       evt.Timestamp,
		}, "thinking", string(provider.ItemThinking))
		if persistErr == nil && err != nil {
			persistErr = fmt.Errorf("persist reasoning: %w", err)
		}
	}

	r.emit("provider:event", evt)
	r.closeTurnSpan(evt.ThreadID, persistErr)
	return persistErr
}

// closeTurnSpan ends the live turn span for the thread, flagging it as
// errored when persistErr is non-nil. Safe to call with no active span.
func (r *Router) closeTurnSpan(threadID string, persistErr error) {
	r.mu.Lock()
	span, ok := r.turnSpans[threadID]
	if ok {
		delete(r.turnSpans, threadID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if persistErr != nil {
		span.RecordError(persistErr)
		r.metrics.TurnsErrored.Add(context.Background(), 1)
	} else {
		r.metrics.TurnsCompleted.Add(context.Background(), 1)
	}
	span.End()
}

func (r *Router) handleThinking(evt provider.ProviderEvent) error {
	if evt.ItemID != "" {
		return r.persistHeavy(evt, "thinking", string(provider.ItemThinking))
	}
	r.accumulate(r.reasoningAccumulators, evt.ThreadID, evt.Content)
	return nil
}

func (r *Router) handleTokenUsage(evt provider.ProviderEvent) error {
	usage, ok := parseTokenUsage(evt.Meta)
	if !ok {
		return r.emitInline(evt)
	}

	model, err := r.lookupThreadModel(evt.ThreadID)
	if err != nil {
		log.Printf("triage: lookup thread model: %v", err)
		return r.emitInline(evt)
	}
	if model == "" {
		return r.emitInline(evt)
	}

	usage.TotalCostUSD = provider.CalculateCost(model, usage)
	if usage.TotalCostUSD == 0 {
		return r.emitInline(evt)
	}

	meta, err := json.Marshal(usage)
	if err != nil {
		log.Printf("triage: marshal token usage: %v", err)
		return r.emitInline(evt)
	}

	evt.Meta = meta
	return r.emitInline(evt)
}

func (r *Router) emitInline(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
	return nil
}

func parseTokenUsage(meta json.RawMessage) (provider.TokenUsage, bool) {
	if len(meta) == 0 {
		return provider.TokenUsage{}, false
	}

	var usage provider.TokenUsage
	if err := json.Unmarshal(meta, &usage); err != nil {
		return provider.TokenUsage{}, false
	}
	return usage, true
}

func (r *Router) lookupThreadModel(threadID string) (string, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	return thread.Model, nil
}

func (r *Router) accumulate(target map[string]*strings.Builder, threadID, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	acc, ok := target[threadID]
	if !ok {
		acc = &strings.Builder{}
		target[threadID] = acc
	}
	acc.WriteString(content)
}

// drainBoth atomically drains both text and reasoning accumulators in a single
// critical section. This prevents a concurrent handleThinking from writing
// between two separate drain calls, which would attribute reasoning to the
// wrong turn.
func (r *Router) drainBoth(threadID string) (text, reasoning string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if acc, ok := r.textAccumulators[threadID]; ok && acc.Len() > 0 {
		text = acc.String()
	}
	delete(r.textAccumulators, threadID)

	if acc, ok := r.reasoningAccumulators[threadID]; ok && acc.Len() > 0 {
		reasoning = acc.String()
	}
	delete(r.reasoningAccumulators, threadID)

	return text, reasoning
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks. Also flags the
// thread as "stopped" so any event that arrives afterward — typically a
// readLoop line that was already in-flight when StopSession returned — is
// dropped instead of persisting under the torn-down session (Bug B5).
func (r *Router) CleanupThread(threadID string) {
	r.mu.Lock()
	// Set the stopped flag BEFORE dropping other state so Handle observes
	// a consistent snapshot: any concurrent Handle call either sees a live
	// thread with full state, or a stopped thread with no state.
	r.stoppedThreads[threadID] = struct{}{}
	var orphanSpan trace.Span
	if span, ok := r.turnSpans[threadID]; ok {
		orphanSpan = span
		delete(r.turnSpans, threadID)
	}
	delete(r.textAccumulators, threadID)
	delete(r.reasoningAccumulators, threadID)
	for key, pending := range r.pendingCommandDiffs {
		if pending.ThreadID == threadID {
			delete(r.pendingCommandDiffs, key)
		}
	}
	prefix := threadID + "|"
	for key := range r.capturedTurns {
		if strings.HasPrefix(key, prefix) {
			delete(r.capturedTurns, key)
		}
	}
	r.mu.Unlock()

	if orphanSpan != nil {
		// Closing outside the lock avoids self-deadlock when the tracer's
		// OnEnd hook reaches for any shared resource.
		orphanSpan.End()
	}
}

// isThreadStopped returns true when CleanupThread has been called for
// threadID and no subsequent EventInit has re-activated it.
func (r *Router) isThreadStopped(threadID string) bool {
	r.mu.Lock()
	_, stopped := r.stoppedThreads[threadID]
	r.mu.Unlock()
	return stopped
}

// markThreadActive clears the stopped flag, called on EventInit so a
// restarted session can persist again. No-op when the flag was already
// clear.
func (r *Router) markThreadActive(threadID string) {
	r.mu.Lock()
	delete(r.stoppedThreads, threadID)
	r.mu.Unlock()
}

func (r *Router) persistTurnText(threadID, content, parentToolUseID string) error {
	now := time.Now().UnixMilli()
	turnIndex, err := r.store.LastTurnIndex(threadID)
	if err != nil {
		log.Printf("triage: last turn index: %v (defaulting to 0)", err)
		turnIndex = 0
	}

	item := store.Item{
		ID:              uuid.New().String(),
		ThreadID:        threadID,
		TurnIndex:       turnIndex,
		Kind:            string(provider.ItemText),
		Role:            "assistant",
		Summary:         content,
		ParentToolUseID: parentToolUseID,
		CreatedAt:       now,
	}
	// AppendItem computes MAX+1 atomically with the insert so a
	// concurrent writer on the same (thread, turn) cannot collide.
	if _, err := r.store.AppendItem(item); err != nil {
		return fmt.Errorf("persist assistant text: %w", err)
	}
	r.metrics.ItemsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", string(provider.ItemText))))
	return nil
}

// persistHeavy extracts meta, stores payload + item, emits meta to frontend.
func (r *Router) persistHeavy(evt provider.ProviderEvent, payloadKind string, itemKind string) error {
	now := time.Now().UnixMilli()
	metaJSON := buildPayloadMeta(payloadKind, evt)
	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		log.Printf("triage: last turn index: %v (defaulting to 0)", err)
		turnIndex = 0
	}

	payloadID := uuid.New().String()
	itemID := evt.ItemID
	if itemID == "" {
		itemID = uuid.New().String()
	}

	var hasExisting bool
	if evt.Replace {
		existing, found, findErr := r.store.FindTurnItem(evt.ThreadID, turnIndex, itemKind)
		if findErr != nil {
			log.Printf("triage: find turn item: %v", findErr)
		} else if found && existing.PayloadID != "" {
			hasExisting = true
			payloadID = existing.PayloadID
			itemID = existing.ID
		} else if found && existing.ID != "" {
			hasExisting = true
			itemID = existing.ID
		}
	}

	r.emitPayloadMeta(payloadID, evt.ThreadID, payloadKind, metaJSON, now)

	payload := store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
	if evt.Replace {
		return r.replaceHeavy(evt, payloadKind, itemKind, payload, itemID, metaJSON, now, turnIndex, hasExisting)
	}
	return r.insertHeavyItemAndPayload(evt, payloadKind, itemKind, payload, itemID, metaJSON, now, turnIndex)
}

func (r *Router) replaceHeavy(
	evt provider.ProviderEvent,
	payloadKind, itemKind string,
	payload store.Payload,
	itemID, metaJSON string,
	now int64,
	turnIndex int,
	hasExisting bool,
) error {
	if err := r.store.UpsertTurnPayload(evt.ThreadID, turnIndex, payloadKind, payload); err != nil {
		return fmt.Errorf("persist payload: %w", err)
	}

	summary := buildSummary(payloadKind, metaJSON)
	if hasExisting {
		if err := r.store.UpdateItemPayload(itemID, payload.ID, summary, now); err != nil {
			return fmt.Errorf("persist item: %w", err)
		}
		return nil
	}
	return r.insertHeavyItem(evt, payloadKind, itemKind, payload.ID, itemID, metaJSON, now, turnIndex)
}

func (r *Router) insertHeavyItem(
	evt provider.ProviderEvent,
	payloadKind, itemKind, payloadID, itemID, metaJSON string,
	now int64,
	turnIndex int,
) error {
	item := store.Item{
		ID:              itemID,
		ThreadID:        evt.ThreadID,
		TurnIndex:       turnIndex,
		Kind:            itemKind,
		Role:            "assistant",
		Summary:         buildSummary(payloadKind, metaJSON),
		PayloadID:       payloadID,
		ParentToolUseID: evt.ParentToolUseID,
		CreatedAt:       now,
	}
	if _, err := r.store.AppendItem(item); err != nil {
		return fmt.Errorf("persist item: %w", err)
	}
	r.metrics.ItemsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", itemKind)))
	r.metrics.PayloadsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", payloadKind)))

	return nil
}

// insertHeavyItemAndPayload writes the payload and its matching item in
// a single transaction (Bug B10). A failure in either half aborts both,
// so the store never retains an orphan payload row when item insertion
// fails (or vice versa). AppendItemWithPayload computes MAX+1 inside
// the transaction so two concurrent heavy-item writers for the same
// (thread, turn) cannot land on the same item_index.
func (r *Router) insertHeavyItemAndPayload(
	evt provider.ProviderEvent,
	payloadKind, itemKind string,
	payload store.Payload,
	itemID, metaJSON string,
	now int64,
	turnIndex int,
) error {
	item := store.Item{
		ID:              itemID,
		ThreadID:        evt.ThreadID,
		TurnIndex:       turnIndex,
		Kind:            itemKind,
		Role:            "assistant",
		Summary:         buildSummary(payloadKind, metaJSON),
		PayloadID:       payload.ID,
		ParentToolUseID: evt.ParentToolUseID,
		CreatedAt:       now,
	}
	if _, err := r.store.AppendItemWithPayload(item, payload); err != nil {
		return fmt.Errorf("persist item+payload: %w", err)
	}
	r.metrics.ItemsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", itemKind)))
	r.metrics.PayloadsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", payloadKind)))

	return nil
}

func (r *Router) emitPayloadMeta(payloadID, threadID, kind, meta string, createdAt int64) {
	r.emit("provider:meta", struct {
		ID        string `json:"id"`
		ThreadID  string `json:"threadId"`
		Kind      string `json:"kind"`
		Meta      string `json:"meta"`
		CreatedAt int64  `json:"createdAt"`
	}{
		ID:        payloadID,
		ThreadID:  threadID,
		Kind:      kind,
		Meta:      meta,
		CreatedAt: createdAt,
	})
}

func buildPayloadMeta(payloadKind string, evt provider.ProviderEvent) string {
	switch payloadKind {
	case "diff":
		dm := ExtractDiffMeta(evt.Content)
		data, err := json.Marshal(dm)
		if err != nil {
			log.Printf("triage: marshal diff meta: %v", err)
			return "{}"
		}
		return string(data)
	case "command_output":
		cm := ExtractCommandOutputMeta(evt.Content, "", 0)
		if evt.Meta != nil {
			var parsed struct {
				Command  string `json:"command"`
				ExitCode int    `json:"exitCode"`
			}
			if json.Unmarshal(evt.Meta, &parsed) == nil {
				cm = ExtractCommandOutputMeta(evt.Content, parsed.Command, parsed.ExitCode)
			}
		}
		data, err := json.Marshal(cm)
		if err != nil {
			log.Printf("triage: marshal command output meta: %v", err)
			return "{}"
		}
		return string(data)
	case "thinking":
		tm := ExtractThinkingMeta(evt.Content)
		data, err := json.Marshal(tm)
		if err != nil {
			log.Printf("triage: marshal thinking meta: %v", err)
			return "{}"
		}
		return string(data)
	case "proposed_plan":
		pm := ExtractProposedPlanMeta(evt.Content)
		data, err := json.Marshal(pm)
		if err != nil {
			log.Printf("triage: marshal proposed plan meta: %v", err)
			return "{}"
		}
		return string(data)
	default:
		return "{}"
	}
}

// buildSummary creates a short human-readable summary from meta.
func buildSummary(payloadKind, metaJSON string) string {
	switch payloadKind {
	case "diff":
		var dm DiffMeta
		if json.Unmarshal([]byte(metaJSON), &dm) == nil {
			return fmt.Sprintf("%s: +%d/-%d %s", dm.ChangeKind, dm.Insertions, dm.Deletions, dm.FilePath)
		}
	case "command_output":
		var cm CommandOutputMeta
		if json.Unmarshal([]byte(metaJSON), &cm) == nil {
			return fmt.Sprintf("$ %s (exit %d, %d lines)", cm.Command, cm.ExitCode, cm.LineCount)
		}
	case "thinking":
		var tm ThinkingMeta
		if json.Unmarshal([]byte(metaJSON), &tm) == nil {
			return tm.Preview
		}
	case "proposed_plan":
		var pm ProposedPlanMeta
		if json.Unmarshal([]byte(metaJSON), &pm) == nil && pm.Title != "" {
			return pm.Title
		}
	}
	return ""
}
