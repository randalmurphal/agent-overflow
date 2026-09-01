import type { ProviderID } from './providers';

/**
 * ReasoningEffort mirrors provider.ReasoningEffort. The active model's
 * metadata controls which subset is selectable.
 */
export type ReasoningEffort =
  | "none"
  | "minimal"
  | "low"
  | "medium"
  | "high"
  | "xhigh"
  | "max"
  | "ultra";

/**
 * ContextWindow mirrors the per-thread context-window token count.
 * Supported values come from the active provider/model metadata because
 * Claude and Codex expose different standard and extended tiers.
 */
export type ContextWindow = number;

/**
 * ThreadMode mirrors the canonical mode column on thread rows. Discussion
 * is included for symmetry even though it's reached via a separate flow.
 */
export type ThreadMode = "chat" | "plan" | "discussion";

export type RuntimeMode =
  | "read-only"
  | "approval-required"
  | "auto-accept-edits"
  | "auto"
  | "full-access";

export type ThreadEnvMode = "local" | "worktree";

export type SansFont = "geist" | "hack-nerd" | "system";
export type MonoFont = "geist" | "hack-nerd" | "system";
export type PaneDensityMode = "compact" | "comfortable" | "spacious";
export type ActivityRunDefaultMode = "expanded" | "collapsed";
export type ProjectSortMode = "lastActivity" | "createdAt" | "manual";
export type CommitMessageStyle = "conventional" | "repo" | "custom";

/**
 * One breadcrumb hop in the agent companion's scope trail. The first entry
 * is always the thread itself (`itemId: ''`, label `main`); every later
 * entry is a launch row the reader descended into.
 */
export interface AgentPaneBreadcrumbEntry {
  itemId: string;
  label: string;
}

/**
 * The agent companion's persisted scope: the launch item its thread view is
 * filtered to, plus the trail that reached it. `scopeItemId` always equals
 * the LAST breadcrumb entry's `itemId`, so the two can never disagree about
 * where the pane is; a snapshot that breaks that is treated as malformed and
 * its pane is dropped on restore.
 *
 * Declared here rather than beside the store so `paneLayout.svelte.ts` can
 * carry it on a layout item without importing the agent store (which imports
 * the layout store — a runtime cycle a type import avoids).
 */
export interface AgentPaneScopeSnapshot {
  scopeItemId: string;
  breadcrumb: AgentPaneBreadcrumbEntry[];
}

export interface PaneLayoutPersistedPane {
  paneId: string;
  kind: "thread" | "plan" | "review" | "agent";
  threadId?: string;
  sourcePaneId?: string;
  widthPx: number;
  /**
   * Present on `agent` companions only, and REQUIRED there — an agent pane
   * with no scope has nothing to render, so the parser drops such an entry
   * instead of restoring a blank pane. Optional on the type because every
   * other kind omits it.
   */
  agentScope?: AgentPaneScopeSnapshot;
}

export interface PaneLayoutPersistedSettings {
  version: 3;
  panes: PaneLayoutPersistedPane[];
  focusedPaneId?: string | null;
}

/**
 * One user-defined environment variable for a provider. Mirrors
 * settings.ProviderEnvVar.
 *
 * `value` is empty for `sensitive` entries on every read path — the
 * backend redacts them before they cross the wire, and the UI masks the
 * row rather than revealing one. Changing a sensitive value means
 * entering a new one.
 */
export interface ProviderEnvVar {
  name: string;
  value: string;
  sensitive?: boolean;
}

/**
 * One system-prompt replacement entry, mirroring settings.PromptOverride.
 *
 * `models` holds normalized model slugs (no context-tier marker) the entry
 * applies to; the backend picks the FIRST enabled entry whose `models`
 * contains the session's model, so order is meaningful. `prompt` may embed
 * the `{{TOKEN}}` placeholders the backend renders at spawn — unknown text
 * inside braces stays literal.
 */
export interface PromptOverride {
  enabled: boolean;
  models: string[];
  prompt: string;
}

/** Claude Code's four built-in output styles. `''` means "no style selected". */
export type ClaudeOutputStyle = 'Concise' | 'Proactive' | 'Explanatory' | 'Learning';

/**
 * What a Claude session does with a message another Claude session on this
 * machine addresses to it.
 *
 * Claude Code's own schema has a third value, `hold`, which Agent Overflow
 * never writes: a held message waits for an approval a headless session has
 * no surface to give, so it is dropped after a timeout with nothing on the
 * wire to say so. See internal/settings/claudecrosssession.go.
 */
export type ClaudeCrossSessionInbound = 'accept' | 'refuse';

/**
 * Claude Code's machine-wide peer inbox: whether other Claude sessions on
 * this machine can discover and message a thread, and what an arriving
 * message does.
 *
 * Two mechanisms, one setting. `enabled` opens the CLI's experiment gate and
 * passes the peer-visible name, which is what binds the socket at all;
 * `inbound` is the delivery policy that only matters once it is bound. Off by
 * default — letting another process start a turn in your thread is opt-in.
 *
 * SPAWN-ONLY: a change converges by restarting the session, which the backend
 * queues as a deferred restart rather than doing under a running turn.
 */
export interface ClaudeCrossSession {
  enabled?: boolean;
  inbound?: ClaudeCrossSessionInbound | '';
}

/**
 * Caps on Claude's subagent fan-out. Zero means "unset" on both axes: the
 * CLI's schema is a positive integer, so 0 is unsendable and would be a
 * silent no-op rather than "no subagents".
 */
export interface ClaudeSubagentLimits {
  maxSpawnDepth?: number;
  maxConcurrent?: number;
}

/**
 * How much Claude thinks before it answers. `''` is Claude Code's own
 * per-model choice (adaptive where the model supports it, its built-in
 * budget otherwise) — not "off".
 */
export type ClaudeThinkingMode = 'off' | 'budget';

/**
 * Whether thinking text reaches the wire. `''` is Agent Overflow's default,
 * which is `summarized` — the spawn has always asked for it so newer models'
 * `omitted` default cannot silence the thinking pane.
 */
export type ClaudeThinkingDisplay = 'summarized' | 'omitted';

/**
 * Claude's extended-thinking preference. `budgetTokens` is meaningful only
 * with `mode: 'budget'`, and the backend drops it otherwise.
 */
export interface ClaudeThinking {
  mode?: ClaudeThinkingMode | '';
  budgetTokens?: number;
  display?: ClaudeThinkingDisplay | '';
}

export interface Settings {
  // NOTE: `theme` used to live here and is RETIRED (docs/architecture/theme-system.md
  // §6.2). The light/dark mode is a property of the client machine, not of a
  // backend, so it lives in `<configDir>/themes/appearance.json` and is read
  // through `stores/appearance.svelte.ts`. The Go field is retired the same
  // way (`retiredSettingsFieldNames`), so an old settings.json keeps its value
  // on disk without being republished.
  timestampFormat: "locale" | "12-hour" | "24-hour";
  /**
   * Sans typeface for the `--font-sans` CSS variable. Default `geist`
   * is eagerly bundled; `hack-nerd` lazy-loads its unicode-range woff2
   * slices on demand; `system` uses the OS fallback chain.
   */
  sansFont: SansFont;
  /**
   * Mono typeface for the `--font-mono` CSS variable. Same option set
   * and load behaviour as `sansFont`.
   */
  monoFont: MonoFont;
  fontSize: number;
  recentWorkspaces: string[];
  diffWordWrap: boolean;
  /**
   * Start chat-timeline file-edit diff cards collapsed (header row
   * only). Controls the default state, not capability — cards stay
   * individually expandable either way.
   */
  collapseDiffPreviews: boolean;
  streamingEnabled: boolean;
  /**
   * Minimize rendering work for GPU-constrained environments (weak
   * machines, or a game running alongside): instant scroll placement
   * instead of spring glides, per-wire-chunk text reveal instead of
   * animated streaming, activity shimmer suppressed. Display-only.
   */
  lowPowerMode: boolean;
  /** Expose Agent Overflow's built-in browser tools to Claude and Codex. */
  browserEnabled: boolean;
  /** Persist encrypted cookies and local storage per workspace. */
  browserPersistSiteData: boolean;
  /** Permit browser_open_file outside the current workspace/project roots. */
  browserAllowOutsideWorkspace: boolean;
  /**
   * Keep-awake master switch (the sidebar moon/sun toggle): while on,
   * the app holds an OS sleep inhibitor so the machine never
   * idle-sleeps. Persisted, so it survives restarts. Mirrors
   * internal/settings.Settings.KeepAwakeEnabled's zero-value default
   * (off).
   */
  keepAwakeEnabled: boolean;
  /**
   * Scope of keep-awake when it is on: true also keeps the display
   * from sleeping (machine + screen), false lets the screen blank
   * while the machine stays awake. Default on.
   */
  keepAwakeScreen: boolean;
  /**
   * OS notifications master switch. Off means this screen raises none at
   * all, including workflow attention and the update notice. Default on:
   * notifications were unconditional before these keys existed, so an
   * absent key must read as the behaviour the user already had.
   */
  notificationsEnabled: boolean;
  /** OS notification when a top-level turn finishes. Default on. */
  notifyTurnComplete: boolean;
  /** OS notification when the agent is blocked on your approval. Default on. */
  notifyApprovalNeeded: boolean;
  /** OS notification when a turn fails or a provider stops. Default on. */
  notifyError: boolean;
  /** OS notification when a provider's login is gone. Default on. */
  notifyProviderSignedOut: boolean;
  /**
   * Working-indicator spinner verbs: replace the rail's "Working" label
   * with one verb per turn, drawn from the built-in list plus
   * `spinnerCustomVerbs`. Default ON — the verbs are text-only and the
   * plain label is one toggle away.
   */
  spinnerVerbsEnabled: boolean;
  /**
   * Working-indicator sprite animations: replace the LED chase with a
   * per-turn random sprite from the pool. Default OFF — the LED chase
   * is the stock behavior.
   */
  spinnerAnimationsEnabled: boolean;
  /** User-added verbs, appended to the built-in pool. */
  spinnerCustomVerbs?: string[];
  /** Draw verbs from `spinnerCustomVerbs` only. */
  spinnerBuiltinVerbsDisabled: boolean;
  /**
   * Animation ids UNCHECKED from the random pool. Exclusion list so a
   * newly dropped custom sprite joins the pool without a settings
   * write.
   */
  spinnerDisabledAnimations?: string[];
  /**
   * Sprite shown while compacting: "" = the built-in default
   * (robo-papers), "none" = no override, else a sprite id.
   */
  spinnerCompactionAnimation: string;
  confirmArchive: boolean;
  confirmDelete: boolean;
  autoPinNewThreads: boolean;
  claudeBinaryPath: string;
  codexBinaryPath: string;
  claudeEnabled: boolean;
  codexEnabled: boolean;
  /**
   * Surfaces the `claude-tui` provider (the real interactive Claude TUI) in
   * the model/provider pickers. Unlike the two flags above this one defaults
   * to FALSE — the TUI surface is opt-in — and it is ANDed with
   * `claudeEnabled`, since claude-tui runs Claude's binary under Claude's
   * auth. Never read it directly: ask `providerIsEnabled` in
   * providers/catalog.ts, which owns that rule.
   */
  claudeTuiEnabled: boolean;
  /**
   * Model slugs hidden from model pickers (hide-list: absent slugs —
   * including newly added catalog models — stay visible). Display-only:
   * existing threads on a hidden model keep working. The Claude list
   * covers both `claude` and `claude-tui`. Go persists with
   * `omitempty`, so treat undefined as [].
   */
  claudeHiddenModels?: string[];
  codexHiddenModels?: string[];
  /**
   * User-defined environment variables applied to the provider processes
   * the backend spawns (sessions, account probes, text generation).
   * Applied at spawn, so edits reach new sessions only. The Claude list
   * covers claude-tui too. Go persists with `omitempty`, so treat
   * undefined as [].
   *
   * Mutated through SetProviderCustomEnvVar / DeleteProviderCustomEnvVar,
   * never through UpdateSettings — the backend rejects these keys on the
   * patch path because `value` comes back redacted for sensitive entries
   * and a read-mutate-write round trip would persist the redaction.
   */
  claudeCustomEnv?: ProviderEnvVar[];
  codexCustomEnv?: ProviderEnvVar[];
  /**
   * System-prompt replacements, in precedence order: the first enabled
   * entry whose `models` contains the session's model replaces the
   * provider's default prompt (Claude `--system-prompt`, Codex
   * `baseInstructions`). Read at spawn, so edits reach new sessions only.
   * Like claudeHiddenModels, the Claude list covers claude-tui too: the
   * interactive TUI is the same binary and honors `--system-prompt-file`
   * exactly as headless does (spike-verified on 2.1.234 — full body
   * replacement, only the TUI's own fixed identity line differs). Go
   * persists with `omitempty`, so treat undefined as [].
   */
  claudePromptOverrides?: PromptOverride[];
  codexPromptOverrides?: PromptOverride[];
  /**
   * Built-in tools kept out of the model's context, provider-wide (not
   * model-scoped). Claude holds raw CLI tool names — passed to
   * `--disallowedTools` verbatim, unknown names are harmless. Codex holds
   * curated toggle ids (see utils/promptOverrides.ts), each of which the
   * backend maps to per-thread config keys; Codex has no flat disallow
   * list. Spawn-only, and the Claude list covers claude-tui too, like the
   * prompt overrides above — the interactive TUI honors repeated
   * `--disallowedTools <name>` and the named tools' schemas are absent
   * from its requests (same 2.1.234 spike). Go persists with `omitempty`,
   * so treat undefined as [].
   */
  claudeDisabledTools?: string[];
  codexDisabledTools?: string[];
  /**
   * Turns off Claude Code's periodic "track your work with the todo
   * tools" nudges (CLAUDE_CODE_TODO_REMINDER_MODE=off at spawn; covers
   * claude-tui too). Only meaningful while at least one todo tool is
   * still exposed — with the whole group disabled the CLI has nothing
   * to nudge about. Go persists sparsely, so treat undefined as false.
   */
  claudeTodoRemindersDisabled?: boolean;
  /**
   * Claude-only session axes delivered through the CLI's `--settings`
   * block. None of them has a CLI flag — the settings key IS the delivery
   * mechanism — and all three are read at spawn, so edits reach new
   * sessions only. claude-tui is deliberately excluded on the backend:
   * its PTY launch passes no `--settings` at all.
   *
   * Every one treats its empty/zero value as "say nothing, let Claude
   * Code decide", never as a value of its own. Go persists sparsely, so
   * treat undefined the same way.
   */
  claudeOutputStyle?: ClaudeOutputStyle | '';
  claudeSubagentLimits?: ClaudeSubagentLimits;
  claudeToolMemoryLimit?: string;
  /**
   * The peer inbox. Rides the same `--settings` block for its policy half
   * but is NOT one of the three above: it also needs an environment gate and
   * a `--name` at spawn, and unlike them the backend reconciles a change
   * onto running sessions as a deferred restart instead of leaving it for
   * whenever the next session happens to start.
   */
  claudeCrossSession?: ClaudeCrossSession;
  /**
   * Extended thinking. Unlike the four axes above this one is NOT
   * spawn-only: the backend applies a change to running headless Claude
   * sessions with a `set_max_thinking_tokens` control request. The one
   * exception is going BACK to Claude Code's own choice, which has no wire
   * form and lands on the deferred-restart path.
   */
  claudeThinking?: ClaudeThinking;
  /** Seeds whether new draft threads start on the current checkout or a new worktree. */
  defaultThreadEnvMode: ThreadEnvMode;
  /** Prefix used for auto-generated worktree branch names. */
  worktreeBranchPrefix: string;
  /** Minimum pane width preset used by the workspace pane host. */
  paneDensity: PaneDensityMode;
  activityRunDefault: ActivityRunDefaultMode;
  activityRunWindowRows: number;
  /**
   * Text generation: which CLI writes commit messages / PR bodies /
   * thread titles. Independent of the chat provider so a user on Claude
   * can still use Codex for commit messages.
   */
  textGenerationProvider: ProviderID;
  /**
   * Text generation model id. Empty string = "use the per-provider
   * default" (codex → gpt-5.6-luna, claude → claude-haiku-4-5).
   */
  textGenerationModel: string;
  /** Text generation reasoning-effort tier. Defaults to "low" — these
   * calls prioritise speed over depth. */
  textGenerationReasoningEffort: ReasoningEffort;
  /**
   * Writing style for generated commit messages: Conventional Commits
   * (default), match the repo's recent commit subjects, or follow the
   * user's custom instructions.
   */
  commitMessageStyle: CommitMessageStyle;
  /** Free-text style instructions used when commitMessageStyle === "custom". */
  commitMessageStyleCustom: string;
  /**
   * Per-provider auto-compact thresholds (percent of the active context
   * window). Each provider has a standard and extended tier; the slider
   * never produces 0, so the value is always 1..90. Per-thread overrides
   * (Thread.autoCompactStandardPercent / …Extended) win when set.
   */
  claudeAutoCompactStandardPercent: number;
  claudeAutoCompactExtendedPercent: number;
  codexAutoCompactStandardPercent: number;
  codexAutoCompactExtendedPercent: number;
  observabilityTracingEnabled: boolean;
  observabilityOtlpEndpoint: string;
  observabilityEventLogEnabled: boolean;
  /**
   * Phase E LAN-bind preferences. Persisted alongside the rest of the
   * settings so a re-launch reapplies the toggle. Mirrors the Go
   * settings.NetworkSettings shape.
   */
  network: NetworkPersistedSettings;

  /**
   * Retention TTL window for the background cleanup sweep. Threads
   * whose updated_at is older than retention.days days are removed
   * along with their on-disk side effects (attachments and replay logs).
   * Dated provider-event log files and bug-report bookmark files are
   * pruned on the same cutoff. 0 disables the sweep entirely.
   */
  retention: RetentionPersistedSettings;

  /**
   * Periodic background `git fetch` for the repositories behind the
   * sidebar's projects, so ahead/behind counts track the remote instead
   * of freezing at the last manual fetch. Default true; one fetch per
   * repository per ~5 minutes, origin only, never --prune.
   */
  backgroundGitFetch: boolean;

  /**
   * Self-hosted GitLab hostnames (bare hosts, e.g.
   * "gitlab.mycompany.com"). Origin URLs whose host matches an entry
   * classify as the "gitlab" forge in addition to the literal
   * gitlab.com match. Empty / undefined means "no self-hosted hosts".
   */
  gitlabSelfHostedHosts: string[];

  /**
   * Phase F --connect target list. Optional in the wire shape because
   * the Go side persists with `omitempty` — fresh installs have no
   * remoteEndpoints key and TS callers should treat undefined as the
   * empty list.
   */
  remoteEndpoints?: RemoteEndpointPersisted[];

  /** Sidebar project sort order. Persisted in Go settings for cross-restart durability. */
  projectSortMode: ProjectSortMode;

  /**
   * Usage-surface time period (sidebar usage footer + usage modal):
   * 'day' | 'week' | 'month' | 'all'. Persisted in Go settings because
   * webview localStorage is not durable (the transport's ephemeral
   * port changes the origin every launch).
   *
   * Per-client view state (pane layout, collapsed projects, sidebar
   * width, …) deliberately does NOT live in Settings — it persists
   * through stores/appStorage.ts (the ui_state table) keyed per
   * client, so two clients of the same backend keep independent view
   * state.
   */
  usagePeriod: string;

  /**
   * Global workflow kill switch. While paused no phase starts anywhere; live
   * turns finish and their runs rest at the next phase boundary.
   */
  workflowPaused: boolean;
}

export interface NetworkPersistedSettings {
  /** When true, the transport server binds to 0.0.0.0 instead of 127.0.0.1. */
  bindAll: boolean;

  /**
   * The port the transport binds, or 0 for automatic — which is an
   * ephemeral port on first launch, reused from a cache after that. A
   * non-zero value is the operator naming the port this install owns.
   */
  listenPort: number;

  /**
   * The one HTTPS name this backend answers to: a bare hostname, no
   * scheme, no port, no path. Empty means the backend is reached by
   * address only.
   */
  canonicalDomain: string;

  /**
   * argv of the command that publishes and removes the DNS-01 challenge
   * record. Run as `<argv...> set|clear <fqdn> <value>`. Empty means the
   * backend never orders a certificate.
   */
  acmeDnsHook: string[];

  /**
   * Absolute paths to a certificate this backend did not obtain. Both or
   * neither; the pair is served for the canonical domain and stops the
   * backend ordering one of its own.
   */
  externalCertFile: string;
  externalKeyFile: string;

  /**
   * When true, this backend joins the owner's tailnet as its own node, so
   * it is reachable from their other devices with no public listener.
   */
  tailnetEnabled: boolean;

  /**
   * The coordination server the node registers with. Empty means the
   * Tailscale service; a self-hosted control plane is why it is settable.
   */
  tailnetControlUrl: string;
}

export interface RetentionPersistedSettings {
  /** Age threshold in days. 0 disables the sweep. */
  days: number;
}

/**
 * RemoteEndpointPersisted mirrors settings.RemoteEndpoint from the Go
 * side. The `lastUsedAt` field is a Unix-seconds timestamp, omitted
 * when the endpoint has never been used.
 */
export interface RemoteEndpointPersisted {
  id: string;
  name: string;
  url: string;
  token: string;
  lastUsedAt?: number;
}

export interface ProviderStatus {
  provider: string;
  installed: boolean;
  version: string;
  binaryPath: string;
  status: "ready" | "not_found" | "error";
  message: string;
}

// The provider-declared service tier a fast-mode turn runs on, mirroring
// provider.FastModeTier. Codex reports it per model; Claude has no tier concept
// and omits it. `capabilities` including "fast_mode" stays the support gate —
// this only names the tier, so a rename upstream moves the label instead of
// breaking the toggle. Absent on Claude models and on stale cached catalogs,
// where the UI falls back to its "Fast" literals.
export interface FastModeTier {
  id: string;
  name?: string;
  description?: string;
}

export interface ModelInfo {
  slug: string;
  name: string;
  provider: string;
  isCustom?: boolean;
  capabilities?: string[];
  fastModeTier?: FastModeTier;
  contextWindows?: ContextWindowOption[];
  reasoningEfforts?: ReasoningEffortOption[];
  // Three-state, mirroring provider.ModelInfo.SupportsAutoMode: true/false
  // are the CLI's own answer, absent means nobody said. Consumers may
  // restrict the Auto tier ONLY on an explicit false — treating absence as
  // denial would disable a working mode for every model the wire doesn't
  // list. See internal/claudemodels/AGENTS.md.
  supportsAutoMode?: boolean | null;
}

export interface ContextWindowOption {
  tokens: number;
  label: string;
  tier: "standard" | "extended" | string;
  // Marks the tier a new thread starts on, mirroring provider.ContextWindowOption
  // (and the generated binding, which has carried it since the 1M-default
  // change). Optional because the wire omits it on non-default tiers.
  default?: boolean;
}

export interface ReasoningEffortOption {
  slug: ReasoningEffort;
  label: string;
  default?: boolean;
}
