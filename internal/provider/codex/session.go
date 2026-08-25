package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/provider"
)

// session.go — the shared Session state and the accessors over it: the struct
// every other file in this package reaches through, the Config it is built
// from, the serialized event-emission gate, the root-thread-id atomic, the
// MCP handler registrations and their retained startup states, and Close,
// which drops the session-scoped maps field by field.
//
// The two sequences that operate on this state live beside it:
// session_start.go builds a Session and gives it a thread, and
// session_turn.go serves the turn verbs on a live one.

// ErrApprovalAlreadyResolved is returned by RespondToApproval when the
// request ID has already been answered. Prevents a second write landing
// at the provider with a stale decision.
var ErrApprovalAlreadyResolved = fmt.Errorf("codex: approval already resolved: %w", provider.ErrStaleInteractiveRequest)

// ErrNoActiveTurn is returned by Steer when there is no active turn to
// steer into. The app layer treats this as a "fall back to Send" signal
// (race window: turn just ended between the frontend reading the
// active-turn registry and the steer RPC arriving here).
var ErrNoActiveTurn = errors.New("codex: no active turn to steer")

// ErrTurnNotSteerable is returned by Steer when a turn IS running but cannot
// take input: upstream refuses `turn/steer` on a review turn and on a
// compaction turn (SteerInputError::ActiveTurnNotSteerable, surfaced as
// `codexErrorInfo: {"activeTurnNotSteerable": {...}}` in the refusal's
// `error.data`).
//
// It is deliberately NOT folded into ErrNoActiveTurn. The two want opposite
// recoveries: a no-active-turn race means the message should open a turn of
// its own, whereas here a turn is running and starting a second one would
// interleave the user's message with a review or a compaction. The app layer
// holds the message instead and re-dispatches at the next turn boundary.
var ErrTurnNotSteerable = errors.New("codex: the active turn cannot be steered")

// IsTurnNotSteerable reports the non-steerable-turn refusal above. Exported so
// the app layer can branch without importing the sentinel by value.
func IsTurnNotSteerable(err error) bool {
	return errors.Is(err, ErrTurnNotSteerable)
}

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
	proc     *provider.Process
	ctx      context.Context
	threadID string // our internal thread ID
	workDir  string // absolute workspace cwd used for project-scoped config/read
	// codexThreadID is the Codex app-server's thread ID for this session's
	// root thread, learned from the thread/start (or thread/resume) response.
	// Read it with rootThreadID(); write it with setRootThreadID().
	//
	// It is atomic rather than mu-guarded because the read loop is already
	// running when the constructor learns the id — NewSession has to start
	// readLoop to receive the handshake response at all, so every
	// notification arriving in that window reads the field concurrently with
	// the write. Two of those readers (registerChildOwnershipWithSource,
	// collabProfileForThread) hold mu already, so folding this field into mu
	// would introduce a self-deadlock; a second mutex would introduce a lock
	// order. An atomic has neither.
	codexThreadID atomic.Pointer[string]
	activeTurnID  string // current active turn ID from turn/started; cleared on turn/completed
	// review is the bounded correlation state for Codex's built-in inline
	// review. A review has two turn ids: the outer id carried by visible item
	// events and turn/completed, and a private execution id carried only by
	// turn/started. The private id must remain activeTurnID because it is the
	// only id turn/interrupt accepts. review owns the separate visible scope.
	review *reviewRun
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
	// without a process restart. All six are guarded by mu: Send copies
	// them per turn, ApplyLiveUpdate rewrites them, and the read loop reads
	// model for usage attribution.
	model           string // requested model; the usage-attribution fallback when Codex has reported nothing
	reasoningEffort string // per-turn reasoning effort override; empty means inherit thread default
	serviceTier     string // per-turn service tier override; "priority" enables Codex fast mode
	approvalPolicy  string // per-turn approval override; empty means inherit thread default
	sandbox         string // per-turn sandbox override; empty means inherit thread default
	// approvalsReviewer is always non-empty (threadApprovalsReviewer resolves
	// the Config's empty value to the protocol default at construction) and is
	// therefore sent on every turn/start. Omitting it would leave the previous
	// reviewer sticky across a runtime-mode switch — the exact opt-out-must-
	// clear-state failure t3-improvements.md §3.2 calls out.
	approvalsReviewer string
	// assertedServiceTier is the `serviceTier` value AO has most recently
	// written onto this thread ("" = AO has never written the axis). Codex
	// spells the axis as a double option on every params struct that carries
	// it: omitted leaves it unchanged, explicit null clears it. Tracking what
	// we asserted is what makes fast-mode OFF actually clear the tier a
	// previous ON stored, without ever clobbering a tier the user's own
	// config selected and AO never touched. Seeded from Config.ServiceTier in
	// NewSession because buildThreadParams already sends the axis on the
	// handshake, so a session that started fast has asserted the tier before
	// it runs a turn. Guarded by mu; the only other writer is
	// commitServiceTierWrite, after a write has landed.
	assertedServiceTier string
	// settingsUpdateUnsupported latches when this app-server answered
	// `thread/settings/update` with a method-unknown error. Per session
	// because a live session cannot swap binaries; a session started after an
	// upgrade re-learns the answer. Guarded by mu.
	settingsUpdateUnsupported bool
	// pendingSettingsEcho is the single-shot, deadline-bounded expectation a
	// successful settings push leaves for the next `thread/settings/updated`
	// (thread_settings_push.go). Guarded by mu.
	pendingSettingsEcho *settingsEchoExpectation
	// observedSettings is Codex's own view of this thread's live config,
	// reconciled from `thread/settings/updated` (thread_settings.go). It is
	// deliberately separate from the requested config above: Codex can
	// change model / effort / tier without AO asking (reroute, guardian
	// downgrade, config reload), and folding its echo back into the
	// requested block would let a stale echo clobber a pending user
	// selection. Guarded by mu; observedSettingsKnown distinguishes
	// "Codex reports the empty string" from "Codex has not reported".
	observedSettings      ThreadSettings
	observedSettingsKnown bool
	// unclaimedNotifications dedupes the protocol-drift warning to once
	// per method per session (notification_catalog.go). Bounded because
	// the key comes off the wire. Guarded by mu.
	unclaimedNotifications           map[string]struct{}
	unclaimedNotificationsOverflowed bool
	nextID                           atomic.Int64
	// LOCK ORDER: mu → childLifecycleMu → eventMu. Take them in that order or
	// not at all; nothing in this package takes them in the reverse direction.
	// approvalsMu and collabAsyncMu are LEAVES — no other Session lock may be
	// acquired while either is held (drainPendingApprovals deliberately
	// releases approvalsMu before it emits; startCollabAsync only starts a
	// goroutine, which enters the order on its own stack).
	//
	// Two edges are actually exercised. Close takes childLifecycleMu under mu
	// to drop childLifecycleRevision. observeAndEmitChildLifecycle and
	// emitRecoveredChildStatus reserve eventMu under childLifecycleMu and then
	// release childLifecycleMu before delivering, so a later child-lifecycle
	// observation cannot overtake an earlier one while still keeping the
	// external callback out from under the lifecycle lock.
	//
	// mu is NEVER held across an emit. Every emitter builds its events under
	// mu, releases it, and only then calls emitEvent / emitEvents — see
	// thread_settings.go for the canonical shape. eventMu is the ONLY lock
	// that may be held across s.onEvent (emitEventLocked): that is what gives
	// the provider callback its serialized-delivery contract without pinning
	// session state while the app layer runs.
	//
	// codexThreadID, appServerVersion, threadHistoryMode, pendingRevert,
	// threadQueueNative, closing and nextID are atomics precisely so their
	// readers never have to enter this order at all.
	mu                 sync.Mutex
	pending            map[int64]chan json.RawMessage
	onEvent            func(provider.ProviderEvent)
	eventMu            sync.Mutex
	dynamicToolHandler DynamicToolHandler
	cancel             context.CancelFunc
	closing            atomic.Bool
	readDone           chan struct{}
	// approvalsMu guards pendingApprovals, resolvedApprovals, and
	// approvalsClosed. A LEAF in the lock order above: no other Session lock
	// may be taken while it is held.
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
	// turnOrigins / pendingLocalTurnStarts answer "did WE start this turn?"
	// for every `turn/started` that arrives. A codex app-server can start a
	// turn on its own from the external queue (external_turns.go), and the
	// answer decides whether the echoed user message is marked as injected.
	// Both guarded by mu; entries are dropped on turn completion.
	turnOrigins            map[string]turnOrigin
	pendingLocalTurnStarts int
	// ambiguousLocalTurnStarts counts how many of those pending claims are
	// held ONLY because a `turn/start` request timed out after its write —
	// the turn may exist, so the claim is deliberately not released
	// (abandonLocalTurnStart's doc). Tracked separately because such a claim
	// is the one that can outlive every turn it could have described, and a
	// surplus claim is eventually consumed by an unrelated `turn/started` —
	// telling the user an externally injected turn was their own. Reconciled
	// the moment something proves the turn dead; see
	// dropAmbiguousLocalTurnStartsLocked.
	ambiguousLocalTurnStarts int
	// reportedForeignSubmissions dedupes the "queued from outside Agent
	// Overflow" notice per foreign submission id, because the provider's queue
	// is re-listed on every `thread/queue/changed` (thread_queue.go). Guarded
	// by mu and bounded by maxReportedForeignSubmissions.
	reportedForeignSubmissions map[string]struct{}
	// ownsQueuedClient is Config.OwnsQueuedClientID, the app layer's answer to
	// "is this listed submission ours?". Immutable after construction and
	// therefore read without mu; nil means every submission is foreign.
	ownsQueuedClient func(string) bool
	// queueListInflight / queueListDirty single-flight the
	// `thread/queue/changed` reconciliation. Every AO mutation and every
	// automatic dispatch raises a change, so without this an N-message queue
	// would spawn 2N goroutines each walking up to queueListPageCap paginated
	// list RPCs. Dirty re-runs the walk once when a change lands while one is
	// already in flight, so the last notification is never the one that gets
	// dropped. Guarded by mu.
	queueListInflight bool
	queueListDirty    bool
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
	// childThreadByAgentPath identifies the current owner of a reusable path.
	// agentPathByThread retains each thread's canonical path so replay from a
	// superseded child can be rejected and close_agent can clean up safely.
	//
	// No heuristic background-tool classifier runs here (invariant 25).
	// The wire-typed signals for a backgrounded item are
	// `CommandExecution.source == "unifiedExecStartup"` and
	// `collabAgentToolCall` with a running child in `agentsStates`; those
	// are what authorize `is_background=true`. The actual stamp happens in
	// `internal/triage/codex_background.go` on the first model-produced
	// yield or at the turn-close catchall — this session package only
	// surfaces the wire fields; it doesn't project them.
	childParentByThread    map[string]string
	childParentByAgentPath map[string]string
	childThreadByAgentPath map[string]string
	childPathOwnerLive     map[string]bool
	agentPathByThread      map[string]string
	// childTurnGenerations counts each owned child's `turn/started`
	// notifications, keyed by canonical agent path where one is known and by
	// provider thread id otherwise. It is the child-turn tiebreak in
	// subagentNotificationDedupKey; nothing else reads it.
	childTurnGenerations      map[string]uint64
	agentMetaByThread         map[string]collabReceiverMeta
	subagentNotificationDedup map[subagentNotificationDedupKey]struct{}
	// childLifecycleMu serializes live and recovered child status emission.
	// The revision rejects a stale thread/read snapshot if a live lifecycle
	// notification arrived while the recovery request was in flight. Sits
	// between mu and eventMu in the lock order above.
	childLifecycleMu       sync.Mutex
	childLifecycleRevision map[string]uint64
	rolloutObserverWG      sync.WaitGroup
	collabMetadataReads    chan struct{}
	rawToolCallsByID       map[string]rawToolCall
	waitReceiverIDsByCall  map[string][]string
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
	// collabAsyncMu is a LEAF in the lock order above — the work it guards is
	// handed to a goroutine, which enters the order on its own stack.
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
	// terminateBackgroundTerminalFn mirrors cleanBackgroundTerminalsFn for
	// TerminateBackgroundTerminal(). Used by app_codex_background_test.go to
	// verify the per-row stop binding forwards the process id and installs a
	// deadline without spinning up a real app-server. Production NewSession
	// never sets it; the wire shape itself is pinned by the in-package tests
	// in session_background_terminals_test.go.
	terminateBackgroundTerminalFn func(ctx context.Context, processID string) (bool, error)
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
	// mcpStartupStates retains the last `mcpServer/startupStatus/updated`
	// seen per server name for this session's lifetime, so a caller
	// listing MCP rows can consult the thread's own startup lifecycle
	// instead of re-deriving it from a probe. Retention is independent of
	// mcpStartupUpdateHandler — a session whose observer is not yet
	// registered still has a startup history. Guarded by mu; lazily
	// allocated.
	mcpStartupStates map[string]MCPStartupUpdate
	// skillsChangedHandler fires when Codex emits `skills/changed` — the
	// app-server's own signal that its watched skill files moved. The App
	// layer drops the skills cache so the next composer render re-reads
	// instead of serving a list that no longer matches disk. Guarded by mu;
	// nil when no observer is registered.
	skillsChangedHandler SkillsChangedHandler
	// binary is the codex CLI this process was spawned from. Kept so a
	// caller deciding whether to ride this session's connection for a
	// binary-scoped read (skills) can match on it rather than assuming the
	// current setting still describes a process that started earlier.
	binary string
	// appServerVersion is the codex build the LIVE process reports at
	// `initialize` (`InitializeResponse.userAgent`), parsed by
	// recordAppServerVersion. Atomic for the same reason codexThreadID is:
	// readLoop is already running when the handshake answers, and a
	// per-method version gate must be readable from any goroutine without
	// taking mu. "" means "could not be determined", which every gate
	// treats as too old — see app_server_version.go.
	appServerVersion atomic.Pointer[string]
	// threadHistoryMode is the `thread.historyMode` this session's thread
	// reported on its start/resume response, recorded by
	// recordThreadHistoryMode. Atomic for the same reason appServerVersion
	// is. "" means the app-server did not state one, which every gate
	// treats as not-paginated — see session_revert.go.
	threadHistoryMode atomic.Pointer[string]
	// pendingRevert is the in-place history cut this session has asked for
	// and not yet seen echoed. `thread/reverted` carries only a thread id,
	// so this is the only thing that can tell a solicited cut from a
	// foreign one — see session_revert.go.
	pendingRevert atomic.Pointer[revertExpectation]
	// threadQueueNative freezes, at handshake time, whether this session
	// hands mid-turn user messages to the provider's own `thread/queue/*`
	// (codex >= 0.148) instead of AO's in-process flushqueue. Atomic for the
	// same reason appServerVersion is — the app-layer dispatcher reads it off
	// its own goroutine — and written exactly once, by
	// recordThreadQueueSupport, because the two queues must be mutually
	// exclusive for the whole life of the session.
	threadQueueNative atomic.Bool
}

// Binary returns the codex CLI path this session's app-server process was
// spawned from. It is fixed for the life of the process: a settings change
// takes effect on the next session, so a caller comparing this against the
// configured binary is asking "is this process still the one the setting
// describes", which is exactly the question a shared-read fallback needs.
func (s *Session) Binary() string { return s.binary }

// rootThreadID returns the Codex app-server thread ID this session's root
// thread is bound to, or "" before the thread/start handshake has answered
// (fresh sessions) — the zero value every caller already treats as "not
// known yet". Safe to call while holding mu.
func (s *Session) rootThreadID() string {
	if id := s.codexThreadID.Load(); id != nil {
		return *id
	}
	return ""
}

// setRootThreadID binds this session to a Codex app-server thread. There is
// exactly one write path — NewSession seeds it from cfg.ResumeThreadID and
// then stores the id the handshake response returned — so no caller has to
// re-derive the ordering between the two. Safe to call while holding mu.
func (s *Session) setRootThreadID(id string) {
	s.codexThreadID.Store(&id)
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
//
// FailureReason is the machine-readable half of a failure, distinct from
// the human Error string: upstream's McpStartupFailureReason enum
// currently has exactly one variant, MCPFailureReasonReauthRequired.
// Without it a token that needs refreshing is indistinguishable from a
// server that crashed, and the UI can only show a dead error string where
// it should offer a sign-in. Empty when the wire sent null (every
// non-failure update, and failures with no classified cause).
type MCPStartupUpdate struct {
	Name          string
	State         string
	Error         string
	FailureReason string
}

// TerminalFailure reports whether this update is a settled, unrecovered
// outcome — "failed" or "cancelled". These are the only retained states
// that outrank a fresh `mcpServerStatus/list` answer when a thread's MCP
// rows are merged (app_mcp_thread.go): the list describes settled
// attempts, so a retained "starting" is by definition an OLDER
// observation than a settled probe, and letting it win would pin a row
// at "Starting…" forever whenever the terminal notification was lost —
// the exact incident the merge exists to prevent. Unrecognized states
// defer to the probe for the same reason: an unknown observation must
// not outrank a settled one.
func (u MCPStartupUpdate) TerminalFailure() bool {
	switch strings.TrimSpace(u.State) {
	case "failed", "cancelled":
		return true
	}
	return false
}

// MCPStartupUpdateHandler observes `mcpServer/startupStatus/updated`
// notifications. Registered by app_session.go after NewSession
// returns — Codex only emits these post-thread/start, so registering
// after the handshake is safe.
type MCPStartupUpdateHandler func(update MCPStartupUpdate)

// Config for creating a Codex session.
type Config struct {
	Binary         string // default: "codex"
	Model          string
	WorkDir        string
	ApprovalPolicy string // "never", "on-failure", "on-request", "untrusted"
	Sandbox        string // "read-only", "workspace-write", "danger-full-access"
	// ApprovalsReviewer routes this thread's approval requests: "user" (the
	// human, Codex's protocol default) or "auto_review" (Codex's reviewer
	// subagent, provider.RuntimeAuto). Empty reads as the protocol default —
	// see threadApprovalsReviewer, which is what actually reaches the wire so
	// the field is sent explicitly on every start, resume, and turn regardless
	// of how the Config was built.
	ApprovalsReviewer     string
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
	// DisabledTools holds curated toggle ids (DisabledToolToggleIDs) for
	// built-in tools this session must not be given. Rendered into the
	// thread/start|resume `config` map by DisabledToolConfigOverrides.
	// Start-time only — the config map is not re-read per turn — which is
	// why PlanLiveUpdate's whole-Config comparison must see this field.
	DisabledTools []string
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
	// BeforeResume runs on a RESUME only, after the initialize handshake and
	// BEFORE the `thread/resume` that loads the thread. Never on a fresh
	// `thread/start` — there is nothing on the other side to reconcile with.
	//
	// It exists for one window, and the window is real rather than tidy:
	// `thread/resume` LOADS the thread, and a loaded thread's idle hook is
	// what drains its provider-side queue (`QueuedItemService::on_thread_idle`
	// → `start_turn_if_idle`). Anything that has to be true before the first
	// queued turn can start has to be
	// true HERE, not after NewSession returns — a rollback's purge of rows the
	// user just removed, say. `thread/queue/list` and `thread/queue/delete`
	// both answer for an unloaded thread (upstream's `require_thread` falls
	// back to a thread-store read and the listing is a plain SQLite page read),
	// which is what makes this side of the resume a usable place to ask.
	//
	// Synchronous, and its failures are its own: the hook runs inside
	// NewSession's handshake sequence, so anything slow here delays the
	// session, and returning nothing means a hook cannot fail a session that
	// is otherwise fine.
	BeforeResume func(*Session)
	// OwnsQueuedClientID answers whether a `clientUserMessageId` sitting in
	// the provider's own queue belongs to this app.
	//
	// AO never calls `thread/queue/add`, so the only rows it can encounter
	// were written by somebody else — a `codex queue --thread …` invocation,
	// or another client on the same Codex home. The predicate exists because
	// "somebody else" is not the same as "not this installation": only the app
	// layer holds the store rows that could account for an id, and the id
	// grammar itself is not a credential (see ownsQueuedClientID).
	//
	// Nil means every listed submission is foreign, which is the honest
	// default for a session given no way to claim one. Called off the read
	// loop's reconcile goroutine, so it must be safe for concurrent use and
	// must not block.
	OwnsQueuedClientID func(clientUserMessageID string) bool
}

// emitEvent preserves the provider callback's serialized-delivery contract
// even for metadata and history work that completes on collaboration workers.
func (s *Session) emitEvent(event provider.ProviderEvent) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.emitEventLocked(event)
}

func (s *Session) emitEventLocked(event provider.ProviderEvent) {
	if s.onEvent == nil {
		return
	}
	s.onEvent(event)
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

// MCPStartupStates returns a copy of the last startup update seen per
// MCP server name on this session. Last-write-wins for the session's
// lifetime: Codex's startup notifications are lossy by upstream's own
// account (its TUI treats them as such — a stale update from a finished
// round can arrive late, and a terminal one can be missed), so this map
// must never latch a state it can't be talked out of, and it is not a
// membership answer. Callers merge it against the current
// `mcpServerStatus/list` membership, which neutralizes entries for
// servers no longer in config and remains the reconciler of record.
func (s *Session) MCPStartupStates() map[string]MCPStartupUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mcpStartupStates) == 0 {
		return nil
	}
	out := make(map[string]MCPStartupUpdate, len(s.mcpStartupStates))
	for name, update := range s.mcpStartupStates {
		out[name] = update
	}
	return out
}

// ForgetMCPStartupState drops the retained startup update for one
// server. Called when AO itself asks Codex to re-run the server's
// startup — the post-OAuth reload, the enable toggle, a manual
// reconnect. The retained state describes a run the caller just
// invalidated, and Codex's fresh startupStatus round only arrives at
// the next turn boundary; keeping the entry would outrank the settled
// list until then, rendering "Failed · invalid_grant / Sign in again"
// over a sign-in that just succeeded.
func (s *Session) ForgetMCPStartupState(name string) {
	s.mu.Lock()
	delete(s.mcpStartupStates, name)
	s.mu.Unlock()
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
	s.review = nil
	s.childParentByThread = nil
	s.childParentByAgentPath = nil
	s.childThreadByAgentPath = nil
	s.childPathOwnerLive = nil
	s.agentPathByThread = nil
	s.childTurnGenerations = nil
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
	s.childLifecycleMu.Lock()
	s.childLifecycleRevision = nil
	s.childLifecycleMu.Unlock()
	s.collabHistoryQueue = nil
	s.collabHistoryVisited = nil
	s.mu.Unlock()
	return err
}
