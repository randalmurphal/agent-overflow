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
	"time"

	"agent-overflow/internal/diffsummary"
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
// without a matching case here. The exhaustiveness test flags this so new
// provider event kinds can't slip through without an explicit routing
// decision.
var ErrUnhandledEventKind = errors.New("triage: unhandled event kind")

// CheckpointCapture is the subset of checkpoint.Store that the router calls
// at turn-start. Kept as an interface so tests can inject a stub.
type CheckpointCapture interface {
	IsGitRepository(ctx context.Context, workspace string) bool
	CaptureBaseline(ctx context.Context, workspace, threadID string, turnCount int) (string, error)
	DiffRefToRef(ctx context.Context, workspace, fromRef, toRef string) ([]byte, error)
	DiffRefToRefSummary(ctx context.Context, workspace, fromRef, toRef string) ([]diffsummary.File, error)
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
	store                  *store.Store
	emit                   func(eventName string, data any) // wraps app.Event.Emit
	checkpoints            CheckpointCapture                // nil-safe; no-op when nil
	tracer                 trace.Tracer
	metrics                TurnMetrics
	mu                     sync.Mutex
	pendingCommandDiffs    map[string]pendingCommandInlineDiff
	pendingApprovals       map[string]pendingApprovalState
	pendingApprovalItems   map[string]string
	pendingUserInputs      map[string]provider.UserInputRequest
	interruptQueue         map[string][]queuedPersistence
	openTurns              map[string]int
	segmentIndexByScope    map[string]int
	blockIndexByScope      map[string]int
	activeTextBlocks       map[string]bool
	activeThinkingBlocks   map[string]bool
	streamingItemCounts    map[string]int
	errorSeqByScope        map[string]int
	notificationSeqByScope map[string]int
	// streamPersistBuffers decouple the live UI stream from durable
	// history writes. Text/thinking deltas emit immediately on ordered
	// provider:item_event deltas, then flush to SQLite by interval, byte
	// threshold, or lifecycle boundary.
	streamPersistBuffers map[string]*streamPersistBuffer
	// capturedTurns guards against double-capture when a provider emits
	// multiple EventTurnStart events for the same (thread, turn) — which
	// happens when Claude re-sends a system.init after an interrupt.
	capturedTurns map[string]bool // key = threadID|turnIndex
	// settledTurns marks turns whose handleTurnComplete has already run
	// to completion (provider:turn_completed emitted, checkpoint
	// captured, streaming items settled). A second EventTurnComplete for
	// a settled turn is the multi-result-per-turn wire pattern (Claude
	// CLI synthesizes a `type:"user"` envelope from a task_notification
	// → second `result` envelope) or the synthetic-truncate-then-real
	// race; in either case the second handler invocation is a no-op so
	// the frontend doesn't see a duplicate provider:turn_completed and
	// the checkpoint isn't captured twice. Cleared by setOpenTurn (so a
	// re-init can re-settle the same turn) and CleanupThread (session
	// teardown). Key = threadID|turnIndex.
	settledTurns map[string]bool
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
	// codexBackground is the Codex-specific background-terminal projector
	// state, keyed by threadID. Tracks inProgress unifiedExec items +
	// spawn_agent rows with running children so we can stamp
	// is_background=true on the first wire-typed yield signal (text /
	// reasoning delta) or at turn/completed (the catchall). See
	// codex_background.go for the lifecycle details and invariant 25 for
	// the wire-typed-signal rule this implements.
	codexBackground map[string]*codexBackgroundState
	// terminalInteractionSeq counts persisted "Waited for background
	// terminal" rows per (thread, turn, processID) so each poll lands
	// at its own id (waited:<pid>:<turn>:<seq>). Bounded the same way
	// other per-turn counters are: prefix-swept on clearOpenTurn and
	// CleanupThread so a long-lived session doesn't accumulate stale
	// entries. See terminal_interaction.go for the handler.
	terminalInteractionSeq map[string]int
	// openAPIRetryRows flags threads whose current api_retry row is in
	// status=running and therefore eligible to flip on the next
	// forward-progress event. The hot streaming path
	// (maybeMarkAPIRetryCompleted, called per text/thinking/tool event)
	// short-circuits when the flag is unset so the common case avoids
	// a SQLite GetThreadItem on every text delta. Set in handleAPIRetry
	// when persisting a running row; cleared after the flip completes
	// or when the turn closes via clearOpenTurn / CleanupThread.
	openAPIRetryRows map[string]bool
	// pendingToolPaths stages the workspace paths a tool will write
	// between EventToolStart and EventToolComplete. Keyed by
	// `<threadID>|<itemID>`. On a successful complete we move the
	// staged paths into committedToolPaths; on failure we drop them so
	// rejected tools don't poison the per-turn revert set.
	pendingToolPaths map[string][]string
	// committedToolPaths accumulates workspace paths the agent
	// successfully wrote during a turn. Keyed by
	// `<threadID>|<turnIndex>`. Drained by captureCompletedTurnCheckpoint
	// when the per-turn checkpoint persists, then cleared in
	// clearOpenTurn / CleanupThread.
	committedToolPaths map[string][]string
	// eventHook is a test-only seam: when set, the Router invokes it for
	// every Handle call AFTER the routing switch runs. Production code
	// never sets a hook (the call site in Handle is nil-checked so the
	// production path pays only one branch). Tests install a hook that
	// forwards events to a channel so test assertions can synchronize on
	// the routing pipeline without depending on a retired
	// `provider:event` emission.
	eventHook func(provider.ProviderEvent)
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
		pendingUserInputs:          make(map[string]provider.UserInputRequest),
		interruptQueue:             make(map[string][]queuedPersistence),
		openTurns:                  make(map[string]int),
		segmentIndexByScope:        make(map[string]int),
		blockIndexByScope:          make(map[string]int),
		activeTextBlocks:           make(map[string]bool),
		activeThinkingBlocks:       make(map[string]bool),
		streamingItemCounts:        make(map[string]int),
		errorSeqByScope:            make(map[string]int),
		notificationSeqByScope:     make(map[string]int),
		streamPersistBuffers:       make(map[string]*streamPersistBuffer),
		capturedTurns:              make(map[string]bool),
		settledTurns:               make(map[string]bool),
		turnSpans:                  make(map[string]trace.Span),
		stoppedThreads:             make(map[string]struct{}),
		unknownSessionStatusLogged: make(map[string]struct{}),
		codexBackground:            make(map[string]*codexBackgroundState),
		terminalInteractionSeq:     make(map[string]int),
		pendingToolPaths:           make(map[string][]string),
		committedToolPaths:         make(map[string][]string),
		openAPIRetryRows:           make(map[string]bool),
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

// SetEventHook installs a test-only observer that fires after every
// Handle call. Production code must leave this nil — the hook exists so
// tests can synchronize on the routing pipeline without depending on a
// wire-level emission. Pass nil to clear.
func (r *Router) SetEventHook(hook func(provider.ProviderEvent)) {
	r.mu.Lock()
	r.eventHook = hook
	r.mu.Unlock()
}

// Handle processes a provider event: persists heavy payloads, forwards inline events.
func (r *Router) Handle(evt provider.ProviderEvent) error {
	r.inflight.Add(1)
	defer r.inflight.Done()
	defer r.fireEventHook(evt)

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
	// Forward-progress for the api_retry row: any wire event that
	// proves the provider went through with the next API call flips an
	// open retry row from running to completed. The list excludes
	// EventAPIRetry (would close the row it just opened) and
	// EventError (the error closes the turn — the retry row's running
	// state is correct context for the failure that followed). See
	// api_retry.go:maybeMarkAPIRetryCompleted.
	if isAPIRetryForwardProgress(evt.Kind) {
		r.maybeMarkAPIRetryCompleted(evt.ThreadID)
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
	case provider.EventUserInputRequest:
		return r.handleUserInputRequest(evt)
	case provider.EventUserInputResolved:
		return r.handleUserInputResolved(evt)
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
	case provider.EventTodoUpdate:
		return r.handleTodoUpdate(evt)
	case provider.EventNotification:
		return r.handleTimelineNotification(evt)
	case provider.EventAPIRetry:
		return r.handleAPIRetry(evt)
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
	case provider.EventBackgroundTaskTerminal:
		return r.handleBackgroundTaskTerminal(evt)
	case provider.EventBackgroundTaskNotification:
		return r.handleBackgroundTaskNotification(evt)
	case provider.EventSubagentNotification:
		return r.handleSubagentNotification(evt)
	case provider.EventTerminalInteraction:
		return r.handleTerminalInteraction(evt)
	case provider.EventDiff:
		return r.handleDiff(evt)
	case provider.EventCommandOutput:
		return r.handleCommandOutput(evt)
	case provider.EventThinking:
		return r.handleThinking(evt)
	case provider.EventProposedPlan:
		return r.handleProposedPlan(evt)
	default:
		// No-op: the event has no routing decision. Return the sentinel so
		// the exhaustiveness test in router_test.go can flag the drift.
		return fmt.Errorf("%w: %s", ErrUnhandledEventKind, evt.Kind)
	}
}

// fireEventHook runs the installed test-only observer. Deferred from
// Handle so the hook fires after the routing switch — callers can rely
// on persistence and emissions having completed before the hook runs.
func (r *Router) fireEventHook(evt provider.ProviderEvent) {
	r.mu.Lock()
	hook := r.eventHook
	r.mu.Unlock()
	if hook != nil {
		hook(evt)
	}
}

func (r *Router) settleStreamingBeforeTimelineBoundary(evt provider.ProviderEvent, boundary string) {
	if !r.hasActiveStreamingItem(evt.ThreadID) {
		return
	}
	if strings.TrimSpace(evt.ParentToolUseID) == "" {
		turnIndex, err := r.currentTurnIndex(evt.ThreadID)
		if err != nil {
			log.Printf("triage: settle streaming before %s: %v", boundary, err)
			return
		}
		if err := r.settleTurnStreaming(evt.ThreadID, turnIndex, statusCompleted); err != nil {
			log.Printf("triage: settle streaming before %s: %v", boundary, err)
		}
		return
	}
	if err := r.settleStreamingScope(evt.ThreadID, evt.ParentToolUseID); err != nil {
		log.Printf("triage: settle streaming before %s: %v", boundary, err)
	}
}

func (r *Router) handleToolStart(evt provider.ProviderEvent) error {
	r.settleStreamingBeforeTimelineBoundary(evt, "tool start")
	r.observeCodexTopLevelToolBoundary(evt)
	if r.observeCodexToolStart(evt) {
		r.stageToolPaths(evt)
		return r.emitInline(evt)
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
	r.stageToolPaths(evt)
	return r.emitInline(evt)
}

func (r *Router) handleToolComplete(evt provider.ProviderEvent) error {
	if handled, err := r.observeCodexUnifiedExecComplete(evt); handled || err != nil {
		r.settleToolPaths(evt)
		return err
	}
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
	// Codex background projector handles persisted spawn_agent / wait_agent
	// completion state. Unified exec completion is handled above by
	// observeCodexUnifiedExecComplete because those commands are transient
	// tray state, not persisted launch rows.
	if err := r.observeCodexToolComplete(evt); err != nil {
		return err
	}
	r.settleToolPaths(evt)
	return r.emitInline(evt)
}

// stageToolPaths records the paths a tool is about to write in
// pendingToolPaths. Out-of-scope tools (Bash, Read, anything else without
// a recognized file_path / changes payload) are no-ops.
func (r *Router) stageToolPaths(evt provider.ProviderEvent) {
	paths := extractToolPaths(evt)
	if len(paths) == 0 {
		return
	}
	if evt.ItemID == "" {
		return
	}
	key := evt.ThreadID + "|" + evt.ItemID
	r.mu.Lock()
	r.pendingToolPaths[key] = paths
	r.mu.Unlock()
}

// settleToolPaths transfers staged paths into the per-turn committed
// set on a successful completion, or drops them on a failed/cancelled
// completion. Failed tools didn't write the file (or the write was
// aborted), so reverting their paths later would silently overwrite
// unrelated user edits.
//
// Provider parsers don't populate `evt.TurnIndex` (only `TurnID`), so
// the turn key normally comes from the router-tracked open-turn map.
// When no open turn is tracked (clearOpenTurn already fired for the
// prior turn but the wire kept emitting events — the multi-result-per-
// turn case where Claude's CLI synthesizes a `type:"user"` envelope
// from a task_notification), fall back to LastTurnIndex so second-half
// tool completes still accumulate paths for the same turn. The next
// user-send drains the cumulative paths into the prior turn's
// checkpoint via capturePriorTurnCheckpoint.
//
// The lock is released for the LastTurnIndex SQL call (matching the
// `currentTurnIndex` convention at turn_lifecycle.go:710 — never hold
// r.mu through a store call, since r.mu serializes ALL Handle dispatch
// across threads). After re-acquiring, we re-check stoppedThreads so a
// concurrent CleanupThread between the unlock and relock cannot leave
// stale path entries on a torn-down thread.
func (r *Router) settleToolPaths(evt provider.ProviderEvent) {
	if evt.ItemID == "" {
		return
	}
	key := evt.ThreadID + "|" + evt.ItemID
	r.mu.Lock()
	staged, ok := r.pendingToolPaths[key]
	delete(r.pendingToolPaths, key)
	if !ok || len(staged) == 0 {
		r.mu.Unlock()
		return
	}
	if !toolCallSucceeded(evt) {
		r.mu.Unlock()
		return
	}
	turnIndex, hasTurn := r.openTurns[evt.ThreadID]
	r.mu.Unlock()
	if !hasTurn {
		// Post-clearOpenTurn fallback: attribute the paths to whichever
		// turn the items table is already pinned to. Done outside the
		// lock — r.mu serializes Handle dispatch and we don't want to
		// block other threads' events on a SQL roundtrip.
		idx, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			// Lookup error — drop the paths rather than attribute them
			// to the wrong turn. Better to lose a single tool's revert
			// granularity than to corrupt the per-turn revert set.
			return
		}
		turnIndex = idx
	}
	turnKey := fmt.Sprintf("%s|%d", evt.ThreadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check stoppedThreads after re-acquiring: a concurrent
	// CleanupThread between the previous unlock and now will have
	// stamped this thread, swept committedToolPaths, and the write
	// below would re-introduce a stale entry that no future
	// CleanupThread is going to clean (CleanupThread is idempotent but
	// only fires once per session).
	if _, stopped := r.stoppedThreads[evt.ThreadID]; stopped {
		return
	}
	r.committedToolPaths[turnKey] = append(r.committedToolPaths[turnKey], staged...)
}

// drainCommittedToolPaths returns and removes the committed-paths slice
// for (threadID, turnIndex). Called by captureCompletedTurnCheckpoint at
// the moment the checkpoint row is being built, so the per-turn
// accumulator always drains in lockstep with the checkpoint write.
//
// pendingToolPaths cleanup is intentionally not done here — clearOpenTurn
// owns it (called from handleTurnComplete after the capture). That keeps
// the staging/committed cleanup symmetric across the early-return paths
// in captureCompletedTurnCheckpoint (no checkpoint store, non-git
// workspace, capture failure) where this drain never fires.
func (r *Router) drainCommittedToolPaths(threadID string, turnIndex int) []string {
	turnKey := fmt.Sprintf("%s|%d", threadID, turnIndex)
	r.mu.Lock()
	paths := r.committedToolPaths[turnKey]
	delete(r.committedToolPaths, turnKey)
	r.mu.Unlock()
	return paths
}

func (r *Router) handleInit(evt provider.ProviderEvent) error {
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

// handleTokenUsage accepts provider-normalized context-window snapshots only.
// Per-turn token/cost accounting lives on turn-complete metadata; summing those
// totals here would over-count multi-call turns and subagent work.
func (r *Router) handleTokenUsage(evt provider.ProviderEvent) error {
	window, ok := decodeContextWindow(evt.Meta)
	if !ok {
		return nil
	}
	return r.persistAndEmitContextWindow(evt.ThreadID, window)
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

func (r *Router) handleError(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		turnIndex, err = r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			turnIndex = 0
		}
	}

	fatal := isFatalProviderError(evt.Meta)

	// Fatal ordering, per chat-rewrite spec §"Live provider-crash flip":
	//   1. flip every streaming/running item in the active turn → errored
	//   2. create the error row
	//   3. drain any queued completions as errored
	//   4. synthesize EventTurnComplete{truncated:true} if no wire
	//      TurnComplete is expected (subprocess exit case) — not needed
	//      for a fatal EventError on an otherwise-alive session.
	//
	// The order matters for the frontend: by the time the error row
	// emits, every streaming/running item is already visibly flipped
	// to errored, so no "still-streaming text next to an error item"
	// visual state appears in the timeline.
	if fatal {
		if err := r.markTurnItemsErrored(evt.ThreadID, turnIndex, now); err != nil {
			return err
		}
	} else if r.hasActiveStreamingItem(evt.ThreadID) {
		r.settleStreamingBeforeTimelineBoundary(evt, "error item")
	}

	scope := strings.TrimSpace(evt.ParentToolUseID)
	// `assistant.error` from Claude carries the SDK enum on `meta.error`
	// (rate_limit, authentication_failed, ...). Persist as `api_error`
	// kind so the frontend can render the actionable copy / link
	// branch by enum (Add credits, Run /login, ...). Generic provider
	// errors stay as the existing `error` kind.
	itemKind := "error"
	itemMeta := ""
	if enum := apiErrorEnum(evt.Meta); enum != "" {
		itemKind = itemKindAPIError
		itemMeta = string(evt.Meta)
	}
	errorItem := store.Item{
		ID:        nextErrorID(turnIndex, scope, r.nextErrorSequence(evt.ThreadID, turnIndex, scope)),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKind,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsx.FirstNonEmptyTrimmed(evt.Content, "Provider error"),
		Meta:      itemMeta,
		ParentID:  eventParentID(evt),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(errorItem, nil); err != nil {
		return err
	}

	if fatal {
		if err := r.drainInterruptQueue(evt.ThreadID, true); err != nil {
			return err
		}
		r.clearOpenTurn(evt.ThreadID)
		r.closeTurnSpan(evt.ThreadID, errors.New(stringsx.FirstNonEmptyTrimmed(evt.Content, "provider error")))

		// Synthesize a truncated TurnComplete only when no wire
		// TurnComplete is expected downstream. `meta.expect_turn_complete`
		// opts in to "the subprocess is still alive, a real wire
		// TurnComplete will still arrive" — the common case for a fatal
		// EventError that represents a mid-turn refusal. Absent that
		// opt-in we assume the subprocess exited (stdout EOF, crash)
		// and emit the synthetic TurnComplete so the frontend working
		// indicator flips off even without a wire event.
		if !fatalExpectsWireTurnComplete(evt.Meta) {
			if err := r.synthesizeTruncatedTurnComplete(evt.ThreadID, now); err != nil {
				return err
			}
		}
	}

	return nil
}

// apiErrorEnum extracts the Claude `assistant.error` enum string from
// an EventError's Meta. The enum is the wire-typed signal that this
// error came from the SDK's documented closed set (rate_limit,
// authentication_failed, billing_error, ...) and warrants the
// `api_error` row kind. An empty return means the error came from a
// generic source (read-loop EOF, Codex willRetry:false) and should
// persist as the catch-all `error` kind.
func apiErrorEnum(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	enum, _ := m["error"].(string)
	return strings.TrimSpace(enum)
}

// fatalExpectsWireTurnComplete reports whether a fatal error carries
// the opt-in `expect_turn_complete: true` flag, signalling that the
// provider process is still alive and a real TurnComplete will follow.
// When absent (the common case — subprocess exit, stream EOF), the
// router synthesizes a TurnComplete{truncated:true} so the frontend
// working indicator flips off without needing the wire event.
func fatalExpectsWireTurnComplete(meta json.RawMessage) bool {
	if len(meta) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil {
		return false
	}
	v, _ := m["expect_turn_complete"].(bool)
	return v
}

// synthesizeTruncatedTurnComplete emits a synthetic
// EventTurnComplete{truncated:true} onto the routing pipeline so the
// frontend observes the turn's termination even when no wire
// TurnComplete arrives (subprocess exit, stream EOF). The synthetic
// event reuses the turn-complete handler's idempotent plumbing — it
// closes the span, clears the open turn, and emits any final
// thread-updated the UI needs to settle state. The handler reads the
// turn it should close from the router's openTurns map, so callers do
// not pass a turnIndex; if no turn is open the handler is a no-op.
//
// Dispatched through the handler directly (not through Handle) to
// avoid re-entering routing-layer guards such as the stopped-thread
// marker check — the fatal path has already committed to closing the
// turn. The test-only event hook is fired manually so synthesis
// observers still see the event.
func (r *Router) synthesizeTruncatedTurnComplete(threadID string, now int64) error {
	synth := provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Meta:      json.RawMessage(`{"truncated":true,"synthetic":true}`),
		Timestamp: time.UnixMilli(now),
	}
	err := r.handleTurnComplete(synth)
	r.fireEventHook(synth)
	return err
}

// emitInline is a no-op kept as an explicit marker at handler exit for
// events that triage routes through typed channels instead of a generic
// provider-event passthrough. The router exposes an eventHook observer (see
// SetEventHook) for tests that need to synchronize on the routing
// pipeline; wire-level passthroughs are intentionally absent so the
// frontend can rely on a single typed contract per channel.
func (r *Router) emitInline(evt provider.ProviderEvent) error {
	_ = evt
	return nil
}

// handleSubagentNotification is the pass-through router for Codex
// `<subagent_notification>` tags. Emission is triage's job; persistence
// is deliberately NOT — the notification isn't a timeline row today
// (tray/subagent UI will decide what to render later). The handler only
// forwards the Meta payload (agent_path, status, optional extras) onto
// `provider:subagent_notification` so the frontend can opt in without
// the parser-side emission needing to land first.
//
// See docs/architecture/turn-lifecycle.md and
// docs/archive/turn-lifecycle-refactor-plan.md WT-codex-parser for the
// emission-side plan.
func (r *Router) handleSubagentNotification(evt provider.ProviderEvent) error {
	// Forward to the frontend channel first so UI observers don't
	// depend on the projector's synthesis having run yet. The
	// projector checks whether any backgrounded spawn_agent rows are
	// waiting on the just-closed child; if so, the sibling completion
	// row is synthesized at the current tail (via maybeDeferOrPersist,
	// same stream-safety as the unifiedExec path).
	r.emit("provider:subagent_notification", SubagentNotificationEvent{
		ThreadID: evt.ThreadID,
		Meta:     evt.Meta,
	})
	return r.observeCodexSubagentNotification(evt)
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
	return r.persistItemWithEmit(item, payload, true)
}

func (r *Router) persistItemQuiet(item store.Item, payload *store.Payload) error {
	return r.persistItemWithEmit(item, payload, false)
}

func (r *Router) persistItemWithEmit(item store.Item, payload *store.Payload, emit bool) error {
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
	if emit {
		r.emitItemUpsert(persisted)
	}
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
