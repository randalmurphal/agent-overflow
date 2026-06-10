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

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

	"github.com/google/uuid"
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
	store                   *store.Store
	emit                    func(eventName string, data any) // wraps app.Event.Emit
	tracer                  trace.Tracer
	metrics                 TurnMetrics
	mu                      sync.Mutex
	pendingCommandDiffs     map[string]pendingCommandInlineDiff
	pendingApprovals        map[string]pendingApprovalState
	pendingApprovalOrder    map[string][]string
	pendingApprovalItems    map[string]string
	pendingUserInputs       map[string]provider.UserInputRequest
	pendingUserInputOrder   map[string][]string
	interruptQueue          map[string][]queuedPersistence
	openTurns               map[string]int
	segmentIndexByScope     map[string]int
	blockIndexByScope       map[string]int
	activeTextBlocks        map[string]bool
	activeThinkingBlocks    map[string]bool
	activeTextBlockRefs     map[string]activeStreamBlock
	activeThinkingBlockRefs map[string]activeStreamBlock
	streamingItemCounts     map[string]int
	errorSeqByScope         map[string]int
	compactionSeqByScope    map[string]int
	notificationSeqByScope  map[string]int
	// streamPersistBuffers decouple the live UI stream from durable
	// history writes. Text/thinking deltas emit immediately on ordered
	// provider:item_event deltas, then flush to SQLite by interval, byte
	// threshold, or lifecycle boundary.
	streamPersistBuffers map[string]*streamPersistBuffer
	// settledTurns marks turns whose handleTurnComplete has already run
	// to completion (turns row UPDATE-d, streaming items settled). A
	// second EventTurnComplete for a settled turn is
	// the multi-result-per-turn wire pattern (Claude CLI synthesizes a
	// `type:"user"` envelope from a task_notification → second `result`
	// envelope) or the synthetic-truncate-then-real race; in either
	// case the second handler invocation is a persistence no-op so
	// the checkpoint isn't captured twice and the turns row isn't
	// re-stamped. Cleared by setOpenTurn (so a re-init can re-settle
	// the same turn) and CleanupThread (session teardown). Key =
	// threadID|turnIndex.
	//
	// Note: this gate operates at LOGICAL-TURN granularity. The
	// frontend-facing `provider:turn_completed` emission is gated
	// independently per WIRE ROUND via currentRoundByThread/takeOpenRound
	// below — so a multi-result-per-turn cascade emits one
	// turn_completed per `result` envelope while persistence stays at
	// one settle per logical turn.
	settledTurns map[string]bool
	// currentRoundByThread names the active wire-round for each thread.
	// Frontend `provider:turn_started` / `provider:turn_completed`
	// emissions are gated per round via this slot — handleTurnStart and
	// the re-round branch of handleInit allocate a fresh round id;
	// handleTurnComplete reads-and-clears it via takeOpenRound. A wire
	// round corresponds to one Claude `result` envelope (or one Codex
	// `turn/completed`); a logical agent-overflow turn can span multiple
	// rounds when Claude's CLI synthesizes a `type:"user"` envelope from
	// a task_notification and the model issues another response. The
	// per-round cadence is what drives the working indicator, Stop
	// button, and composer-block state — all of
	// which want "model is engaged right now" semantics rather than
	// "user-typed prompt is in flight." Key = threadID. Cleared by
	// takeOpenRound (every wire complete) and CleanupThread.
	currentRoundByThread map[string]ActiveTurnSnapshot
	// latestTodoByThread carries the latest live todo/update_plan snapshot
	// per thread for frontend refresh / reconnect. Todo state is session
	// state, not history; this map is the backend-owned live projection.
	latestTodoByThread map[string]LiveTodoSnapshot
	// tasksByThread is the per-thread mirror of the Claude Task*
	// family task list. Survives any number of Parser recreations
	// within the process lifetime so a TaskUpdate against an id
	// created before session resume still routes correctly. Cleared
	// by CleanupThread and ResetThreadForRollback. Bounded by
	// maxTasksPerThread on insert (cap-and-reject).
	tasksByThread map[string]*threadTasks
	// turnSpans holds the active span for each in-flight turn so we can
	// close it when the matching EventTurnComplete arrives. Keyed by
	// threadID since the provider treats each thread as its own turn
	// stream.
	turnSpans map[string]trace.Span
	// stoppedThreads remembers thread IDs that CleanupThread has
	// explicitly stopped. While the flag is set, Handle drops events
	// that would persist to the store so late-arriving readLoop lines
	// from the torn-down subprocess do not leave orphan rows on the
	// stopped thread (Bug B5). Cleared ONLY by the host's session-start
	// path via MarkThreadActive — never by a wire event. A session that
	// dies before emitting anything recognizable (e.g. Claude failing
	// its --resume-session-at validation pre-init) must still have its
	// error results routed, so the host declares the thread active when
	// it commits to a replacement session rather than waiting for proof
	// of life from the wire. Host-synthesized events bypass the flag via
	// HandleSynthetic.
	stoppedThreads map[string]struct{}
	// threadEpochs counts MarkThreadActive calls per thread. An
	// asynchronous teardown captures the epoch before unregistering a
	// dead session and hands it to CleanupThreadIfEpoch, which no-ops
	// when the epoch has moved — i.e. the host committed to a
	// replacement session while the teardown goroutine was still in
	// flight. Entries are never deleted (a delete would reset the
	// counter to 0 and let a stale captured 0 match); growth is bounded
	// by the number of distinct threads that start a session in this
	// process's lifetime.
	threadEpochs map[string]uint64
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
	// terminalInteractionSeq counts terminal interaction carriers per
	// (thread, turn, processID). Empty wait polls may reuse the latest
	// carrier for that process; forwarded-stdin interactions always take
	// the next id. Bounded the same way other id-allocating counters are:
	// retained across clearOpenTurn to avoid multi-result id collisions and
	// swept on CleanupThread / selective turn re-init reset. See
	// terminal_interaction.go for the handler.
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
	// pendingToolPaths stages workspace paths from mutating tool starts until
	// the matching successful completion arrives. Keyed by
	// `<threadID>|<itemID>`. Failed/denied tools drop their staged paths so a
	// later conversation+files revert does not restore files the agent never
	// successfully changed.
	pendingToolPaths map[string][]string
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
	// settleWG tracks every fire-and-forget streaming-settle goroutine in
	// flight. Block-stop and stream-item settle hot paths spawn settle
	// work on its own goroutine so the provider read-loop doesn't pay
	// the SQLite-write latency mid-turn (the freeze hot path between a
	// thinking block ending and the next agent output streaming in).
	// settleTurnStreaming uses a per-turn local WaitGroup AND this one,
	// so the turn-row commit still sequences after every streaming-item
	// commit on the same logical turn. The router blocks shutdown on
	// this counter (see WaitForPendingSettles) so SQLite isn't closed
	// underneath an in-flight settle.
	settleWG sync.WaitGroup
	// pendingByThread is the FIFO of AO-initiated user sends awaiting
	// wire confirmation, keyed by threadID. Triage's send path appends
	// an entry when it dispatches a user message; the matching wire
	// EventUserText pops the head. Bounded by user attention (typically
	// 0-1 entries per thread). Lifecycle: user-send-time carry-over —
	// swept at CleanupThread as a safety net. See pending_send.go.
	pendingByThread map[string][]pendingSend
	// wireOnlyUserTextSeen dedupes wire EventUserText events that don't
	// match any pending AO send (the "agent prompted itself" or
	// session-resume replay case). Outer key = threadID; inner set =
	// providerItemIDs we've already observed. Cleared by CleanupThread.
	wireOnlyUserTextSeen map[string]map[string]struct{}
	// queuedFlushItems is the per-thread "queued user message awaiting
	// provider boundary" state. Populated when the user types into the
	// composer mid-turn and submits; drained when no top-level
	// foreground tool or live background task remains. Lifecycle: spans
	// turn boundaries by design, so NOT swept by clearOpenTurn — only
	// by CleanupThread on session teardown. See flush_queue.go.
	queuedFlushItems map[string][]QueuedFlushItem
	// dispatchFlush is the app-layer callback invoked when the queue
	// drains. Wired via SetFlushDispatcher; nil disables dispatch. Triage
	// releases r.mu before invoking, and the callback must return quickly;
	// provider writes belong behind the app-layer async/FIFO dispatcher.
	// See flush_queue.go.
	dispatchFlush FlushDispatcher
	// deferredUserTextConfirmed is an app-layer callback invoked after a
	// deferred queued user_text row has been persisted from a provider
	// echo. Used for side effects that require the row to exist, such
	// as message checkpoint capture.
	deferredUserTextConfirmed func(threadID string, item store.Item)
	// workspacePathByThread is a small read-through cache for the
	// thread row's WorkspacePath, populated lazily by enrichPathRefs
	// (the only hot caller). A thread's workspace is set at create
	// time and effectively immutable, so the cache is safe without
	// invalidation beyond CleanupThread. Without it, every
	// assistant_text settle ran a SQLite GetThread JUST to read a
	// stable string — fine on its own, but adds up across the
	// 10-30 text blocks per heavy turn. Keyed by threadID.
	workspacePathByThread map[string]string
	// streamingPathRefsLast dedupes the live-stream pathRefs meta
	// emissions per streaming assistant_text row. Each flushed
	// summary delta re-runs the validator against the row's running
	// Summary and emits action:"meta" only when the resulting JSON
	// changes; the most-common case (typing forward through text
	// that has no new path-shaped tokens) short-circuits cheaply.
	// Keyed by streamPersistKey(threadID,itemID); cleared at
	// doSettleStreamingText, clearActiveStreamBlocksForTurnLocked,
	// CleanupThread, and ResetThreadForRollback so a torn-down
	// streaming row can't leak its last-seen hash into the next
	// turn or session.
	streamingPathRefsLast map[string]string
	// revertedTurns marks threads whose next provider:turn_completed
	// emission should carry RevertedUserMessage=true. Set by the App
	// layer's revert-on-interrupt path BEFORE it tears down the
	// session; consumed (read-and-clear) inside buildRoundCompletedEvent.
	// Defensively swept by clearOpenTurn and CleanupThread so a stale
	// flag never leaks into a future turn. See revert_marker.go.
	revertedTurns map[string]struct{}
	// usageEmitThrottles rate-limits provider:usage emissions to at most
	// one per usageEmitMinInterval per thread. The context-window meter
	// changes gradually during streaming; Claude can fire 10-50 token
	// usage events/second but the UI doesn't benefit from updates faster
	// than ~2/sec. The pending window is flushed on turn-complete and
	// CleanupThread so the final reading always reaches the frontend.
	// Keyed by threadID.
	usageEmitThrottles map[string]*usageEmitThrottle
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
		pendingApprovalOrder:       make(map[string][]string),
		pendingApprovalItems:       make(map[string]string),
		pendingUserInputs:          make(map[string]provider.UserInputRequest),
		pendingUserInputOrder:      make(map[string][]string),
		interruptQueue:             make(map[string][]queuedPersistence),
		openTurns:                  make(map[string]int),
		segmentIndexByScope:        make(map[string]int),
		blockIndexByScope:          make(map[string]int),
		activeTextBlocks:           make(map[string]bool),
		activeThinkingBlocks:       make(map[string]bool),
		activeTextBlockRefs:        make(map[string]activeStreamBlock),
		activeThinkingBlockRefs:    make(map[string]activeStreamBlock),
		streamingItemCounts:        make(map[string]int),
		errorSeqByScope:            make(map[string]int),
		compactionSeqByScope:       make(map[string]int),
		notificationSeqByScope:     make(map[string]int),
		streamPersistBuffers:       make(map[string]*streamPersistBuffer),
		settledTurns:               make(map[string]bool),
		currentRoundByThread:       make(map[string]ActiveTurnSnapshot),
		latestTodoByThread:         make(map[string]LiveTodoSnapshot),
		tasksByThread:              make(map[string]*threadTasks),
		turnSpans:                  make(map[string]trace.Span),
		stoppedThreads:             make(map[string]struct{}),
		threadEpochs:               make(map[string]uint64),
		unknownSessionStatusLogged: make(map[string]struct{}),
		codexBackground:            make(map[string]*codexBackgroundState),
		terminalInteractionSeq:     make(map[string]int),
		openAPIRetryRows:           make(map[string]bool),
		pendingToolPaths:           make(map[string][]string),
		pendingByThread:            make(map[string][]pendingSend),
		wireOnlyUserTextSeen:       make(map[string]map[string]struct{}),
		queuedFlushItems:           make(map[string][]QueuedFlushItem),
		workspacePathByThread:      make(map[string]string),
		streamingPathRefsLast:      make(map[string]string),
		revertedTurns:              make(map[string]struct{}),
		usageEmitThrottles:         make(map[string]*usageEmitThrottle),
	}
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

	if r.isThreadStopped(evt.ThreadID) {
		// Drop silently — including EventInit. The readLoop could still
		// be draining in-flight lines after StopSession returned;
		// persisting them under the stopped thread would pollute the
		// timeline (Bug B5). The flag is cleared exclusively by the
		// host's session-start path (MarkThreadActive), so a stale init
		// from a torn-down subprocess cannot re-admit its trailing
		// frames.
		log.Printf("triage: dropping %s event for stopped thread %s", evt.Kind, evt.ThreadID)
		return nil
	}
	return r.dispatch(evt)
}

// HandleSynthetic routes a host-synthesized event exactly like Handle
// but bypasses the stopped-thread gate. Host events are not stale wire
// frames from a torn-down subprocess (the gate's target), and several
// fire precisely while the thread is stopped — the send-failure settle
// in app_send.go and reconnect-failure errors via emitErrorToThread.
// Wire events from provider read loops must keep going through Handle.
func (r *Router) HandleSynthetic(evt provider.ProviderEvent) error {
	r.inflight.Add(1)
	defer r.inflight.Done()
	defer r.fireEventHook(evt)
	return r.dispatch(evt)
}

func (r *Router) dispatch(evt provider.ProviderEvent) error {
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
	case provider.EventTaskCreate:
		return r.handleTaskCreate(evt)
	case provider.EventTaskUpdate:
		return r.handleTaskUpdate(evt)
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
		if err := r.handleBackgroundTaskTerminal(evt); err != nil {
			return err
		}
		r.maybeFlushQueueAtBoundary(evt.ThreadID)
		return nil
	case provider.EventBackgroundTaskNotification:
		return r.handleBackgroundTaskNotification(evt)
	case provider.EventSubagentNotification:
		return r.handleSubagentNotification(evt)
	case provider.EventSubagentStatus:
		return r.handleSubagentStatus(evt)
	case provider.EventCodexExecResult:
		return r.handleCodexExecResult(evt)
	case provider.EventTerminalInteraction:
		return r.handleTerminalInteraction(evt)
	case provider.EventUserText:
		return r.handleUserText(evt)
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
	scope := eventParentID(evt)
	if scope == "" {
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
	if err := r.settleStreamingScope(evt.ThreadID, scope); err != nil {
		log.Printf("triage: settle streaming before %s: %v", boundary, err)
	}
}

func (r *Router) handleToolStart(evt provider.ProviderEvent) error {
	if isToolStartMetaUpdateOnly(evt.Meta) {
		return r.persistToolCallLaunch(evt)
	}
	r.settleStreamingBeforeTimelineBoundary(evt, "tool start")
	// Codex TUI does not flush a unified-exec wait streak just because an
	// unrelated top-level tool starts. Wait streaks flush on assistant
	// content, turn boundaries, terminal interactions, or matching command
	// completion.
	if r.observeCodexToolStart(evt) {
		r.stageToolPaths(evt)
		return r.emitInline(evt)
	}
	// Lifecycle row first so the file-change / command-mutation helpers
	// below find an existing item to attach their rich payload onto via
	// UpsertItem — otherwise they'd race to AppendItem with the same
	// evt.ItemID and trip the UNIQUE id constraint.
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
		if handled && err == nil {
			r.maybeFlushQueueAtBoundary(evt.ThreadID)
		}
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
	r.maybeFlushQueueAtBoundary(evt.ThreadID)
	return r.emitInline(evt)
}

// stageToolPaths records candidate paths from a mutating tool start. The paths
// are only made durable after the matching successful completion, which avoids
// restoring denied/failed edits on a later conversation+files revert.
func (r *Router) stageToolPaths(evt provider.ProviderEvent) {
	raw := extractToolPaths(evt)
	if len(raw) == 0 || evt.ItemID == "" {
		return
	}
	r.mu.Lock()
	r.pendingToolPaths[evt.ThreadID+"|"+evt.ItemID] = raw
	r.mu.Unlock()
}

func (r *Router) settleToolPaths(evt provider.ProviderEvent) {
	if evt.ItemID == "" {
		return
	}
	key := evt.ThreadID + "|" + evt.ItemID
	r.mu.Lock()
	raw := r.pendingToolPaths[key]
	delete(r.pendingToolPaths, key)
	r.mu.Unlock()
	if !toolCallSucceeded(evt) {
		return
	}
	if len(raw) == 0 && isCodexFileChangeItem(evt.ItemType) {
		raw = extractToolPaths(evt)
	}
	if len(raw) == 0 {
		return
	}
	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		log.Printf("triage: track tool paths load thread %s: %v", evt.ThreadID, err)
		turnIndex, _ := r.currentTurnIndex(evt.ThreadID)
		r.emit("checkpoint:error", map[string]any{
			"threadId":  evt.ThreadID,
			"turnIndex": turnIndex,
			"error":     err.Error(),
		})
		return
	}
	paths := normalizeWorkspaceRelativePaths(raw, thread.WorkspacePath)
	if len(paths) == 0 {
		return
	}
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		log.Printf("triage: track tool paths current turn thread=%s item=%s: %v", evt.ThreadID, evt.ItemID, err)
		r.emit("checkpoint:error", map[string]any{
			"threadId":  evt.ThreadID,
			"turnIndex": 0,
			"error":     err.Error(),
		})
		return
	}
	if err := r.store.UpsertTrackedFiles(evt.ThreadID, turnIndex, paths); err != nil {
		log.Printf("triage: track tool paths thread=%s item=%s: %v", evt.ThreadID, evt.ItemID, err)
		r.emit("checkpoint:error", map[string]any{
			"threadId":  evt.ThreadID,
			"turnIndex": turnIndex,
			"error":     err.Error(),
		})
	}
}

// handleInit reacts to a wire `system/init` envelope (Claude only — Codex
// has no equivalent). Three cases share this entry point:
//
//  1. **Fresh AO send** — the send path registered a pending-send marker
//     before writing to stdin, and the provider has just acknowledged the
//     session by emitting `system/init`. We route through handleTurnStart
//     to open round 1 of the logical turn, fire plan/comment acceptance,
//     and emit `provider:turn_started`. This is the wire-driven
//     replacement for the synthetic EventTurnStart that app_send.go used
//     to emit after a successful stdin write. The pending-send marker is
//     NOT consumed here — handleUserText pops it when the matching
//     replay envelope arrives.
//
//  2. **Cascade re-round** — Claude's CLI synthesizes a `type:"user"`
//     envelope from a task_notification and follows it with a fresh
//     `system/init` before the next `result`. The current logical turn
//     is already settled, so maybeEmitReRoundOnInit opens a new wire
//     round (setOpenRound only — never setOpenTurn, see invariant
//     "setOpenTurn does NOT fire from handleInit").
//
//  3. **Idle session attach** — the app reattaches to a thread on
//     startup or session resume with no AO send in flight. Both the
//     pending-send check and the settled-turn check fail, so this
//     handler is a no-op beyond the session_ref update.
//
// Pending-send takes precedence over the settled-turn check because an
// AO send launched during a cascade settle window is round 1 of a NEW
// logical turn (handleTurnStart territory), not round 2+ of the
// previous logical turn (re-round territory).
func (r *Router) handleInit(evt provider.ProviderEvent) error {
	if evt.Meta != nil {
		var info provider.SessionInfo
		if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
			if err := r.store.UpdateSessionRef(evt.ThreadID, info.SessionID); err != nil {
				log.Printf("triage: update session ref: %v", err)
			}
		}
	}

	if r.HasPendingSendForThread(evt.ThreadID) {
		return r.handleTurnStart(evt)
	}

	r.maybeEmitReRoundOnInit(evt)
	return nil
}

// maybeEmitReRoundOnInit is the `system.init` re-round entry point: it
// re-lights the working indicator when an EventInit arrives for a
// thread whose current logical turn is already settled (the
// multi-result cascade — see maybeReopenSettledRound for the full
// mechanism and the second, parent-content-resume entry point). Named
// so call sites and the area guides have a system.init-specific hook.
func (r *Router) maybeEmitReRoundOnInit(evt provider.ProviderEvent) {
	r.maybeReopenSettledRound(evt)
}

// maybeReopenSettledRound opens a fresh wire round (emitting
// `provider:turn_started`) when the current logical turn is already
// settled and no round is currently open. It is the shared mechanism
// behind both re-round entry points:
//
//   - system.init re-round (maybeEmitReRoundOnInit) — Claude's CLI
//     synthesizes a `type:"user"` envelope from a task_notification and
//     follows it with a fresh `system.init` before the next `result`.
//   - parent-content resume (handleContentBlockStart, parent-only) —
//     Claude 2.1.154+ splits one logical turn into multiple wire
//     messages, closing each segment with a parent `message_delta`
//     stop_reason (the soft round-close, parse_stream.go) and then
//     resuming the SAME turn with a fresh parent `message_start` and no
//     intervening `result`/`system.init`. The soft-close already fired
//     `provider:turn_completed` (clearing the working indicator + Stop
//     button); without this re-arm the indicator stays dead for the
//     rest of the turn even though the agent is actively streaming.
//     See invariants.md §27.
//
// Critically, this path does NOT call setOpenTurn — id-allocating
// counters (segmentIndexByScope, blockIndexByScope, errorSeqByScope,
// terminalInteractionSeq) MUST survive across the multi-result/segment
// boundary, otherwise text/think/error ids collide with rows already
// persisted under the same logical turn (see multi_result_test.go).
//
// Two guards keep this from over-firing:
//
//   - settled — handleTurnComplete has fired at least once for this
//     logical turn. This disambiguates re-round from "fresh session
//     attaching to a thread that has no in-flight logical turn" (a real
//     session start yields no emission) AND from ordinary mid-round
//     content (round 1 is not settled until its first complete).
//   - no open round — once re-opened, subsequent content blocks in the
//     same wire round are no-ops. Only the FIRST parent content after a
//     soft-close re-arms; everything until the next close rides the
//     open round. This is what stops the indicator from blinking on
//     every content block of a resumed segment.
//
// The no-open-round guard is also why this is safe against the
// legitimate local_agent-outlives soft-close (invariant 27): there the
// parent has genuinely stopped and only the SUBAGENT streams until the
// trailing `result`. handleContentBlockStart gates this call on
// parent_tool_use_id == "" so subagent content never re-arms the
// parent's round — the indicator correctly stays cleared through the
// subagent wait.
func (r *Router) maybeReopenSettledRound(evt provider.ProviderEvent) {
	// Already-open round: nothing to re-arm. Cheap check first so the
	// common in-round content_block_start path returns immediately
	// without touching settledTurns or the store.
	if r.openRoundID(evt.ThreadID) != "" {
		return
	}

	// Resolve the logical-turn index: prefer the in-memory open turn
	// (round 1 of the same logical turn opened by handleTurnStart but
	// since closed by clearOpenTurn at handleTurnComplete will leave
	// this empty); fall back to LastTurnIndex so a settled-turn check
	// against the persisted row id can still run.
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return
		}
		turnIndex = last
	}

	r.mu.Lock()
	settled := r.settledTurns[settledTurnKey(evt.ThreadID, turnIndex)]
	r.mu.Unlock()
	if !settled {
		return
	}

	startedAt := eventTimestampMillis(evt)
	snapshot := ActiveTurnSnapshot{
		ThreadID:  evt.ThreadID,
		TurnID:    uuid.NewString(),
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}
	r.setOpenRoundSnapshot(snapshot)
	r.emit("provider:turn_started", TurnStartedEvent(snapshot))
}

func (r *Router) handleThreadModelUpdate(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateModel(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread model: %w", err)
	}
	r.emitThreadPatch(evt.ThreadID, ThreadUpdateEvent{Model: &evt.Content})
	return nil
}

func (r *Router) handleThreadRename(evt provider.ProviderEvent) error {
	if evt.Content == "" {
		return nil
	}
	if err := r.store.UpdateTitle(evt.ThreadID, evt.Content); err != nil {
		return fmt.Errorf("update thread title: %w", err)
	}
	r.emitThreadPatch(evt.ThreadID, ThreadUpdateEvent{Title: &evt.Content})
	return nil
}

// handleTokenUsage accepts provider-normalized context-window snapshots only.
// Per-turn token/cost accounting lives on turn-complete metadata; summing those
// totals here would over-count multi-call turns and subagent work.
//
// Subagent-emitted token usage (ParentToolUseID set) is dropped — subagent
// context tracking is private to the subagent; surfacing it on the parent
// meter would conflate two unrelated context windows. Mirrors the Claude
// pattern in `internal/provider/claude/parse_assistant.go:appendContextUsageEvent`.
// The provider parser already filters child-thread tokenUsage notifications;
// this is defense-in-depth for any future regression in the classifier.
func (r *Router) handleTokenUsage(evt provider.ProviderEvent) error {
	if strings.TrimSpace(evt.ParentToolUseID) != "" {
		return nil
	}
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
	//   4. synthesize EventTurnComplete with TruncatedTurnCompleteMeta if no wire
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
	summary := stringsx.FirstNonEmptyTrimmed(evt.Content, "Provider error")
	if err := r.persistProviderErrorItem(evt.ThreadID, turnIndex, summary, evt.Meta, scope, eventParentID(evt), now); err != nil {
		return err
	}

	if fatal {
		if err := r.finishFatalProviderError(evt.ThreadID, now, summary, evt.Meta); err != nil {
			return err
		}
	}

	return nil
}

// persistProviderErrorItem builds and persists a system error item
// through the shared per-turn error-id counter and the same
// already-persisted dedup handleError applies, so error rows from any
// source — wire EventError, orphan error results in handleTurnComplete
// — slot together without id collisions or duplicate rows.
//
// `assistant.error` from Claude carries the SDK enum on `meta.error`
// (rate_limit, authentication_failed, ...). Those persist as
// `api_error` kind so the frontend can render the actionable copy /
// link branch by enum (Add credits, Run /login, ...). Generic provider
// errors stay as the `error` kind. scope/parentID attribute subagent
// errors; pass "" for thread-level ones.
func (r *Router) persistProviderErrorItem(threadID string, turnIndex int, summary string, meta json.RawMessage, scope, parentID string, now int64) error {
	// Provider error strings are unbounded boundary input (wire
	// ErrorMessage / EventError content can carry whole stack traces);
	// items.summary is a preview surface shipped with every thread
	// emit. Clamp before the dedup probe so a re-fired long error
	// compares equal to its already-clamped row.
	summary = clampErrorSummary(summary)
	itemKind := "error"
	itemMeta := ""
	if enum := apiErrorEnum(meta); enum != "" {
		itemKind = itemKindAPIError
		itemMeta = string(meta)
	}
	if r.hasMatchingErrorItem(threadID, turnIndex, itemKind, summary, parentID) {
		return nil
	}
	errorItem := store.Item{
		ID:        nextErrorID(turnIndex, scope, r.nextErrorSequence(threadID, turnIndex, scope)),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      itemKind,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   summary,
		Meta:      itemMeta,
		ParentID:  parentID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(errorItem, nil)
}

// maxErrorSummaryRunes bounds persisted error summaries. 1000 runes
// keeps any realistic CLI diagnostic intact while capping pathological
// payloads (full stack traces, embedded request bodies) that would
// otherwise ride along on every thread-list emit.
const maxErrorSummaryRunes = 1000

// clampErrorSummary truncates at a rune boundary with an ellipsis so a
// multi-byte character is never split mid-sequence. Byte length bounds
// rune count, so the common short summary returns without allocating.
func clampErrorSummary(summary string) string {
	if len(summary) <= maxErrorSummaryRunes {
		return summary
	}
	runes := []rune(summary)
	if len(runes) <= maxErrorSummaryRunes {
		return summary
	}
	return string(runes[:maxErrorSummaryRunes-1]) + "…"
}

func (r *Router) finishFatalProviderError(threadID string, now int64, summary string, meta json.RawMessage) error {
	if err := r.drainInterruptQueue(threadID, true); err != nil {
		return err
	}
	r.clearOpenTurn(threadID)
	r.closeTurnSpan(threadID, errors.New(summary))

	// Synthesize a truncated TurnComplete only when no wire
	// TurnComplete is expected downstream. `meta.expect_turn_complete`
	// opts in to "the subprocess is still alive, a real wire
	// TurnComplete will still arrive" — the common case for a fatal
	// EventError that represents a mid-turn refusal. Absent that
	// opt-in we assume the subprocess exited (stdout EOF, crash)
	// and emit the synthetic TurnComplete so the frontend working
	// indicator flips off even without a wire event.
	if !fatalExpectsWireTurnComplete(meta) {
		if err := r.synthesizeTruncatedTurnComplete(threadID, now); err != nil {
			return err
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
	enum, _ := m["api_error_enum"].(string)
	if enum = strings.TrimSpace(enum); enum != "" {
		return enum
	}

	enum, _ = m["error"].(string)
	enum = strings.TrimSpace(enum)
	if enum == "" || !isLegacyAssistantErrorMeta(m, enum) {
		return ""
	}
	return enum
}

func isLegacyAssistantErrorMeta(meta map[string]any, enum string) bool {
	// Backwards-compatible with older live Claude assistant.error events:
	// they carried the enum in meta.error and were tagged as expecting a
	// real result envelope. Codex fatal notifications also used
	// meta.error for raw human text, but do not carry this opt-in.
	expectTurnComplete, _ := meta["expect_turn_complete"].(bool)
	return expectTurnComplete && isLegacyAssistantErrorEnum(enum)
}

func isLegacyAssistantErrorEnum(enum string) bool {
	switch enum {
	case "authentication_failed",
		"billing_error",
		"rate_limit",
		"invalid_request",
		"server_error",
		"unknown",
		"max_output_tokens":
		return true
	default:
		return false
	}
}

func (r *Router) hasMatchingErrorItem(threadID string, turnIndex int, kind string, summary string, parentID string) bool {
	found, err := r.store.HasMatchingSystemItem(threadID, turnIndex, kind, parentID, summary)
	if err != nil {
		log.Printf("triage: matching error row duplicate check: %v", err)
		return false
	}
	return found
}

// fatalExpectsWireTurnComplete reports whether a fatal error carries
// the opt-in `expect_turn_complete: true` flag, signalling that the
// provider process is still alive and a real TurnComplete will follow.
// When absent (the common case — subprocess exit, stream EOF), the
// router synthesizes a TurnComplete with TruncatedTurnCompleteMeta so the frontend
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
// EventTurnComplete with TruncatedTurnCompleteMeta onto the routing pipeline so the
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
		Kind:         provider.EventTurnComplete,
		ThreadID:     threadID,
		TurnComplete: &provider.TruncatedTurnCompleteMeta{Synthetic: true},
		Timestamp:    time.UnixMilli(now),
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

func (r *Router) handleSubagentStatus(evt provider.ProviderEvent) error {
	return r.observeCodexSubagentStatus(evt)
}

// ThreadUpdateEvent is the wire shape for thread:updated. Action "full"
// carries the entire Thread struct; "patch" carries only the changed fields.
type ThreadUpdateEvent struct {
	Action string        `json:"action"`
	Thread *store.Thread `json:"thread,omitempty"`
	ID     string        `json:"id,omitempty"`
	Title  *string       `json:"title,omitempty"`
	Model  *string       `json:"model,omitempty"`
}

func (r *Router) emitThreadPatch(threadID string, patch ThreadUpdateEvent) {
	patch.Action = "patch"
	patch.ID = threadID
	r.emit("thread:updated", patch)
}

// bumpThreadActivity is the single chokepoint for advancing
// threads.updated_at at the three sidebar-bump boundaries (user_text
// persist, turn settle, approval / user-input request). Logs and
// continues on error — the underlying turn/approval/persist write
// already succeeded; sidebar ordering is best-effort. The nil-store
// short-circuit supports tests that construct a Router with no store
// (e.g. interactive_requests_test.go).
func (r *Router) bumpThreadActivity(threadID string, at int64, reason string) {
	if r.store == nil || threadID == "" {
		return
	}
	if err := r.store.MarkThreadActivity(threadID, at); err != nil {
		log.Printf("triage: mark thread activity on %s for %s: %v", reason, threadID, err)
	}
}

func userTextCountsAsThreadActivity(item store.Item) bool {
	if item.Kind != itemKindUserText {
		return false
	}
	if item.ParentID != "" {
		return false
	}
	if strings.TrimSpace(item.Meta) == "" {
		return true
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		return true
	}
	if wireOnly, ok := meta["wire_only"].(bool); ok && wireOnly {
		return false
	}
	return true
}

func (r *Router) persistItem(item store.Item, payload *store.Payload) error {
	_, err := r.persistItemWithEmit(item, payload, nil, true)
	return err
}

func (r *Router) persistItemQuiet(item store.Item, payload *store.Payload) error {
	_, err := r.persistItemWithEmit(item, payload, nil, false)
	return err
}

// persistItemQuietReturning persists without emitting and returns the stored
// row, which carries store-assigned fields (notably item_index from
// nextItemIndexTx). Callers that need to emit a tailored upsert — e.g. a
// streaming row whose first chunk ships as a delta rather than being baked
// into the creation summary — must emit THIS, not the hand-built pre-persist
// struct, or the emitted row would carry item_index 0 and mis-sort.
func (r *Router) persistItemQuietReturning(item store.Item, payload *store.Payload) (store.Item, error) {
	return r.persistItemWithEmit(item, payload, nil, false)
}

// persistItemWithInputPayload is the two-payload variant of persistItem used
// by the tool-call lifecycle when applyToolMetaRule has promoted heavy
// inputs out of items.meta into a sibling tool_call_input payload row.
// resultPayload and inputPayload may each be nil independently.
func (r *Router) persistItemWithInputPayload(item store.Item, resultPayload, inputPayload *store.Payload) error {
	_, err := r.persistItemWithEmit(item, resultPayload, inputPayload, true)
	return err
}

func (r *Router) persistItemWithEmit(item store.Item, payload *store.Payload, inputPayload *store.Payload, emit bool) (store.Item, error) {
	countsAsActivity := userTextCountsAsThreadActivity(item)

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

	persisted, err := r.store.UpsertItemWithInputPayload(item, payload, inputPayload)
	if err != nil {
		return store.Item{}, err
	}
	// Bump sidebar activity only for user-authored user_text rows.
	// Provider-injected wire-only context and subagent-internal prompts
	// are timeline history, not activity that should reshuffle the
	// sidebar.
	if countsAsActivity {
		r.bumpThreadActivity(persisted.ThreadID, persisted.UpdatedAt, "user_text persist")
	}
	if emit {
		r.emitItemUpsertWithActivity(persisted, countsAsActivity)
	}
	r.metrics.ItemsPersisted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", persisted.Kind)))
	if payload != nil {
		r.metrics.PayloadsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", payload.Kind)))
	}
	if inputPayload != nil {
		r.metrics.PayloadsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", inputPayload.Kind)))
	}
	return persisted, nil
}

// emitItemPatch sends a lightweight patch event carrying only the fields
// that changed. The frontend merges the patch into the existing item in
// place, avoiding re-transmission of immutable structural fields and the
// potentially large summary text.
func (r *Router) emitItemPatch(threadID, itemID, kind string, patch ItemPatchFields) {
	r.emit("provider:item_event", newItemStreamPatch(threadID, itemID, kind, patch))
}

// persistItemFieldsAndPatch writes a targeted UPDATE for the specified
// fields and emits a patch event. Use instead of persistItem when the
// row already exists and only a narrow set of fields changed (e.g.,
// streaming settle: status + meta + updatedAt).
func (r *Router) persistItemFieldsAndPatch(threadID, itemID, kind string, update store.ItemPartialUpdate) error {
	if err := r.store.UpdateItemFields(threadID, itemID, update); err != nil {
		return err
	}
	r.emitItemPatch(threadID, itemID, kind, patchFromPartial(update))
	return nil
}

func patchFromPartial(u store.ItemPartialUpdate) ItemPatchFields {
	return ItemPatchFields{
		Status:    u.Status,
		Summary:   u.Summary,
		Meta:      u.Meta,
		Decision:  u.Decision,
		UpdatedAt: u.UpdatedAt,
	}
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

// PersistItemQuiet persists the item to the store without emitting
// provider:item_event. Used by the eager-persist flush path to reserve
// timeline position in SQLite while the frontend keeps showing the
// item as a Zone 2 queued marker until the provider echo confirms it.
func (r *Router) PersistItemQuiet(item store.Item, payload *store.Payload) error {
	return r.persistItemQuiet(item, payload)
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
