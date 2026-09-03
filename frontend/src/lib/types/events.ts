import type { Item, ThreadGroup } from './models';
import type { ProviderID } from './providers';

export type ApprovalKind =
  | 'command'
  | 'write-stdin'
  | 'file-read'
  | 'file-change'
  | 'permission'
  | 'mcp-elicitation';

export interface UserInputQuestionOption {
  label: string;
  description: string;
  /**
   * Optional markdown content rendered in a side-by-side preview pane next
   * to the option list when any option in a single-select question carries
   * a non-empty preview. Used for ASCII mockups, code-snippet comparisons,
   * configuration variants. Ignored on multi-select questions per the
   * upstream tool spec.
   */
  preview?: string;
}

export interface UserInputQuestion {
  id: string;
  header: string;
  question: string;
  options?: UserInputQuestionOption[];
  multiSelect?: boolean;
}

interface NetworkPermissions {
  enabled?: boolean;
}

interface FileSystemPermissions {
  read?: string[];
  write?: string[];
}

interface PermissionProfile {
  network?: NetworkPermissions;
  fileSystem?: FileSystemPermissions;
}

/**
 * ElicitationRequest mirrors the Go ApprovalRequest.Elicitation field.
 * Populated for `kind: "mcp-elicitation"`.
 *
 * Two modes:
 *  - `form` — the server wants structured user input described by a JSON
 *    schema. `requestedSchema` is the raw schema from the MCP wire format;
 *    the UI parses it via utils/elicitationSchema.ts.
 *  - `url`  — the server wants the user to complete an out-of-band flow at
 *    an external URL (e.g. OAuth consent). The response only carries the
 *    user's action (accept / decline / cancel) since completion happens in
 *    the browser.
 */
export interface ElicitationRequest {
  mode: 'form' | 'url';
  message: string;
  serverName?: string;
  // Form mode:
  requestedSchema?: unknown;
  // URL mode:
  url?: string;
  elicitationId?: string;
}

export interface ApprovalRequest {
  requestId: string;
  threadId: string;
  turnId?: string;
  toolUseId?: string;
  /**
   * Launch tool_use of the SUBAGENT that is asking; absent when the main
   * agent asks. Nests the request under that agent's card ("needs
   * approval" pill) — the prompt itself still renders in the thread's
   * normal approval UI.
   */
  parentToolUseId?: string;
  toolName: string;
  description: string;
  input: unknown;
  title: string;
  kind?: ApprovalKind;
  providerApprovalId?: string;
  /** Provider-native values. Codex can include structured policy amendments. */
  availableDecisions?: unknown[];
  permissions?: PermissionProfile;
  elicitation?: ElicitationRequest;
}

export interface ContextWindow {
  usedTokens: number;
  maxTokens?: number;
  usedPercentage?: number;
  autoCompactPercent?: number;
  autoCompactTokenLimit?: number;
  /**
   * Wire-confirmed `ContextWindowExceeded` sentinel from Codex
   * (`last.totalTokens === modelContextWindow`). Render as a distinct
   * state — not a real reading at 100%. Claude has no equivalent.
   */
  exceeded?: boolean;
}

export interface ApprovalEvent {
  action: 'request' | 'resolve' | 'fail';
  threadId?: string;
  request?: ApprovalRequest;
  requestId?: string;
  decision?: '' | 'approved' | 'declined' | 'amended' | 'lost' | 'failed';
  detail?: string;
  requestedAt?: number;
  /**
   * The page load whose answer failed, on `action: 'fail'` only. A
   * failure is not a fact about the thread — the prompt is still open
   * everywhere else — so only the connection that tried shows the
   * banner. Absent means no screen was behind it (or a backend too old
   * to stamp it), which reads as "not mine".
   */
  connectionId?: string;
}

export interface UserInputRequest {
  requestId: string;
  threadId: string;
  turnId?: string;
  toolUseId?: string;
  /** Same scope rule as ApprovalRequest.parentToolUseId. */
  parentToolUseId?: string;
  toolName: string;
  title: string;
  questions: UserInputQuestion[];
}

export interface UserInputEvent {
  action: 'request' | 'resolve' | 'fail';
  threadId?: string;
  request?: UserInputRequest;
  requestId?: string;
  decision?: '' | 'answered' | 'declined' | 'lost' | 'failed';
  detail?: string;
  requestedAt?: number;
  /** Mirrors ApprovalEvent.connectionId, on `action: 'fail'` only. */
  connectionId?: string;
}

/**
 * Claude safety/classifier fallback. `effectiveModel` is session-scoped;
 * an empty value clears the override and returns the UI to Thread.model,
 * which remains the user's requested model.
 */
export interface ModelFallbackEvent {
  threadId: string;
  requestedModel?: string;
  effectiveModel?: string;
  reason?: string;
  category?: string;
  revision: number;
}

export interface PendingInteractiveRequests {
  approvals: ApprovalRequest[];
  userInputs: UserInputRequest[];
}

export interface ItemDeltaEvent {
  threadId: string;
  itemId: string;
  kind: string;
  delta: string;
  updatedAt: number;
}

/**
 * Re-validated `meta` blob for an in-flight row. Used by triage's
 * streaming path-link validator: each flushed text delta re-runs the
 * workspace allowlist and pushes the resulting `pathRefs` JSON onto the
 * existing row so path tokens can become clickable mid-stream instead of
 * only after the model finishes. Frontend listeners MUST flush any
 * queued deltas for the same `itemId` before applying the meta so the
 * allowlist lands against text the user has already seen.
 */
export interface ItemMetaEvent {
  threadId: string;
  itemId: string;
  kind: string;
  meta: string;
  updatedAt: number;
}

export interface ItemPatchEvent {
  threadId: string;
  itemId: string;
  kind: string;
  patch: {
    status?: Item['status'];
    summary?: string;
    meta?: string;
    decision?: Item['decision'];
    updatedAt?: number;
  };
}

export type ItemStreamEvent =
	  | {
	      action: 'upsert';
	      threadId: string;
	      item: Item;
	    }
	  | ({ action: 'delta' } & ItemDeltaEvent)
	  | ({ action: 'meta' } & ItemMetaEvent)
	  | ({ action: 'patch' } & ItemPatchEvent);

/**
 * RateLimitsSnapshot mirrors the Go `provider.RateLimitsSnapshot` payload.
 * Lives here because the chat-rewrite `UsageEvent` now folds rate-limit
 * snapshots onto the provider:usage channel via `action: 'rate_limits'`.
 * See docs/architecture/chat-rewrite.md "Channels".
 */
export interface RateLimitsSnapshot {
  provider: string;
  accountId?: string;
  limits: RateLimitEntry[];
  updatedAt: number;
  /**
   * The reading is the provider's whole answer for this account, so a stored
   * limit it omits no longer exists. Absent on partial readings (a
   * single-window wire event, Claude's header fallback, a Codex per-bucket
   * notification) and on the cached union the backend persists.
   */
  complete?: boolean;
}

export interface UsageEvent {
  action: 'usage' | 'reset' | 'rate_limits' | 'rate_limits_removed';
  threadId: string;
  usedTokens?: number;
  maxTokens?: number;
  contextPercent?: number;
  autoCompactPercent?: number;
  autoCompactTokenLimit?: number;
  /**
   * Mirrors `ContextWindow.exceeded` — set when the wire signals
   * `ContextWindowExceeded` rather than a real reading at 100%.
   */
  exceeded?: boolean;
  rateLimits?: RateLimitsSnapshot;
}

export interface BackgroundTasksChangedEvent {
  threadId: string;
}

/**
 * Tray-state nudge fired when the host-side process state of a
 * Claude background task changes. Two states emitted today:
 *
 *  - `exited`   — task_updated terminal observed (host process exit);
 *                 stash row written, tray hides launch.
 *  - `drained`  — agent observation arrived and the tool_completion
 *                 sibling has been written; stash row removed.
 *
 * Pure UI nudge — the tray query is the source of truth. A frontend
 * that misses the event still gets correct state on its next
 * mount/query. See internal/triage/turn_events.go BackgroundTaskStateEvent.
 */
export interface BackgroundTaskStateEvent {
  threadId: string;
  taskId: string;
  launchId?: string;
  state: 'exited' | 'drained';
  updatedAt: number;
}

/**
 * Persistent provider-status kinds carried on `provider:status` by the
 * chat-rewrite router (closed set — see docs/architecture/chat-rewrite.md
 * "Channels"). Surfacing a value outside this set in the banner is a bug,
 * not a feature: the frontend should drop unknown kinds with a console warn.
 */
export type ProviderStatusKind =
  | 'binary_missing'
  | 'unauthenticated'
  | 'version_incompatible'
  // The installed provider CLI was replaced while this thread's session was
  // already running it. Thread-scoped and warning-severity: nothing is
  // broken, the session is just pinned to the old binary until it restarts.
  // There is no matching CLEAR kind — a withdrawal has nothing new to say,
  // so the backend clears with the legacy thread-scoped `status: 'ready'`.
  | 'binary_stale';

export interface ProviderStatusEvent {
  /**
   * Legacy shape (app_provider_status.go / binary detection). `provider`
   * and `status` are the fields the existing ProviderStatusBanner renders;
   * both are required for the banner to scope + dispatch correctly.
   */
  provider: ProviderID;
  status:
    | 'ready'
    | 'not_found'
    | 'version_too_old'
    | 'unauthenticated'
    | 'error'
    // No legacy counterpart — the binary-detect pipeline has no notion of a
    // session outliving the binary it started on. Carried in the same union
    // so the banner branches on one vocabulary.
    | 'binary_stale';
  message?: string;
  version?: string;
  actionable: boolean;
  actionUrl?: string;
  /**
   * New chat-rewrite kind enum. Optional because the legacy binary-detect
   * emissions don't populate it; the router does for EventSessionStatus.
   * When present, the banner can branch on this first and fall back to
   * `status` otherwise.
   */
  kind?: ProviderStatusKind;
  /**
   * `binary_stale` only: the CLI version this thread's live session was
   * started on, and the version now installed. Either can be empty when the
   * backend could not read one, so the banner copy degrades rather than
   * printing half an arrow. Empty on every other kind.
   */
  sessionVersion?: string;
  installedVersion?: string;
  /**
   * threadId is attached by the new chat-rewrite router so the banner can
   * scope to a specific pane. Legacy binary-detect emissions omit it —
   * those are provider-global and the banner fans out to every matching pane.
   */
  threadId?: string;
}

/**
 * Authenticated account info for a provider, populated by the startup
 * probe (Claude: control_request{subtype:"initialize"}, Codex:
 * account/rateLimits/read). Surfaced via the `provider:account` event
 * and rendered in the rate-limit ring popover. Empty subscriptionType
 * means "probe succeeded but plan info isn't available" — the popover
 * branches on that to keep its layout stable.
 */
export interface ProviderAccountEvent {
  provider: ProviderID;
  accountId?: string;
  generation?: number;
  cleared?: boolean;
  account: {
    email?: string;
    displayName?: string;
    subscriptionType?: string;
    tokenSource?: string;
    apiProvider?: string;
  };
}

/** Account currently attached to one live provider process/thread. */
export interface ProviderSessionAccountEvent extends ProviderAccountEvent {
  threadId: string;
  connected: boolean;
}

export interface ProviderAccountUsageErrorEvent {
  provider: ProviderID;
  accountId: string;
  message?: string;
}

export interface RateLimitEntry {
  limitId: string;
  limitName: string;
  usedPercent: number;
  windowMins: number;
  resetsAt: number;
}

// ---------------------------------------------------------------------------
// Turn lifecycle events — shapes mirror internal/triage/turn_events.go. See
// docs/architecture/turn-lifecycle.md §Frontend state shape for how these
// feed into the global active-turn registry (threadStatuses.svelte.ts) and
// pane.latestSettledTurn.
// ---------------------------------------------------------------------------

/**
 * TurnStartedEvent is the payload the frontend receives on
 * `provider:turn_started`. When this arrives, events.ts writes the
 * active-turn entry into the global registry (threadStatuses) so the
 * working indicator lights up — readers call getActiveTurn(threadId).
 */
export interface TurnStartedEvent {
  threadId: string;
  turnId: string;
  turnIndex: number;
  /** Wall-clock unix-millis; the working indicator reads this for its timer. */
  startedAt: number;
}

/**
 * TurnCompletedEvent is the payload for `provider:turn_completed`. Carries
 * the settled-turn projection the UI uses for read-state/trace surfaces
 * plus clear the working indicator. `tokenUsageJson` is a JSON-encoded
 * string because triage stores it that way for the DB round-trip — the
 * event listener parses it into TokenUsageSummary before writing pane state.
 */
export interface TurnCompletedEvent {
  threadId: string;
  turnId: string;
  turnIndex: number;
  startedAt: number;
  completedAt: number;
  stopReason: string;
  /**
   * Provider message id (Claude `msg_…`, Codex equivalent) of the
   * FINAL assistant message of this turn. For multi-round logical
   * turns (Claude task_notification cascades, stdin-during-wait
   * re-rounds) each round's settle overwrites the persisted column
   * so the value always points to the last message — matching the
   * `SettledTurn.assistantMessageId` contract. Empty string when
   * the provider didn't report one (e.g. session-died synthesis
   * before any assistant envelope).
   */
  assistantMessageId?: string;
  /**
   * JSON-encoded string carrying the provider's `usage` snapshot. Triage
   * round-trips it through SQLite so the shape matches the Go-side
   * `json.RawMessage`. The listener parses this into TokenUsageSummary
   * before exposing it to panes.
   */
  tokenUsage?: string;
  errorMessage?: string;
  /** True when the turn ended via interruption / truncation / abort. */
  aborted?: boolean;
  /** False for nested subagent/internal turns that should not reorder the sidebar. */
  countsAsActivity?: boolean;
  /**
   * True when this turn ended because the user message was reverted
   * via InterruptAndRevertIfClean (Stop-before-response). The sidebar
   * pill MUST NOT flip to "Interrupted" — nothing happened, so don't
   * paint it like something did. Set by the backend Router after
   * `MarkTurnReverted` consumes its one-shot flag.
   */
  revertedUserMessage?: boolean;
  /**
   * Thread history invalidation stamps (docs/architecture/thread-replica-sync.md
   * §3) read as this event was built. Adopted IN MEMORY ONLY: a writer
   * outside the emitting goroutine (the async highlight-span worker) can
   * commit a rev bump before this read while its own frame arrives later
   * — or never — so the pair is not a full attestation and must never be
   * persisted into the replica (§3.4). Zero means "no stamp".
   */
  historyRev?: number;
  historyEpoch?: number;
}

/**
 * SessionDiedEvent is the payload for `provider:session_died`. Emitted
 * when a provider subprocess exits abnormally mid-turn (non-zero exit
 * code, killed by signal, read-loop EOF). Drives the session-error
 * banner with the Reconnect call-to-action. The working indicator is
 * cleared separately via the synthesized `provider:turn_completed`,
 * and the historical row lives in the timeline as a
 * `notification`-kind item with `meta.kind = "session_died"` — three
 * loosely-coupled observers, three jobs.
 */
export interface SessionDiedEvent {
  threadId: string;
  reason?: string;
  exitCode?: number;
  signal?: string;
  /**
   * The process's captured last stderr output, pre-sanitized backend-side
   * (single line, hard length cap). Carries the actual failure text for
   * exits with no wire output (bad CLI flag, missing module).
   */
  stderrTail?: string;
  /** Unix-millis when the exit was observed. */
  occurredAt: number;
}

/**
 * TodoStepStatus is the canonical camelCase status enum shared by both
 * providers. Codex emits this shape natively; Claude TodoWrite's
 * snake_case `in_progress` is normalised to `inProgress` in the parser
 * so the frontend sees one vocabulary.
 */
export type TodoStepStatus = 'pending' | 'inProgress' | 'completed';

/**
 * TodoStep is one item in a live todo list snapshot.
 *
 * `id` and `owner` are populated by the Claude Code 2.1.150+ Task*
 * family (TaskCreate / TaskUpdate). Legacy TodoWrite and Codex
 * update_plan omit both; the widget treats them as optional —
 * missing `id` falls back to position-based keying, missing or empty
 * `owner` suppresses the badge so the rendering matches the
 * pre-Task* behaviour exactly.
 */
export interface TodoStep {
  step: string;
  status: TodoStepStatus;
  id?: string;
  owner?: string;
}

/**
 * TodoUpdateEvent is the payload for `provider:todo_update`. Carries
 * the latest live-todo snapshot from either Claude TodoWrite or Codex
 * update_plan. The listener writes it to `pane.liveTodo`; the
 * activity rail's Todos segment renders the snapshot. There is no
 * timeline footprint — todos are session state, not history.
 */
export interface TodoUpdateEvent {
  threadId: string;
  steps: TodoStep[];
}

/**
 * TokenUsageSummary is the parsed shape the pane stores on
 * latestSettledTurn.tokenUsage. Built from the provider's `usage` JSON
 * snapshot (Claude's `result.usage` or Codex's token-usage payload). All
 * fields are optional because different providers populate different
 * subsets.
 */
export interface TokenUsageSummary {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  totalCostUsd?: number;
}

/**
 * Periodic host CPU + memory snapshot pushed from the Go backend on
 * `system:stats`. Drives the sidebar SystemStatsFooter above the
 * Settings entry. `isWsl` toggles the "WSL" label so the panel reads
 * as WSL-specific when the backend is running inside a WSL2 distro;
 * native Linux / macOS render without it.
 */
export interface SystemStatsEvent {
  isWsl: boolean;
  cpuPercent: number;
  memUsedBytes: number;
  memTotalBytes: number;
}

/**
 * WorktreeSetupEvent is one frame on the `worktree:setup` channel — the
 * per-project setup recipe running over a worktree a chat thread just had cut
 * (app_worktree_setup.go). `phase` names which fields are meaningful:
 *
 *   - `started`       — `steps`, `worktreePath`, `startedAt`
 *   - `step-started`  — `stepIndex`
 *   - `output`        — `stepIndex`, `seq`, `chunk`
 *   - `step-finished` — `stepIndex`, `state` (succeeded | failed), `error`
 *   - `finished`      — `state` (succeeded | failed | cancelled), `error`,
 *                       `finishedAt`
 *
 * `seq` is a monotonic per-run chunk counter. It is what lets a snapshot and
 * the live stream be reconciled without double-appending output, and what
 * makes a lost frame detectable — see stores/worktreeSetup.svelte.ts.
 */
export interface WorktreeSetupEvent {
  phase: 'started' | 'step-started' | 'output' | 'step-finished' | 'finished';
  threadId: string;
  runId: string;
  worktreePath?: string;
  steps?: WorktreeSetupStepFrame[];
  stepIndex?: number;
  seq?: number;
  chunk?: string;
  state?: string;
  error?: string;
  startedAt?: number;
  finishedAt?: number;
}

/** One resolved step of a setup run, in execution order. */
export interface WorktreeSetupStepFrame {
  index: number;
  /** `copy` for the file-copy phase, `command` for an argv command. */
  kind: string;
  label: string;
  argv?: string[];
}

/**
 * Live counters for one running subagent (Go: provider.SubagentProgressMeta).
 * Cumulative for the agent's whole run; a provider that cannot report a
 * field omits it (Codex reports only totalTokens).
 */
export interface SubagentProgress {
  taskId?: string;
  toolUses?: number;
  totalTokens?: number;
  durationMs?: number;
  /** The agent's CURRENT activity line, not the launch description. */
  activity?: string;
  lastToolName?: string;
  agentType?: string;
  summary?: string;
  /** Epoch ms of the tick; stamped by the store on apply. */
  updatedAt?: number;
}

/** `provider:subagent_progress` payload (Go: triage.SubagentProgressEvent). */
export interface SubagentProgressEvent {
  threadId: string;
  /** The launch tool_use the tick belongs to. */
  itemId: string;
  /** The launch's own parent tool_use; absent at top level. */
  parentId?: string;
  progress: SubagentProgress;
  updatedAt: number;
}

/** One member of Claude's live background-task level set. */
export interface BackgroundTaskRef {
  taskId: string;
  toolUseId?: string;
  taskType?: string;
  description?: string;
}

/**
 * `settings:updated` payload (Go: app.SettingsUpdatedEvent). One frame per
 * TIER a persisted settings write moved.
 *
 * Values never ride this channel — settings carry redacted fields with no read
 * path, so the frame names the changed keys and every client re-reads through
 * `GetSettings` (see `resyncSettings`). `tier` is "host" | "user" | "device":
 * whose preference moved (the backend machine's, the person's, this screen's).
 */
export interface SettingsUpdatedEvent {
  tier: string;
  keys: string[];
}

/**
 * thread-group:updated — one frame per thread-group write, so a second
 * connected client stays current. `delete` carries only the id in
 * `group.id`; the rest of the row is whatever the backend last knew.
 * Thread membership changes ride the existing thread:updated channel
 * (`action: 'full'`) rather than this one.
 */
export interface ThreadGroupUpdateEvent {
  action: 'create' | 'patch' | 'delete';
  group: ThreadGroup;
}
