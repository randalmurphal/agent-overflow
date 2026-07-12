package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered. Prevents a second write landing
// at the provider with a stale decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("codex: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// ErrNoActiveTurn is returned by Steer when there is no active turn to
// steer into. The app layer treats this as a "fall back to Send" signal
// (race window: turn just ended between the frontend reading the
// active-turn registry and the steer RPC arriving here).
var ErrNoActiveTurn = errors.New("codex: no active turn to steer")

// DynamicToolHandler is called when the provider invokes a dynamic tool (item/tool/call
// or dynamicToolCall). The handler receives the tool name and arguments, and returns
// the result content and a success flag.
type DynamicToolHandler func(toolName string, args map[string]any) (content string, success bool, err error)

// Compile-time guarantee that *Session satisfies the provider.Session
// interface the app layer calls into. Changing any of the methods in a
// way that breaks the contract is caught at build time.
var _ provider.Session = (*Session)(nil)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc          *provider.Process
	ctx           context.Context
	threadID      string // our internal thread ID
	codexThreadID string // the Codex app-server's thread ID from thread/start
	activeTurnID  string // current active turn ID from turn/started; cleared on turn/completed
	// pendingTurnSchemaKnown/pendingTurnSchemaed bridge Send's per-turn
	// options to the turn ID supplied by the response or turn/started. The
	// maps retain only schemaed turns and their latest completed agentMessage;
	// turn/completed consumes both so state cannot leak across turns.
	pendingTurnSchemaKnown bool
	pendingTurnSchemaed    bool
	schemaedTurnIDs        map[string]struct{}
	structuredOutputByTurn map[string]json.RawMessage
	// The turn config block below is re-applied to every turn/start call, so
	// a mid-session change (ApplyLiveUpdate) takes effect on the next turn
	// without a process restart. All five are guarded by mu: Send copies
	// them per turn, ApplyLiveUpdate rewrites them, and the read loop reads
	// model for usage attribution.
	model              string // active model; also used for usage attribution
	reasoningEffort    string // per-turn reasoning effort override; empty means inherit thread default
	serviceTier        string // per-turn service tier override; "priority" enables Codex fast mode
	approvalPolicy     string // per-turn approval override; empty means inherit thread default
	sandbox            string // per-turn sandbox override; empty means inherit thread default
	nextID             atomic.Int64
	mu                 sync.Mutex
	pending            map[int64]chan json.RawMessage
	onEvent            func(provider.ProviderEvent)
	dynamicToolHandler DynamicToolHandler
	cancel             context.CancelFunc
	closing            atomic.Bool
	readDone           chan struct{}
	// approvalsMu guards pendingApprovals, resolvedApprovals, and
	// approvalsClosed.
	approvalsMu sync.Mutex
	// pendingApprovals maps request ID (string form, matching the
	// RequestID field of ApprovalResponse) to the in-flight request
	// metadata needed to resolve, cancel, or drain it.
	pendingApprovals map[string]*pendingApproval
	// approvalDedup tracks request IDs already answered so a second
	// RespondToApproval returns ErrApprovalAlreadyResolved (Bug B9)
	// rather than silently writing another response to the provider.
	// Guarded by approvalsMu.
	approvalDedup provider.ApprovalDeduper
	// approvalsClosed is set by Close so late-arriving approvals don't
	// register new pending requests after teardown.
	approvalsClosed bool
	// seenTurnStarts dedupes EventTurnStart emissions (Bug B6). Keyed by
	// turnID. Entries are added by claimTurnStart and cleared by
	// clearTurnStart on EventTurnComplete so re-used turn IDs (rare,
	// typically across resumed sessions) can fire fresh.
	seenTurnStarts map[string]struct{}
	// usageAcct derives per-turn token usage from the cumulative
	// thread/tokenUsage/updated totals (see usage_accounting.go).
	// Read-loop goroutine only — observed in dispatchNotification and
	// settled in updateNotificationState; no lock.
	usageAcct usageAccounting
	// requestTimeoutOverride replaces defaultRequestTimeout when
	// non-zero. Set by tests that exercise the late-response path; a
	// production Session leaves it at zero to use the default.
	requestTimeoutOverride time.Duration
	// childParentByThread maps a spawned collab receiver thread back to the
	// parent SpawnAgent tool-call item id. Raw spawn_agent output can also
	// return an `agent_id` before the typed child-thread metadata arrives;
	// current Codex completion notifications may echo that same value as
	// `agent_path`, so it is indexed here as a resolver alias too.
	// Notifications from the child thread are re-emitted onto the parent
	// thread with ParentToolUseID set to this card id.
	//
	// childParentByAgentPath maps Codex's subagent_notification
	// `agent_path` value back to the same parent card. Named Codex agents
	// report a path such as `/root/researcher`, not the receiver thread id,
	// so the detached-completion path cannot rely on receiverThreadIds
	// alone.
	//
	// agentPathByThread is the inverse index used to delete path mappings
	// when a close_agent event closes a receiver thread.
	//
	// No heuristic background-tool classifier runs here (invariant 25).
	// The wire-typed signals for a backgrounded item are
	// `CommandExecution.source == "unifiedExecStartup"` and
	// `collabAgentToolCall` with a running child in `agentsStates`; those
	// are what authorize `is_background=true`. The actual stamp happens in
	// `internal/triage/codex_background.go` on the first model-produced
	// yield or at the turn-close catchall — this session package only
	// surfaces the wire fields; it doesn't project them.
	childParentByThread       map[string]string
	childParentByAgentPath    map[string]string
	agentPathByThread         map[string]string
	agentMetaByThread         map[string]collabReceiverMeta
	subagentNotificationDedup map[subagentNotificationDedupKey]struct{}
	rolloutObserverWG         sync.WaitGroup
	collabMetadataReads       chan struct{}
	rawToolCallsByID          map[string]rawToolCall
	waitReceiverIDsByCall     map[string][]string
	// deferredChildWireEvents quarantines notifications and server requests
	// from provider threads whose spawn ownership has not arrived yet. Codex
	// MultiAgentV2 starts the child before it emits the parent's canonical
	// subAgentActivity item, so child output can legitimately win that race.
	// Nothing in this queue may reach the AO parent projection until a typed
	// ownership edge maps the provider thread to its spawn card.
	deferredChildWireEvents map[string][]deferredChildWireEvent
	deferredChildWireCount  int
	deferredChildWireBytes  int
	deferredChildDeadlines  map[string]*time.Timer
	childRoutingWarned      bool
	// Collaboration metadata enrichment and reopen-history traversal run in
	// session-scoped background work. collabAsyncMu closes the Add/Wait race;
	// collabHistory* is guarded by mu and feeds one sequential traversal.
	collabAsyncMu                 sync.Mutex
	collabAsyncClosing            bool
	collabAsyncWG                 sync.WaitGroup
	collabHistoryQueue            []collabHistoryJob
	collabHistoryRunning          bool
	collabHistoryGeneration       uint64
	collabHistoryVisited          map[string]uint64
	collabHistoryAttempts         int
	collabHistoryWarnedGeneration uint64
	planBuffersByItemID           map[string]*planBuffer
	planBuffersByTurnID           map[string]*planBuffer
	// probeFn is a test-only override for Probe(). When non-nil, Probe
	// skips the wire call and returns the result from this function.
	// Production Session construction (NewSession) never sets it.
	probeFn func(ctx context.Context) (ProbeResult, error)
	// resumeFn mirrors probeFn for Resume(). Used by
	// app_codex_reconcile_test.go to verify the post-probe rehydration
	// path without needing a live app-server. Production NewSession
	// never sets it.
	resumeFn func(ctx context.Context) error
	// cleanBackgroundTerminalsFn mirrors probeFn for
	// CleanBackgroundTerminals(). Used by app_codex_background_test.go to
	// verify the binding wires through to a Codex session without
	// spinning up a real app-server. Production NewSession never sets it.
	cleanBackgroundTerminalsFn func(ctx context.Context) error
	// mcpOAuthCompletedHandler fires when Codex emits an
	// `mcpServer/oauthLogin/completed` notification after the user
	// finishes the browser hop on a previously-issued
	// `mcpServer/oauth/login` request. The App layer uses it to
	// invalidate the MCP probe cache so the next status read reflects
	// the freshly authenticated state instead of a cached
	// `needs-auth`. Guarded by mu; nil when no observer is registered
	// (the only production registrant is app_session.go and tests).
	mcpOAuthCompletedHandler MCPOAuthCompletedHandler
	// mcpStartupUpdateHandler fires when Codex emits an
	// `mcpServer/startupStatus/updated` notification — a per-server
	// state delta during/after thread/start (status ∈ starting | ready |
	// failed | cancelled). The App layer feeds these into the mcpstatus
	// cache so the popup reflects the running provider's view without
	// a refetch. Per-thread-only emission — there's no app-server-wide
	// stream. Guarded by mu; nil when no observer is registered.
	mcpStartupUpdateHandler MCPStartupUpdateHandler
}

// MCPOAuthCompletedHandler observes the wire-level completion of an
// MCP server OAuth flow on a live Codex session. ServerName matches
// the AO library row's `name`; success/error reflect Codex's payload
// without further normalisation so the App can decide whether to
// surface the failure to the thread.
type MCPOAuthCompletedHandler func(serverName string, success bool, errorMessage string)

// MCPStartupUpdate is the normalized payload carried by Codex's
// `mcpServer/startupStatus/updated` notification. State values
// per wire: "starting" | "ready" | "failed" | "cancelled".
type MCPStartupUpdate struct {
	Name  string
	State string
	Error string
}

// MCPStartupUpdateHandler observes `mcpServer/startupStatus/updated`
// notifications. Registered by app_session.go after NewSession
// returns — Codex only emits these post-thread/start, so registering
// after the handshake is safe.
type MCPStartupUpdateHandler func(update MCPStartupUpdate)

// Config for creating a Codex session.
type Config struct {
	Binary                string // default: "codex"
	Model                 string
	WorkDir               string
	ApprovalPolicy        string // "never", "on-failure", "on-request", "untrusted"
	Sandbox               string // "read-only", "workspace-write", "danger-full-access"
	ResumeThreadID        string // thread ID to resume, empty for new
	SystemPrompt          string
	MCPServers            map[string]any
	ContextWindow         int
	AutoCompactTokenLimit int
	// ReasoningEffort is the Codex-native reasoning_effort enum value exposed
	// by the selected model. Applied to the thread start
	// handshake under `config.model_reasoning_effort`, and re-applied to
	// every turn/start call under the `effort` parameter so per-thread
	// tuning takes effect without a session restart.
	ReasoningEffort string
	// ServiceTier is Codex's native speed tier. "priority" is sent as
	// serviceTier on thread/start|resume and turn/start. It must not rewrite
	// Model; fast mode does not change the selected model.
	ServiceTier string
	// Env carries per-session environment variables for the spawned
	// app-server process, merged over the inherited environment. The
	// agent test harness scopes its mock-provider control credentials
	// to provider spawns through this instead of the process env.
	Env         map[string]string
	EventLogger *logging.Logger
}

// NewSession spawns codex app-server, performs the initialize handshake,
// and starts (or resumes) a thread. Returns after handshake completes.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary:           binary,
		Args:             []string{"app-server"},
		Dir:              cfg.WorkDir,
		Env:              cfg.Env,
		EventLogger:      cfg.EventLogger,
		EventLogRedactor: newCodexProviderEventLogRedactor(),
		ThreadID:         threadID,
		Provider:         string(provider.Codex),
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex: spawn: %w", err)
	}

	s := &Session{
		proc:                      proc,
		ctx:                       childCtx,
		threadID:                  threadID,
		model:                     cfg.Model,
		usageAcct:                 newUsageAccounting(cfg.ResumeThreadID != ""),
		reasoningEffort:           cfg.ReasoningEffort,
		serviceTier:               cfg.ServiceTier,
		approvalPolicy:            cfg.ApprovalPolicy,
		sandbox:                   cfg.Sandbox,
		pending:                   make(map[int64]chan json.RawMessage),
		onEvent:                   onEvent,
		cancel:                    cancel,
		readDone:                  make(chan struct{}),
		childParentByThread:       make(map[string]string),
		childParentByAgentPath:    make(map[string]string),
		agentPathByThread:         make(map[string]string),
		agentMetaByThread:         make(map[string]collabReceiverMeta),
		subagentNotificationDedup: make(map[subagentNotificationDedupKey]struct{}),
		collabMetadataReads:       make(chan struct{}, 4),
		rawToolCallsByID:          make(map[string]rawToolCall),
		waitReceiverIDsByCall:     make(map[string][]string),
		deferredChildWireEvents:   make(map[string][]deferredChildWireEvent),
		deferredChildDeadlines:    make(map[string]*time.Timer),
		collabHistoryGeneration:   1,
		collabHistoryVisited:      make(map[string]uint64),
		planBuffersByItemID:       make(map[string]*planBuffer),
		planBuffersByTurnID:       make(map[string]*planBuffer),
	}
	// On resume the root provider id is already durable in AO. Seed it before
	// the read loop starts so child notifications racing the thread/resume
	// response are quarantined instead of being mistaken for root events. A
	// fresh thread cannot have children before NewSession returns.
	if cfg.ResumeThreadID != "" {
		s.codexThreadID = cfg.ResumeThreadID
	}

	// Start stdout reader goroutine before sending any requests.
	go s.readLoop()

	// Initialize handshake.
	_, err = s.sendRequest(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agent_overflow",
			"title":   "Agent Overflow",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: initialize handshake failed: %w", err)
	}

	// Send initialized notification (no id, no response expected).
	if err := s.writeNotification("initialized", nil); err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: send initialized notification: %w", err)
	}

	// Start or resume thread.
	threadParams := buildThreadParams(cfg)
	var method string
	if cfg.ResumeThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = cfg.ResumeThreadID
	} else {
		method = "thread/start"
		threadParams["experimentalRawEvents"] = true
	}

	resp, err := s.sendRequest(ctx, method, threadParams)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: %s failed: %w", method, err)
	}

	// Extract the Codex thread ID from response.
	s.threadID = threadID // our internal ID
	responseThreadID := readNestedString(resp, "thread", "id")
	if responseThreadID == "" {
		log.Printf("codex: %s response missing thread.id; response: %s", method, string(resp))
		s.Close()
		return nil, fmt.Errorf("codex: %s: response did not contain a thread ID", method)
	}
	if s.codexThreadID != "" && s.codexThreadID != responseThreadID {
		s.Close()
		return nil, fmt.Errorf("codex: %s: response thread ID %q does not match requested thread %q", method, responseThreadID, s.codexThreadID)
	}
	s.codexThreadID = responseThreadID
	if method == "thread/resume" {
		s.rehydrateCollabOwnershipFromThreadResponse(resp)
		s.startRolloutSubagentNotificationObserver(readNestedString(resp, "thread", "path"))
	}

	meta, _ := json.Marshal(provider.SessionInfo{
		SessionID: s.codexThreadID,
		Model:     cfg.Model,
		CWD:       cfg.WorkDir,
	})
	s.onEvent(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})

	return s, nil
}

// Send sends a user turn via turn/start.
//
// The JSON-RPC response does not directly drive an EventTurnStart: the
// app-server reliably follows turn/start with a turn/started notification
// that ClassifyNotification turns into the event. Emitting here as well
// produced two EventTurnStart per user send (Bug B6). We still record the
// turn ID locally so Interrupt has something to cancel even if the
// notification has not yet arrived.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	input, err := buildTurnInput(content, opts.Attachments)
	if err != nil {
		return err
	}

	cfg := s.turnConfig()
	params := map[string]any{
		"threadId":          s.codexThreadID,
		"input":             input,
		"collaborationMode": codexCollaborationMode(opts.InteractionMode, cfg.Model, cfg.ReasoningEffort),
	}
	if len(opts.OutputSchema) > 0 {
		params["outputSchema"] = opts.OutputSchema
	}
	// Per-turn config overrides — Codex's TurnStartParams takes `model`,
	// `effort`, `serviceTier`, `approvalPolicy`, and `sandboxPolicy` at the
	// top level, each documented upstream as applying "for this turn and
	// subsequent turns". Threading them here (rather than only at
	// thread-start) means a mid-session change from the composer
	// (ApplyLiveUpdate) takes effect on the very next turn without a
	// session restart. Empty means "inherit the thread default set during
	// thread/start".
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.ReasoningEffort != "" {
		params["effort"] = cfg.ReasoningEffort
	}
	if cfg.ServiceTier != "" {
		params["serviceTier"] = cfg.ServiceTier
	}
	if cfg.ApprovalPolicy != "" {
		params["approvalPolicy"] = cfg.ApprovalPolicy
	}
	if cfg.Sandbox != "" {
		sandboxPolicy, err := turnSandboxPolicy(cfg.Sandbox)
		if err != nil {
			return err
		}
		params["sandboxPolicy"] = sandboxPolicy
	}

	s.setPendingTurnSchema(len(opts.OutputSchema) > 0)
	resp, err := s.sendRequest(ctx, "turn/start", params)
	if err != nil {
		s.clearPendingTurnSchema()
		return fmt.Errorf("codex: turn/start: %w", err)
	}

	turnID := readNestedString(resp, "turn", "id")
	if turnID != "" {
		s.bindPendingTurnSchema(turnID)
		s.mu.Lock()
		s.activeTurnID = turnID
		s.mu.Unlock()
	}

	return nil
}

func (s *Session) setPendingTurnSchema(schemaed bool) {
	s.mu.Lock()
	s.pendingTurnSchemaKnown = true
	s.pendingTurnSchemaed = schemaed
	s.mu.Unlock()
}

func (s *Session) clearPendingTurnSchema() {
	s.mu.Lock()
	s.pendingTurnSchemaKnown = false
	s.pendingTurnSchemaed = false
	s.mu.Unlock()
}

func (s *Session) bindPendingTurnSchema(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pendingTurnSchemaKnown {
		return
	}
	if s.pendingTurnSchemaed {
		if s.schemaedTurnIDs == nil {
			s.schemaedTurnIDs = make(map[string]struct{})
		}
		s.schemaedTurnIDs[turnID] = struct{}{}
	} else {
		delete(s.schemaedTurnIDs, turnID)
	}
	s.pendingTurnSchemaKnown = false
	s.pendingTurnSchemaed = false
}

// Steer injects user input into the currently-active turn's
// pending_input queue via Codex's `turn/steer` JSON-RPC. Mid-turn
// injection lets the user "steer" the model without spawning a new
// turn — Codex drains pending_input on the next iteration of its
// run_turn loop, and the app-server confirms the inject by emitting
// a wire-typed `item/completed userMessage` inside the same active
// turn (which our triage handleUserText path correlates with the
// pending-send marker).
//
// REQUIRES an active turn — returns ErrNoActiveTurn if no turn is
// currently in flight, so the caller can fall back to Send rather
// than racing the wire. The caller should also fall back when the
// app-server returns NoActiveTurn or ExpectedTurnMismatch (race
// window: turn ended or a new turn started between the frontend
// reading the active-turn registry and the steer RPC arriving here).
//
// DOES NOT take effort / approvalPolicy / sandboxPolicy /
// collaborationMode — those are turn-creation params for turn/start,
// not steer. Steer's contract is "inject input into an existing
// turn's input queue"; per-turn settings are fixed at the turn's
// creation.
//
// Wire shape per
// codex-rs/app-server-protocol/src/protocol/v2.rs:5192-5209
// (TurnSteerParams). Server-side reference:
// codex-rs/core/src/session/mod.rs:2983 (errors NoActiveTurn if no
// turn is running, ExpectedTurnMismatch if the turn id has rolled).
func (s *Session) Steer(ctx context.Context, content string, opts provider.SendOptions) error {
	s.mu.Lock()
	expectedTurnID := s.activeTurnID
	s.mu.Unlock()
	if expectedTurnID == "" {
		return ErrNoActiveTurn
	}

	input, err := buildTurnInput(content, opts.Attachments)
	if err != nil {
		return fmt.Errorf("codex: turn/steer: %w", err)
	}

	params := map[string]any{
		"threadId":       s.codexThreadID,
		"input":          input,
		"expectedTurnId": expectedTurnID,
	}

	if _, err := s.sendRequest(ctx, "turn/steer", params); err != nil {
		return fmt.Errorf("codex: turn/steer: %w", err)
	}
	return nil
}

// Interrupt sends turn/interrupt to abort whatever the thread is
// currently doing. We pass `turnId: activeTurnID` when a turn is in
// flight and `turnId: ""` (the empty string) when the user pressed
// stop during the dispatch window before `turn/started` arrived —
// the upstream app-server treats an empty turn_id as a "startup
// interrupt" and submits Op::Interrupt to the core anyway, then
// responds immediately with `{}` because startup cancellation has
// no TurnAborted event to wait on. See
// codex-rs/app-server/src/codex_message_processor.rs:7790-7849
// (`is_startup_interrupt = turn_id.is_empty()`) and the README
// summary at codex-rs/app-server/README.md:167.
//
// On success, drains pending approvals and user-input requests so
// the frontend clears its prompt panel and Codex's pending JSON-RPC
// requests resolve. Drain on failure too: the user pressed stop and
// expects the prompt to disappear even if the JSON-RPC errored.
// This is a deliberate fix beyond t3-code's
// CodexSessionRuntime.interruptTurn (CodexSessionRuntime.ts:1238–1250),
// which only sends the JSON-RPC and leaks the local Deferreds.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turnID := s.activeTurnID
	s.mu.Unlock()

	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"threadId": s.codexThreadID,
		"turnId":   turnID,
	})

	s.drainPendingApprovals("cancel", false, true)
	return err
}

// ThreadID returns our internal thread identifier.
func (s *Session) ThreadID() string {
	return s.threadID
}

// SetDynamicToolHandler registers a handler for dynamic tool calls (item/tool/call,
// dynamicToolCall). If nil, those requests are rejected with -32601.
func (s *Session) SetDynamicToolHandler(h DynamicToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dynamicToolHandler = h
}

// SetMCPOAuthCompletedHandler installs an observer for the
// `mcpServer/oauthLogin/completed` notification. Safe to register after
// NewSession: Codex only emits the notification after a successful
// `mcpServer/oauth/login` request, so the handler is in place before
// the wire signal can fire.
func (s *Session) SetMCPOAuthCompletedHandler(h MCPOAuthCompletedHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcpOAuthCompletedHandler = h
}

// SetMCPStartupUpdateHandler installs an observer for the
// `mcpServer/startupStatus/updated` notification. Codex emits these
// only post-thread/start, so registering immediately after NewSession
// is safe — the first update can't fire before the handler is in
// place.
func (s *Session) SetMCPStartupUpdateHandler(h MCPStartupUpdateHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcpStartupUpdateHandler = h
}

// PID returns the OS process id (and process-group id) of the Codex
// app-server subprocess, or 0 when no process is live.
func (s *Session) PID() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Close shuts down the app-server process.
// Closes stdin first for graceful shutdown, then cancels the context as fallback.
func (s *Session) Close() error {
	s.closing.Store(true)
	s.collabAsyncMu.Lock()
	s.collabAsyncClosing = true
	s.collabAsyncMu.Unlock()
	s.clearPendingApprovals()
	err := s.proc.Close()
	s.cancel()
	if s.readDone != nil {
		<-s.readDone
	}
	s.rolloutObserverWG.Wait()
	s.collabAsyncWG.Wait()
	// Drop session-scoped maps so the closed Session doesn't hold onto
	// per-turn / per-child-thread entries indefinitely. The dispatch
	// goroutine and rollout observer have exited by this point, so no
	// concurrent writer races these deletions. We deliberately leave
	// s.pending as an empty map (readLoop already drained it) — a late
	// sendRequest caller would otherwise panic writing to a nil map;
	// the existing WriteLine-on-closed-proc path handles shutdown
	// cleanly.
	s.mu.Lock()
	deferredChildEvents := 0
	for _, queued := range s.deferredChildWireEvents {
		deferredChildEvents += len(queued)
	}
	if deferredChildEvents > 0 {
		log.Printf("codex: closing with %d quarantined child events whose spawn ownership never arrived", deferredChildEvents)
	}
	s.seenTurnStarts = nil
	s.pendingTurnSchemaKnown = false
	s.pendingTurnSchemaed = false
	s.schemaedTurnIDs = nil
	s.structuredOutputByTurn = nil
	s.childParentByThread = nil
	s.childParentByAgentPath = nil
	s.agentPathByThread = nil
	s.agentMetaByThread = nil
	s.subagentNotificationDedup = nil
	s.rawToolCallsByID = nil
	s.waitReceiverIDsByCall = nil
	s.deferredChildWireEvents = nil
	s.deferredChildWireCount = 0
	s.deferredChildWireBytes = 0
	for _, timer := range s.deferredChildDeadlines {
		timer.Stop()
	}
	s.deferredChildDeadlines = nil
	s.collabHistoryQueue = nil
	s.collabHistoryVisited = nil
	s.mu.Unlock()
	return err
}

// claimTurnStart records the first observation of a turnID, returning
// true. A second observation returns false so dispatchLine can skip the
// duplicate EventTurnStart. The map is bounded by the number of live
// turns — cleared on EventTurnComplete or session Close.
func (s *Session) claimTurnStart(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenTurnStarts == nil {
		s.seenTurnStarts = make(map[string]struct{})
	}
	if _, ok := s.seenTurnStarts[turnID]; ok {
		return false
	}
	s.seenTurnStarts[turnID] = struct{}{}
	return true
}

// clearTurnStart drops the recorded turnID on completion so a follow-up
// turn with the same ID (rare, but possible across resumed sessions)
// can fire fresh.
func (s *Session) clearTurnStart(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	delete(s.seenTurnStarts, turnID)
	s.mu.Unlock()
}
