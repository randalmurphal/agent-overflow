/**
 * ReasoningEffort mirrors provider.ReasoningEffort's five tiers.
 * Claude exposes all five; Codex omits "max". Settings store keeps
 * the full set so a user who switches provider doesn't lose their
 * preference.
 */
export type ReasoningEffort = 'low' | 'medium' | 'high' | 'xhigh' | 'max';

/**
 * ContextWindow mirrors the schema-legal per-thread context-window
 * values. 200000 is Claude's default; 1000000 is the extended-beta
 * tier. Codex ignores the field at the translation layer.
 */
export type ContextWindow = 200000 | 1000000;

/**
 * ThreadMode mirrors the canonical mode column on thread rows. Discussion
 * is included for symmetry even though it's reached via a separate flow.
 */
export type ThreadMode = 'chat' | 'plan' | 'design' | 'discussion';

export type RuntimeMode = 'approval-required' | 'auto-accept-edits' | 'full-access';

export type ThreadEnvMode = 'local' | 'worktree';

export interface Settings {
  theme: 'system' | 'light' | 'dark';
  timestampFormat: 'locale' | '12-hour' | '24-hour';
  defaultProvider: 'claude' | 'codex';
  defaultModelClaude: string;
  defaultModelCodex: string;
  modelContextWindows: Record<string, ContextWindow>;
  recentWorkspaces: string[];
  diffWordWrap: boolean;
  showEndOfTurnDiffs: boolean;
  backgroundTrayExpanded: boolean;
  streamingEnabled: boolean;
  confirmArchive: boolean;
  confirmDelete: boolean;
  claudeBinaryPath: string;
  codexBinaryPath: string;
  claudeEnabled: boolean;
  codexEnabled: boolean;
  /** Legacy setting kept for old settings files; normal new threads start in chat mode. */
  defaultMode: ThreadMode;
  /** Seeds the per-thread runtime permissions mode. */
  defaultRuntimeMode: RuntimeMode;
  /** Seeds whether new draft threads start on the current checkout or a new worktree. */
  defaultThreadEnvMode: ThreadEnvMode;
  /** Prefix used for auto-generated worktree branch names. */
  worktreeBranchPrefix: string;
  /** Seeds the per-thread reasoning-effort tier. */
  defaultReasoningEffort: ReasoningEffort;
  /** Seeds the per-thread fast-mode toggle. */
  defaultFastMode: boolean;
  /** Seeds the per-thread context-window preference. */
  defaultContextWindow: ContextWindow;
  /**
   * Text generation: which CLI writes commit messages / PR bodies /
   * thread titles. Independent of the chat provider so a user on Claude
   * can still use Codex for commit messages.
   */
  textGenerationProvider: 'claude' | 'codex';
  /**
   * Text generation model id. Empty string = "use the per-provider
   * default" (codex → gpt-5.4-mini, claude → claude-haiku-4-5).
   */
  textGenerationModel: string;
  /** Text generation reasoning-effort tier. Defaults to "low" — these
   * calls prioritise speed over depth. */
  textGenerationReasoningEffort: ReasoningEffort;
  observabilityTracingEnabled: boolean;
  observabilityOtlpEndpoint: string;
  observabilityEventLogEnabled: boolean;
}

export interface ProviderStatus {
  provider: string;
  installed: boolean;
  version: string;
  binaryPath: string;
  status: 'ready' | 'not_found' | 'error';
  message: string;
}

export interface ModelInfo {
  slug: string;
  name: string;
  provider: string;
  capabilities: string[];
}
