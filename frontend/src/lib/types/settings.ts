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
  | "max";

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
export type ThreadMode = "chat" | "plan" | "design" | "discussion";

export type RuntimeMode =
  | "approval-required"
  | "auto-accept-edits"
  | "full-access";

export type ThreadEnvMode = "local" | "worktree";

export type SansFont = "geist" | "hack-nerd" | "system";
export type MonoFont = "geist" | "hack-nerd" | "system";
export type PaneDensityMode = "compact" | "comfortable" | "spacious";
export type ProjectSortMode = "lastActivity" | "createdAt" | "manual";

export interface PaneLayoutPersistedPane {
  paneId: string;
  threadId: string;
  ratio: number;
}

export interface PaneLayoutPersistedSettings {
  version: 1;
  panes: PaneLayoutPersistedPane[];
  focusedPaneId?: string | null;
}

export interface Settings {
  theme: "system" | "light" | "dark";
  timestampFormat: "locale" | "12-hour" | "24-hour";
  /**
   * Sans typeface for the `--font-sans` CSS variable. Default `geist`
   * is eagerly bundled; `hack-nerd` lazy-loads a separate woff2 chunk;
   * `system` uses the OS fallback chain.
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
  streamingEnabled: boolean;
  confirmArchive: boolean;
  confirmDelete: boolean;
  claudeBinaryPath: string;
  codexBinaryPath: string;
  claudeEnabled: boolean;
  codexEnabled: boolean;
  /** Seeds whether new draft threads start on the current checkout or a new worktree. */
  defaultThreadEnvMode: ThreadEnvMode;
  /** Prefix used for auto-generated worktree branch names. */
  worktreeBranchPrefix: string;
  /** Minimum pane width preset used by the workspace pane host. */
  paneDensity: PaneDensityMode;
  /**
   * Text generation: which CLI writes commit messages / PR bodies /
   * thread titles. Independent of the chat provider so a user on Claude
   * can still use Codex for commit messages.
   */
  textGenerationProvider: ProviderID;
  /**
   * Text generation model id. Empty string = "use the per-provider
   * default" (codex → gpt-5.4-mini, claude → claude-haiku-4-5).
   */
  textGenerationModel: string;
  /** Text generation reasoning-effort tier. Defaults to "low" — these
   * calls prioritise speed over depth. */
  textGenerationReasoningEffort: ReasoningEffort;
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
   * along with their on-disk side effects (attachments, design
   * workdirs, replay logs, checkpoint git refs in the user's repo).
   * Dated provider-event log files and bug-report bookmark files are
   * pruned on the same cutoff. 0 disables the sweep entirely.
   */
  retention: RetentionPersistedSettings;

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
  /** Project IDs explicitly collapsed in the sidebar. Absent IDs default to expanded.
   * Go uses `omitempty`, so the key may be absent on the wire — treat undefined as [].
   */
  collapsedProjects?: string[];

  /**
   * Visible thread pane order, ratios, and focus. Stored in Go settings
   * instead of webview localStorage because packaged webviews are not
   * durable on every platform.
   */
  paneLayout: PaneLayoutPersistedSettings;
}

export interface NetworkPersistedSettings {
  /** When true, the transport server binds to 0.0.0.0 instead of 127.0.0.1. */
  bindAll: boolean;
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

export interface ModelInfo {
  slug: string;
  name: string;
  provider: string;
  isCustom?: boolean;
  capabilities?: string[];
  contextWindows?: ContextWindowOption[];
  reasoningEfforts?: ReasoningEffortOption[];
}

export interface ContextWindowOption {
  tokens: number;
  label: string;
  tier: "standard" | "extended" | string;
}

export interface ReasoningEffortOption {
  slug: ReasoningEffort;
  label: string;
  default?: boolean;
}
