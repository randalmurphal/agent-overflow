// The settings search index: every control a user might look for, anchored
// to the page it lives on.
//
// Every `SettingsField` takes a `SettingsFieldId` from this list and stamps it
// on its root as `data-settings-field`; blocks that are not a plain labelled
// row (radio groups, the accounts list, the auto-compact sliders) stamp the
// same attribute themselves. Search matches against this list and reveals
// the match by that attribute, so a control is findable exactly when it is
// registered here.
//
// Labels here are canonical. `fields.test.ts` mounts every page and fails
// when a registered id does not render, when a rendered anchor is not
// registered, or when a rendered label differs from the one listed — so the
// index cannot silently drift from the pages. A `conditional` entry is one
// the default fixture cannot show (a platform-gated block, a control behind
// another setting); the test tolerates its absence but still checks the
// label when it is present.
//
// `hint` is the rendered hint where it is static — it carries the words a
// user actually searches for ("sleep", "dark") — and `keywords` covers what
// neither label nor hint says, or replaces the hint where the rendered one
// is computed from state.

import type { SettingsSection } from './sections';
import { providerLabel } from '../../stores/providerAccountLabels';

export interface SettingsFieldDef {
  readonly id: string;
  readonly section: SettingsSection;
  /** The in-page section header the control sits under, for the breadcrumb. */
  readonly heading?: string;
  readonly label: string;
  readonly hint?: string;
  readonly keywords?: readonly string[];
  readonly conditional?: boolean;
}

/** The providers that get a settings page each. Mirrors `PROVIDER_SETTINGS_ORDER`. */
export const SETTINGS_PROVIDERS = ['claude', 'codex'] as const;

export type SettingsProvider = (typeof SETTINGS_PROVIDERS)[number];

const PROVIDER_LABELS: Record<SettingsProvider, string> = {
  claude: 'Claude',
  codex: 'Codex',
};

/**
 * Controls that every provider page renders from the same shared component,
 * so their ids are `<provider>.<slug>` and typed as such. Provider-specific
 * controls (Claude's session axes, the two differently-worded tool editors)
 * are ordinary static entries below.
 */
const PROVIDER_FIELDS = [
  {
    slug: 'enabled',
    heading: 'Setup',
    label: 'Enabled',
    keywords: ['allow', 'new threads', 'provider', 'turn on', 'turn off'],
  },
  {
    slug: 'binary-path',
    heading: 'Setup',
    label: 'Binary path',
    hint: 'Override the auto-detected CLI binary.',
    keywords: ['cli', 'executable', 'install'],
  },
  {
    slug: 'models',
    heading: 'Setup',
    label: 'Available models',
    hint: 'Click a model to show or hide it in model pickers.',
    keywords: ['hide', 'picker', 'catalog'],
  },
  {
    slug: 'env',
    heading: 'Environment',
    label: 'Environment variables',
    keywords: ['env', 'process', 'secret', 'api key'],
  },
  {
    slug: 'accounts',
    heading: 'Accounts',
    label: 'Accounts',
    keywords: ['login', 'sign in', 'switch', 'usage', 'limits', 'rate limit'],
  },
  {
    slug: 'auto-compact',
    heading: 'Context window',
    label: 'Auto-compact',
    hint: 'Trigger compaction at this percent of the live context window.',
    keywords: ['context', 'compaction', 'threshold', 'standard window', 'extended window'],
  },
  {
    slug: 'system-prompt',
    heading: 'System prompt',
    label: 'System prompt overrides',
    hint: "Replaces the provider's default system prompt. The first enabled entry whose models include the session's model wins.",
    keywords: ['instructions', 'prompt override', 'placeholder'],
  },
] as const;

type ProviderFieldSlug = (typeof PROVIDER_FIELDS)[number]['slug'];

export type ProviderFieldId = `${SettingsProvider}.${ProviderFieldSlug}`;

/** Builds the typed id for a shared provider control. */
export function providerFieldId(provider: SettingsProvider, slug: ProviderFieldSlug): ProviderFieldId {
  return `${provider}.${slug}`;
}

const STATIC_FIELDS = [
  // --- Theme --------------------------------------------------------------
  {
    id: 'theme.mode',
    section: 'theme',
    label: 'Mode',
    hint: 'Choose your preferred color scheme.',
    keywords: ['dark', 'light', 'system', 'color scheme'],
  },
  {
    id: 'theme.interface',
    section: 'theme',
    label: 'Interface theme',
    hint: 'Surfaces, text, borders, accent and status colors.',
    keywords: ['ui theme', 'colors', 'palette'],
  },
  {
    id: 'theme.code',
    section: 'theme',
    label: 'Code theme',
    hint: 'Syntax highlighting, ANSI output and the grounds behind code blocks and the terminal.',
    keywords: ['syntax', 'highlight', 'terminal colors'],
  },

  {
    id: 'theme.copy-files', section: 'theme', label: 'Copy themes',
    hint: 'Replace this device’s custom files with a copy from a computer. Your selections stay the same.',
    keywords: ['import', 'custom', 'offline', 'phone', 'files'], conditional: true,
  },

  // --- Typography ---------------------------------------------------------
  {
    id: 'typography.ui-font',
    section: 'typography',
    label: 'UI font',
    hint: 'Typeface for general UI text. Hack Nerd Font lazy-loads on first use.',
    keywords: ['sans', 'typeface', 'font family'],
  },
  {
    id: 'typography.code-font',
    section: 'typography',
    label: 'Code font',
    hint: 'Typeface for code, diffs, and command output.',
    keywords: ['mono', 'monospace', 'typeface', 'font family'],
  },
  {
    id: 'typography.font-size',
    section: 'typography',
    label: 'Font size',
    hint: 'Base text size in pixels. Scales the entire UI.',
    keywords: ['zoom', 'scale', 'text size', 'bigger', 'smaller'],
  },

  // --- Chat ---------------------------------------------------------------
  {
    id: 'chat.timestamp-format',
    section: 'chat',
    heading: 'Messages',
    label: 'Timestamp format',
    hint: 'How timestamps appear in the chat.',
    keywords: ['time', 'clock', '12-hour', '24-hour', 'locale'],
  },
  {
    id: 'chat.diff-word-wrap',
    section: 'chat',
    heading: 'Messages',
    label: 'Diff word wrap',
    hint: 'Wrap long lines in diff views.',
    keywords: ['wrapping', 'long lines', 'horizontal scroll'],
  },
  {
    id: 'chat.collapse-diff-previews',
    section: 'chat',
    heading: 'Messages',
    label: 'Collapse diff previews',
    hint: 'Show file edits collapsed by default; expand a row to reveal the diff preview.',
    keywords: ['file edits', 'expand', 'fold'],
  },
  {
    id: 'chat.pane-density',
    section: 'chat',
    heading: 'Pane density',
    label: 'Pane density',
    hint: 'Choose the minimum width each workspace pane keeps before horizontal scrolling starts.',
    keywords: ['pane width', 'compact', 'comfortable', 'columns', 'layout'],
  },
  {
    id: 'chat.activity-run-default',
    section: 'chat',
    heading: 'Activity runs',
    label: 'Activity runs',
    hint: "Consecutive tool calls and thinking are grouped into one run so a long stretch of activity can't push the conversation off screen.",
    keywords: ['tool calls', 'thinking', 'collapsed', 'expanded', 'grouping'],
  },
  {
    id: 'chat.activity-run-window-rows',
    section: 'chat',
    heading: 'Activity runs',
    label: 'Rows kept mounted',
    hint: "How many of a run's newest rows stay rendered. Scrolling to the top of a run loads the next chunk of older rows.",
    keywords: ['window', 'virtualization', 'memory'],
  },

  // --- Working indicator --------------------------------------------------
  {
    id: 'spinner.verbs',
    section: 'spinner',
    label: 'Spinner verbs',
    hint: 'One verb per turn in place of "Working", from Claude Code’s list plus yours.',
    keywords: ['working', 'status text', 'thinking words'],
  },
  {
    id: 'spinner.builtin-verbs',
    section: 'spinner',
    label: 'Built-in verbs',
    hint: 'Draw from the 186 verbs Claude Code ships. Off uses only your own.',
  },
  {
    id: 'spinner.custom-verbs',
    section: 'spinner',
    label: 'Custom verbs',
    hint: 'One per line. Added to the draw.',
    keywords: ['own words'],
  },
  {
    id: 'spinner.animated',
    section: 'spinner',
    label: 'Animated spinner',
    hint: 'A sprite in place of the LED chase, random per turn from the pool below.',
    keywords: ['emoji', 'animation', 'sprite', 'led'],
  },
  {
    id: 'spinner.pool',
    section: 'spinner',
    label: 'Pool',
    hint: 'Checked animations are in the per-turn draw.',
    keywords: ['sprites', 'animations'],
    conditional: true,
  },
  {
    id: 'spinner.compaction-animation',
    section: 'spinner',
    label: 'Compaction animation',
    hint: "Shown while the provider compacts the thread's context.",
    keywords: ['compact', 'context'],
    conditional: true,
  },

  {
    id: 'spinner.copy-files', section: 'spinner', label: 'Copy animations',
    hint: 'Replace this device’s custom files with a copy from a computer. Your selections stay the same.',
    keywords: ['import', 'custom', 'offline', 'phone', 'files'], conditional: true,
  },

  // --- Threads ------------------------------------------------------------
  {
    id: 'threads.default-environment',
    section: 'threads',
    heading: 'New threads',
    label: 'Default environment',
    hint: 'Workspace mode seeded on new draft threads.',
    keywords: ['worktree', 'checkout', 'workspace', 'draft'],
  },
  {
    id: 'threads.auto-pin',
    section: 'threads',
    heading: 'New threads',
    label: 'Auto-pin new threads',
    hint: 'Put a new thread on the front burner after its first message is sent.',
    keywords: ['pin', 'front burner', 'sidebar'],
  },
  {
    id: 'threads.confirm-archive',
    section: 'threads',
    heading: 'Safety checks',
    label: 'Confirm before archive',
    hint: 'Show a confirmation dialog when archiving threads.',
    keywords: ['dialog', 'prompt', 'archive'],
  },
  {
    id: 'threads.confirm-delete',
    section: 'threads',
    heading: 'Safety checks',
    label: 'Confirm before delete',
    hint: 'Show a confirmation dialog when deleting threads.',
    keywords: ['dialog', 'prompt', 'delete'],
  },

  // --- Performance --------------------------------------------------------
  {
    id: 'performance.streaming',
    section: 'performance',
    label: 'Streaming enabled',
    hint: "Show text live as it arrives. When off, each block appears only once it's complete.",
    keywords: ['live', 'typing', 'incremental'],
  },
  {
    id: 'performance.low-power-mode',
    section: 'performance',
    label: 'Low power mode',
    hint: 'Minimize rendering work: instant scroll placement, chunked text reveal, static working indicator. For weaker machines or when running GPU-heavy apps alongside.',
    keywords: ['gpu', 'battery', 'lag', 'slow machine', 'reduce motion'],
  },
  {
    id: 'performance.keep-awake-screen',
    section: 'performance',
    label: 'Keep-awake includes screen',
    hint: 'When keep-awake is on (the sun toggle in the sidebar), also keep the display from sleeping. Off: the machine stays awake but the screen may turn off.',
    keywords: ['sleep', 'display', 'caffeine', 'monitor', 'idle'],
  },

  // --- Claude Code (Claude-only controls) ---------------------------------
  {
    id: 'claude.tui',
    section: 'claude',
    heading: 'Setup',
    label: 'Claude TUI',
    keywords: ['terminal', 'interactive', 'pty'],
  },
  {
    id: 'claude.disabled-tools',
    section: 'claude',
    heading: 'Tools',
    label: 'Disabled tools',
    hint: "Their schemas never reach the model. Names are passed to the Claude CLI verbatim, so a name it doesn't recognise is harmless.",
    keywords: ['turn off tools', 'schema', 'context'],
  },
  {
    id: 'claude.todo-tools',
    section: 'claude',
    heading: 'Tools',
    label: 'Todo tools',
    keywords: ['task list', 'nudges', 'reminders', 'todo'],
  },
  {
    id: 'claude.output-style',
    section: 'claude',
    heading: 'Session',
    label: 'Output style',
    hint: "Replaces Claude Code's response style for every session.",
    keywords: ['concise', 'explanatory', 'learning', 'verbosity'],
  },
  {
    id: 'claude.subagent-limits',
    section: 'claude',
    heading: 'Session',
    label: 'Subagent limits',
    hint: 'How deep subagents may spawn further subagents, and how many may run at once. 0 leaves each to Claude Code.',
    keywords: ['depth', 'concurrency', 'agents', 'parallel'],
  },
  {
    id: 'claude.thinking',
    section: 'claude',
    heading: 'Session',
    label: 'Thinking',
    hint: 'How much Claude thinks before answering. A fixed budget only binds on models that take an explicit budget — on models with adaptive thinking, Claude keeps deciding for itself.',
    keywords: ['reasoning', 'budget', 'effort', 'extended thinking'],
  },
  {
    id: 'claude.show-thinking',
    section: 'claude',
    heading: 'Session',
    label: 'Show thinking',
    hint: "Whether Claude's thinking text reaches the thread. Claude thinks either way — this only decides whether you see it.",
    keywords: ['reasoning', 'hide thinking', 'display'],
  },
  {
    id: 'claude.tool-memory-limit',
    section: 'claude',
    heading: 'Session',
    label: 'Tool memory limit',
    hint: "Caps memory for the processes Claude's tools spawn — a size like 4G, or none to lift it. Applies only when the backend runs on Linux (WSL included); it is a memory cgroup, which macOS and native Windows have no equivalent for.",
    keywords: ['cgroup', 'ram', 'oom', 'bash'],
  },
  {
    id: 'claude.cross-session',
    section: 'claude',
    heading: 'Cross-session messaging',
    label: 'Let Claude sessions message each other',
    hint: 'Claude Code can list the other Claude sessions running on this machine and send them messages. Turning this on lists these threads too, so any program running as you on this machine can start a turn in an idle thread.',
    keywords: ['inter-session', 'messaging', 'discover'],
  },
  {
    id: 'claude.cross-session-inbound',
    section: 'claude',
    heading: 'Cross-session messaging',
    label: 'Incoming messages',
    hint: 'What happens when another session sends one of these threads a message.',
    keywords: ['inbound', 'queue', 'start turn'],
    conditional: true,
  },

  // --- Codex (Codex-only controls) ----------------------------------------
  {
    id: 'codex.builtin-tools',
    section: 'codex',
    heading: 'Tools',
    label: 'Built-in tools',
    hint: "Turn one off to keep its schema out of the model's context. Shell and file editing are not offered — a session cannot work without them.",
    keywords: ['disable tools', 'web search', 'schema'],
  },

  // --- Commit messages ----------------------------------------------------
  {
    id: 'commit-messages.provider',
    section: 'commit-messages',
    label: 'Provider',
    hint: 'CLI that generates non-chat text.',
    keywords: ['text generation', 'titles', 'pr body'],
  },
  {
    id: 'commit-messages.model',
    section: 'commit-messages',
    label: 'Model',
    hint: "Leave empty to use the provider's default small-text model.",
  },
  {
    id: 'commit-messages.reasoning-effort',
    section: 'commit-messages',
    label: 'Reasoning effort',
    hint: 'Budget for commit/PR text generation.',
  },
  {
    id: 'commit-messages.style',
    section: 'commit-messages',
    label: 'Commit message style',
    hint: 'Phrasing guidance for generated commit messages.',
    keywords: ['conventional commits', 'repo history'],
  },
  {
    id: 'commit-messages.style-instructions',
    section: 'commit-messages',
    label: 'Style instructions',
    hint: 'Free-text rules the generated subject and body should follow.',
    conditional: true,
  },

  // --- Browser ------------------------------------------------------------
  {
    id: 'browser.enabled',
    section: 'browser',
    label: 'Built-in browser tools',
    hint: 'Give Claude and Codex a browser in a companion pane.',
    keywords: ['web', 'playwright', 'companion pane'],
  },
  {
    id: 'browser.persist-site-data',
    section: 'browser',
    label: 'Remember site data',
    hint: 'Keep encrypted cookies and local storage separately for each workspace.',
    keywords: ['cookies', 'login', 'sessions'],
  },
  {
    id: 'browser.outside-workspace',
    section: 'browser',
    label: 'Files outside workspace',
    hint: 'Allow browser tools to open any regular file your OS account can read.',
    keywords: ['file://', 'local files', 'sandbox'],
  },
  {
    id: 'browser.chromium-path',
    section: 'browser',
    label: 'Chromium path',
    hint: 'Only used by a serve host, which launches its own browser. Must be an absolute path.',
    keywords: ['headless', 'chrome', 'binary', 'serve'],
  },
  {
    id: 'browser.clear-site-data',
    section: 'browser',
    heading: 'Site data',
    label: 'Clear site data',
    hint: 'Closes open browser pages and removes saved cookies and local storage.',
    keywords: ['cookies', 'reset', 'wipe'],
  },

  // --- Projects -----------------------------------------------------------
  {
    id: 'projects.project',
    section: 'projects',
    label: 'Project',
    hint: 'Choose which project these settings apply to.',
  },
  {
    id: 'projects.worktree-setup',
    section: 'projects',
    label: 'Worktree setup',
    hint: 'Runs whenever this project creates a new worktree. Commands run from the worktree root with AO_PROJECT_ROOT and AO_WORKTREE_PATH set to the two checkouts.',
    keywords: ['copy', 'commands', 'bootstrap', 'node_modules', '.env'],
    conditional: true,
  },

  // --- Git ----------------------------------------------------------------
  {
    id: 'git.background-fetch',
    section: 'git',
    heading: 'Repository sync',
    label: 'Fetch remotes in the background',
    keywords: ['git fetch', 'ahead', 'behind', 'origin', 'remote'],
  },
  {
    id: 'git.worktree-branch-prefix',
    section: 'git',
    heading: 'Worktrees',
    label: 'Worktree branch prefix',
    hint: 'Prefix for generated worktree branches.',
    keywords: ['branch name', 'naming'],
  },
  {
    id: 'git.gitlab-hosts',
    section: 'git',
    heading: 'Self-hosted GitLab hosts',
    label: 'Self-hosted GitLab hosts',
    hint: "Treated as GitLab when classifying a repo's origin remote, enabling Create MR, MR labels, and the `glab` CLI. gitlab.com and github.com are automatic.",
    keywords: ['merge request', 'glab', 'self-hosted'],
  },

  // --- Editor -------------------------------------------------------------
  {
    id: 'editor.preferred',
    section: 'editor',
    label: 'Preferred editor',
    keywords: ['vs code', 'cursor', 'zed', 'open file', '$EDITOR'],
    conditional: true,
  },

  // --- Remote access ------------------------------------------------------
  {
    id: 'remote.allow-remote-access',
    section: 'remote',
    heading: 'Local network',
    label: 'Allow LAN connections',
    hint: 'Allow paired devices on your Wi-Fi or wired network.',
    keywords: ['lan', 'bind', 'listen', 'phone', 'tablet', 'network'],
    conditional: true,
  },
  {
    id: 'remote.port',
    section: 'remote',
    heading: 'Network binding',
    label: 'Port',
    hint: 'The port this backend listens on. Leave it blank to let Agent Overflow pick one and keep reusing it.',
    keywords: ['listen', 'tcp', 'firewall', 'fixed port'],
    conditional: true,
  },
  {
    id: 'remote.share-url',
    section: 'remote',
    heading: 'Share URL',
    label: 'Share URL',
    keywords: ['link', 'token', 'qr', 'connect from another device'],
    conditional: true,
  },
  {
    id: 'remote.wsl-distro',
    section: 'remote',
    heading: 'WSL distro',
    label: 'WSL distro',
    keywords: ['windows', 'ubuntu', 'distribution', 'launcher'],
    conditional: true,
  },
  {
    id: 'remote.domain',
    section: 'remote',
    heading: 'Domain and HTTPS',
    label: 'Domain',
    hint: 'A bare hostname such as ao.example.com. No scheme, no port, no path. Leave it blank to reach this backend by address only.',
    keywords: ['hostname', 'https', 'tls', 'certificate', 'lets encrypt', 'dns'],
    conditional: true,
  },
  {
    id: 'remote.dns-hook',
    section: 'remote',
    heading: 'Domain and HTTPS',
    label: 'DNS update command',
    hint: "Publishes the record Let's Encrypt checks. Agent Overflow runs it with set or clear, the record name, and the value appended. There is no shell, so write sh -c '…' if you need one. Leave it blank if you are not using Let's Encrypt.",
    keywords: ['acme', 'dns-01', 'txt record', 'lets encrypt', 'hook', 'script'],
    conditional: true,
  },
  {
    id: 'remote.certificate-file',
    section: 'remote',
    heading: 'Domain and HTTPS',
    label: 'Certificate file',
    hint: 'Absolute path to a PEM certificate you already have. Fill this and the key to serve them instead of obtaining a certificate.',
    keywords: ['pem', 'tls', 'https', 'cert', 'fullchain'],
    conditional: true,
  },
  {
    id: 'remote.private-key-file',
    section: 'remote',
    heading: 'Domain and HTTPS',
    label: 'Private key file',
    hint: 'Absolute path to the matching PEM private key.',
    keywords: ['pem', 'tls', 'https', 'privkey'],
    conditional: true,
  },
  {
    id: 'remote.tailnet',
    section: 'remote',
    heading: 'Tailscale',
    label: 'Join my tailnet',
    hint: 'The first time you turn this on, you approve this machine in your browser.',
    keywords: ['tailscale', 'headscale', 'vpn', 'wireguard', 'magicdns'],
    conditional: true,
  },
  {
    id: 'remote.tailnet-control-url',
    section: 'remote',
    heading: 'Tailscale',
    label: 'Coordination server',
    hint: 'Leave blank for Tailscale. Set it only if you run your own coordination server.',
    keywords: ['headscale', 'tailscale', 'control url', 'self-hosted'],
    conditional: true,
  },
  {
    id: 'remote.preview-port-add',
    section: 'remote',
    heading: 'Preview ports',
    label: 'Share a port',
    keywords: ['dev server', 'preview', 'localhost', 'vite', 'forward', 'phone'],
    conditional: true,
  },

  // --- Notifications ------------------------------------------------------
  {
    id: 'notifications.enabled',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Desktop notifications',
    hint: 'Off silences every kind on this screen, including workflow and update notices.',
    keywords: ['alerts', 'os notification', 'silence', 'mute', 'sound'],
  },
  {
    id: 'notifications.turn-complete',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Turn complete',
    hint: 'When the agent finishes a turn and the thread is waiting on you.',
    keywords: ['finished', 'done', 'idle', 'waiting'],
    conditional: true,
  },
  {
    id: 'notifications.approval-needed',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Approval needed',
    hint: 'When the agent is blocked asking permission to use a tool.',
    keywords: ['permission', 'tool', 'blocked', 'allow'],
    conditional: true,
  },
  {
    id: 'notifications.errors',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Errors',
    hint: 'When a turn fails, or a provider stops while a thread is using it.',
    keywords: ['failed', 'crash', 'provider exited'],
    conditional: true,
  },
  {
    id: 'notifications.provider-signed-out',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Provider signed out',
    hint: "When a provider's login is gone and nothing will run until you sign in again.",
    keywords: ['login', 'sign in', 'credentials', 'expired', 'logged out'],
    conditional: true,
  },
  {
    id: 'notifications.workflow-attention',
    section: 'notifications',
    heading: 'Notifications',
    label: 'Workflow needs attention',
    hint: 'When a workflow item is waiting on a person, or failed.',
    keywords: ['workflow', 'run', 'parked', 'gate', 'attention'],
    conditional: true,
  },
  {
    id: 'notifications.app-update',
    section: 'notifications',
    heading: 'Notifications',
    label: 'App update notices',
    hint: 'When an update did not apply and the app needs a hand.',
    keywords: ['update', 'upgrade', 'restart', 'version'],
    conditional: true,
  },
  {
    id: 'notifications.quiet-when',
    section: 'notifications',
    heading: 'Quiet when',
    label: 'Quiet when',
    hint: 'Held back on this screen only. A paired phone is still woken.',
    keywords: ['quiet', 'focus', 'foreground', 'visible', 'pane', 'open thread', 'mute', 'while using'],
    conditional: true,
  },
  {
    id: 'notifications.phone-push',
    section: 'notifications',
    heading: 'Phone push',
    label: 'Phone push',
    hint: 'Wakes a paired phone that is not connected. The message says what happened and which machine, never the thread.',
    keywords: ['fcm', 'firebase', 'mobile', 'android', 'wake'],
    conditional: true,
  },
  {
    id: 'notifications.phone-push-credential',
    section: 'notifications',
    heading: 'Phone push',
    label: 'Firebase service account key',
    hint: 'Use a service-account JSON key from the Firebase project built into your Android APK. Other projects will not work. Saved only on this computer.',
    keywords: ['fcm', 'firebase', 'service account', 'json', 'credential'],
    conditional: true,
  },

  // --- Observability ------------------------------------------------------
  {
    id: 'observability.tracing',
    section: 'observability',
    heading: 'OpenTelemetry OTLP',
    label: 'Enable tracing',
    hint: 'Turn on OTLP trace + metric export.',
    keywords: ['otel', 'jaeger', 'honeycomb', 'tempo', 'spans', 'metrics'],
  },
  {
    id: 'observability.otlp-endpoint',
    section: 'observability',
    heading: 'OpenTelemetry OTLP',
    label: 'OTLP endpoint',
    hint: 'gRPC host:port. Leave blank to use the OTel default (localhost:4317). Only used when tracing is enabled.',
    keywords: ['collector', 'grpc'],
  },
  {
    id: 'observability.event-log',
    section: 'observability',
    heading: 'Per-thread event recorder',
    label: 'Enable event replay log',
    hint: 'Takes effect immediately — no restart needed.',
    keywords: ['replay', 'jsonl', 'record', 'reproduce', 'debug'],
  },

  // --- Storage ------------------------------------------------------------
  {
    id: 'storage.retention-days',
    section: 'storage',
    heading: 'Automatic cleanup',
    label: 'Retention (days)',
    keywords: ['delete old threads', 'cleanup', 'disk', 'prune'],
  },
  {
    id: 'storage.archived-threads',
    section: 'storage',
    heading: 'Archived threads',
    label: 'Archived threads',
    keywords: ['unarchive', 'restore', 'delete permanently'],
  },
] as const satisfies ReadonlyArray<SettingsFieldDef>;

export type StaticFieldId = (typeof STATIC_FIELDS)[number]['id'];

export type SettingsFieldId = StaticFieldId | ProviderFieldId;

/**
 * The full index, provider pages expanded. Provider entries sit under their
 * page in the same order as the static Claude/Codex extras so a search
 * listing reads top-to-bottom like the page does.
 */
export const SETTINGS_FIELDS: readonly SettingsFieldDef[] = [
  ...STATIC_FIELDS.filter((f) => f.section !== 'claude' && f.section !== 'codex'),
  ...SETTINGS_PROVIDERS.flatMap((provider) => [
    ...PROVIDER_FIELDS.map((f) => ({
      id: providerFieldId(provider, f.slug),
      section: f.slug === 'accounts' ? 'accounts' as const : provider,
      heading: f.slug === 'accounts' ? providerLabel(provider) : f.heading,
      label: f.label,
      hint: 'hint' in f ? f.hint : undefined,
      keywords: [...f.keywords, PROVIDER_LABELS[provider]],
    })),
    ...STATIC_FIELDS.filter((f) => f.section === provider),
  ]),
];
