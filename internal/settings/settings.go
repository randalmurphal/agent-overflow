package settings

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/windowgeom"
)

// NetworkSettings groups LAN-bind preferences for the embedded
// transport server. Persisted as a nested object so the JSON shape
// stays stable when more network fields land (origin allow-list,
// TLS hints, etc.).
type NetworkSettings struct {
	// BindAll, when true, asks the transport server to listen on
	// 0.0.0.0 so other devices on the LAN can reach the app. Default
	// false keeps the bind on 127.0.0.1 — the safe loopback behaviour.
	BindAll bool `json:"bindAll"`
}

// EditorSettings groups the open-in-editor preferences. Lives in its
// own nested object so future fields (custom argv template, last-used
// editor for analytics, etc.) can land without reshuffling the
// top-level Settings struct.
type EditorSettings struct {
	// Preference is the editor ID (e.g. "code", "cursor",
	// "env:editor") the user explicitly selected. Empty falls back
	// to the catalog priority order in internal/editor.Resolve.
	Preference string `json:"preference"`
}

// RetentionSettings groups TTL cleanup preferences for the background
// sweeper that prunes stale threads (and their on-disk side effects)
// plus dated provider-event log files and bug-report bookmark files.
// Persisted as a nested object so future retention knobs (per-resource
// overrides, exemption lists) can land without reshuffling Settings.
type RetentionSettings struct {
	// Days is the age threshold in days. Threads whose updated_at is
	// older than now-(Days*24h) are eligible for sweep, as are dated
	// provider-event log files and bug-report bookmark files. A value
	// of 0 disables the sweep entirely.
	Days int `json:"days"`
}

// CurrentSchemaVersion is the version stamped on every Update-written
// settings file. Bump on any breaking shape change so a future loader
// can branch on the version and run a one-shot migration before
// merging defaults.
//
// Backwards compatibility convention: bump only when an existing field
// changes shape or semantics. Adding a new field, even with a
// non-zero default, does not require a bump because the sparse-load
// path tolerates absent keys naturally.
const CurrentSchemaVersion = 1

// Settings holds all user-configurable preferences.
type Settings struct {
	// SchemaVersion is the version of the on-disk shape this struct
	// expects. Older files (or files written before versioning) load as
	// SchemaVersion=0 and the Service treats them identically to
	// CurrentSchemaVersion until a future shape change introduces a
	// migration step. Never written by users; always overwritten to
	// CurrentSchemaVersion on any save via writeSparse.
	SchemaVersion int `json:"$schemaVersion,omitempty"`

	// NOTE: "theme" used to live here and is RETIRED (see
	// retiredSettingsFieldNames and docs/architecture/theme-system.md §6.2). The
	// light/dark mode is a property of the CLIENT MACHINE, not of a backend,
	// so it moved to <configDir>/themes/appearance.json. The old value is
	// CONSUMED ONCE at boot (initThemeDirectory reads it raw via
	// RetiredString) and DROPPED by the next sparse write — a retired field
	// is excluded from unknown-field preservation, so nothing republishes
	// it. Boot ordering is what makes that safe, and it is load-bearing.
	TimestampFormat string `json:"timestampFormat"`
	// SansFont and MonoFont select the typefaces wired into the
	// `--font-sans` and `--font-mono` CSS variables on the frontend.
	// Each is one of {"geist", "hack-nerd", "system"}. "geist" is the
	// eagerly-bundled default; "hack-nerd" lazy-loads a separate woff2
	// chunk so users on the default never pay its bundle cost; "system"
	// falls through to the OS fallback chain and adds zero weight.
	SansFont         string   `json:"sansFont"`
	MonoFont         string   `json:"monoFont"`
	FontSize         int      `json:"fontSize"`
	RecentWorkspaces []string `json:"recentWorkspaces"`
	DiffWordWrap     bool     `json:"diffWordWrap"`
	// CollapseDiffPreviews starts chat-timeline file-edit diff cards
	// collapsed (header row only). It controls the default state, not
	// capability: cards stay individually expandable either way.
	CollapseDiffPreviews bool `json:"collapseDiffPreviews"`
	StreamingEnabled     bool `json:"streamingEnabled"`
	// LowPowerMode minimizes rendering work for GPU-constrained
	// environments (weak machines, or a game/GPU-heavy app running
	// alongside): scroll placement is instant instead of spring-gliding,
	// streamed text reveals per wire chunk instead of animating, and the
	// activity shimmer is suppressed. Display-only — content and timing
	// are unaffected.
	LowPowerMode bool `json:"lowPowerMode"`
	// BrowserEnabled exposes Agent Overflow's built-in browser MCP tools and
	// companion pane to Claude and Codex sessions. BrowserPersistSiteData
	// checkpoints encrypted cookies and local storage per workspace. Both
	// default on. Outside-workspace
	// file access is the separate, deliberately off-by-default authority grant.
	BrowserEnabled               bool   `json:"browserEnabled"`
	BrowserPersistSiteData       bool   `json:"browserPersistSiteData"`
	BrowserAllowOutsideWorkspace bool   `json:"browserAllowOutsideWorkspace,omitempty"`
	ConfirmArchive               bool   `json:"confirmArchive"`
	ConfirmDelete                bool   `json:"confirmDelete"`
	AutoPinNewThreads            bool   `json:"autoPinNewThreads"`
	ClaudeBinaryPath             string `json:"claudeBinaryPath"`
	CodexBinaryPath              string `json:"codexBinaryPath"`
	ClaudeEnabled                bool   `json:"claudeEnabled"`
	CodexEnabled                 bool   `json:"codexEnabled"`

	// ClaudeTUIEnabled surfaces the claude-tui provider — the real
	// interactive Claude TUI driven inside a PTY — in the model/provider
	// pickers. It runs Claude's binary under Claude's auth, so
	// ClaudeEnabled gates it too: the frontend offers claude-tui only
	// when BOTH are on (frontend/src/lib/providers/catalog.ts,
	// providerIsEnabled).
	//
	// Deliberately absent from DefaultSettings, unlike the two fields
	// above: its default is the Go zero value, false (user decision,
	// 2026-08-18). The inversion is the point. Every settings file that
	// exists today predates this key, and an absent key must read as
	// "hidden" so upgrading users get the requested default instead of a
	// provider appearing in their pickers unasked. Defaulting it true
	// would also invert writeSparse — which persists only what differs
	// from DefaultSettings — and make `false` the value that survives a
	// write while `true` was dropped.
	//
	// Visibility only: an existing claude-tui thread keeps rendering,
	// resuming and sending with this off, exactly as a claude thread does
	// under ClaudeEnabled=false. Nothing in Go reads any of the three.
	ClaudeTUIEnabled bool `json:"claudeTuiEnabled"`

	// ClaudeHiddenModels / CodexHiddenModels list catalog model slugs
	// the user has hidden from model pickers. Hide-list semantics:
	// slugs absent from the list — including models a later app update
	// adds to the catalog — stay visible. Hiding is display-only:
	// existing threads on a hidden model keep working, and slug /
	// cost / capability resolution still sees the full catalog. The
	// Claude list applies to both the claude and claude-tui providers
	// (one binary, one catalog). Unknown slugs are kept as-is so a
	// hidden Codex model survives the live catalog being offline.
	ClaudeHiddenModels []string `json:"claudeHiddenModels,omitempty"`
	CodexHiddenModels  []string `json:"codexHiddenModels,omitempty"`

	// ClaudeCustomEnv / CodexCustomEnv are user-defined environment
	// variables applied to the provider processes Agent Overflow spawns
	// for that provider — chat sessions, account / identity / rate-limit
	// probes, and the text-generation CLI. Applied at spawn, so a change
	// reaches new sessions and probes only. The Claude list also covers
	// claude-tui (one binary, one backend).
	//
	// Names Agent Overflow pins itself are rejected on save rather than
	// dropped at spawn — see providerenv.go for the shape rules, the
	// reserved names, and why this is a list rather than a map. Sensitive
	// values are redacted out of the GetSettings wire shape; they are NOT
	// encrypted on disk (see the SECURITY NOTE below).
	ClaudeCustomEnv []ProviderEnvVar `json:"claudeCustomEnv,omitempty"`
	CodexCustomEnv  []ProviderEnvVar `json:"codexCustomEnv,omitempty"`

	// ClaudePromptOverrides / CodexPromptOverrides replace the provider's
	// own system prompt for the models each entry names — Claude via
	// `--system-prompt`, Codex via `baseInstructions`. Read at session
	// spawn, so an edit reaches the next session started and never
	// disturbs a running one. First ENABLED entry whose Models contains
	// the session's normalized model slug wins; the list is per provider
	// because the default prompt is model-shaped on both sides.
	//
	// Prompt text may carry `{{...}}` placeholders (workdir, git snapshot,
	// platform, model, Claude memory dir) rendered at spawn — see
	// internal/promptoverride, which owns both the match and the render.
	ClaudePromptOverrides []PromptOverride `json:"claudePromptOverrides,omitempty"`
	CodexPromptOverrides  []PromptOverride `json:"codexPromptOverrides,omitempty"`

	// ClaudeDisabledTools / CodexDisabledTools remove built-in tool
	// schemas from the model's context. Provider-wide (not model-scoped)
	// and spawn-only on both sides.
	//
	// The two lists speak different vocabularies on purpose. Claude has a
	// flat `--disallowedTools` deny list, so entries are RAW TOOL NAMES
	// ("Workflow", "WebSearch") and an unknown name is harmless. Codex has
	// no deny list at all — each entry is a CURATED TOGGLE ID that maps to
	// one or more per-thread config keys (see
	// internal/provider/codex.DisabledToolConfigOverrides, which owns the
	// table and skips ids it does not know). Neither list is enum-validated
	// here: Claude's vocabulary is the CLI's, and duplicating Codex's
	// toggle table into this package (which must not import
	// internal/provider) would give it a second place to drift.
	ClaudeDisabledTools []string `json:"claudeDisabledTools,omitempty"`
	CodexDisabledTools  []string `json:"codexDisabledTools,omitempty"`

	// ClaudeTodoRemindersDisabled turns off Claude Code's periodic
	// "track your work with the todo tools" nudges by exporting
	// CLAUDE_CODE_TODO_REMINDER_MODE=off into Claude sessions (headless
	// and claude-tui — one binary). Spawn-only, like the tool lists.
	// Default false keeps the vendor's behavior: the CLI nudges only
	// while the todo tools are actually in the session's tool set, so a
	// user who disables the whole todo group needs no reminder setting
	// at all — this exists for the keep-the-tools, lose-the-nudges
	// middle ground. Zero-value default, so it stays out of
	// DefaultSettings by construction.
	ClaudeTodoRemindersDisabled bool `json:"claudeTodoRemindersDisabled,omitempty"`

	// The Claude-only spawn-time axes delivered through the CLI's
	// `--settings` flagSettings block. None has a CLI flag; the settings
	// key is the delivery mechanism. Shapes, allowed values, validators
	// and the "why claude-tui is excluded" note live in claudesession.go.
	//
	// Every one of them treats its ZERO VALUE as "say nothing" — the key
	// is omitted from the rendered block and the CLI's own resolution
	// stands. That is what keeps them out of DefaultSettings and makes an
	// absent key in an older settings file read as the CLI default.
	//
	// Note which fields carry `omitempty` and which do not: it is a no-op
	// on a STRUCT under encoding/json, so writing it there would claim an
	// omission that never happens. The struct-valued axes below spell the
	// tag plainly and state their zero-value meaning in prose instead;
	// only the string-valued ones actually omit.
	//
	// ClaudeOutputStyle names one of Claude Code's four built-in output
	// styles (Concise / Proactive / Explanatory / Learning).
	ClaudeOutputStyle string `json:"claudeOutputStyle,omitempty"`
	// ClaudeCrossSession is the machine-wide peer inbox: whether other
	// Claude sessions on this host may discover and message AO's threads,
	// and what happens to a message that arrives. Shape, allowed values
	// and the reason "hold" is not one of them live in
	// claudecrosssession.go.
	//
	// It is NOT part of ClaudeSessionAxes below even though half of it
	// rides the same `--settings` block: those axes are spawn-only BY
	// CONSTRUCTION (invisible to PlanLiveUpdate), while this one travels
	// on provider.SessionOptions so a save converges a running session
	// through the ordinary deferred-restart path. The inbox binds once at
	// setup, so a restart is the only way to change it.
	ClaudeCrossSession ClaudeCrossSession `json:"claudeCrossSession"`
	// ClaudeSubagentLimits caps subagent spawn depth and concurrency via
	// CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH /
	// CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS in the block's env map.
	ClaudeSubagentLimits ClaudeSubagentLimits `json:"claudeSubagentLimits"`
	// ClaudeToolMemoryLimit caps the memory cgroup Claude installs for
	// its tool subprocesses (CLAUDE_CODE_TOOL_MEMORY_LIMIT). The CLI
	// implements this by writing a cgroup file under /proc/self/cgroup,
	// so it takes effect only when the backend runs on Linux (including
	// the WSL backend) — on macOS and native Windows the value is inert.
	ClaudeToolMemoryLimit string `json:"claudeToolMemoryLimit,omitempty"`

	// ClaudeThinking is the extended-thinking preference for headless
	// Claude sessions: leave it to Claude Code, turn thinking off, or pin
	// a fixed token budget — plus whether thinking text reaches the wire.
	//
	// It sits beside the four axes above but is NOT one of them: those
	// ride the `--settings` block and are spawn-only, while this one has
	// BOTH a spawn form (`--thinking` / `--max-thinking-tokens` /
	// `--thinking-display`) and a live form (the `set_max_thinking_tokens`
	// control_request), so a change reaches running sessions. The one
	// exception is the return to "Claude Code decides", which has no wire
	// form at all and lands as a deferred restart — exactly like turning a
	// prompt override off (docs/specs/prompt-tool-overrides.md).
	ClaudeThinking ClaudeThinking `json:"claudeThinking"`

	// DefaultThreadEnvMode seeds the workspace mode for new draft threads.
	// Accepts "local" or "worktree"; unknown values fall back to "local"
	// when settings are loaded.
	DefaultThreadEnvMode string `json:"defaultThreadEnvMode"`

	// WorktreeBranchPrefix is prepended to auto-generated temporary and
	// semantic worktree branch names. It is intentionally flat (default
	// "ao-") rather than namespace-like ("ao/") so generated branches
	// read like normal feature branches.
	WorktreeBranchPrefix string `json:"worktreeBranchPrefix"`

	// PaneDensity controls the minimum workspace pane width before the pane
	// host starts horizontal scrolling. One of {"compact", "comfortable",
	// "spacious"}.
	PaneDensity string `json:"paneDensity"`

	// ActivityRunDefault is the starting state for a run of consecutive
	// tool/thinking rows in the transcript. One of {"expanded",
	// "collapsed"}. Applies to every run the user has not explicitly
	// toggled, including the live one -- with "collapsed" a streaming run
	// shows as a chip with counts ticking until opened.
	ActivityRunDefault string `json:"activityRunDefault"`

	// ActivityRunWindowRows is how many of a run's newest rows stay mounted.
	// Sized to overfill the run's height cap so its tail always has content
	// below the fold; older rows mount on demand behind an "N earlier" line.
	ActivityRunWindowRows int `json:"activityRunWindowRows"`

	// TextGenerationProvider selects which CLI drives non-chat text
	// generation (commit messages today; PR bodies and thread titles
	// eventually). Mirrors t3-code's RoutingTextGeneration: one of
	// {"codex", "claude"}. Empty falls through to the default at the
	// validation layer.
	TextGenerationProvider string `json:"textGenerationProvider"`

	// TextGenerationModel is the model id the text-generation CLI uses.
	// Empty string means "use the per-provider default" (codex ->
	// gpt-5.6-luna, claude -> claude-haiku-4-5). We avoid forcing a
	// concrete default on the field itself because the right model
	// depends on which provider is selected, and a cross-provider
	// default would be wrong half the time.
	TextGenerationModel string `json:"textGenerationModel"`

	// TextGenerationReasoningEffort controls the reasoning budget the
	// text-generation CLI spends. Mirrors the provider-specific reasoning
	// effort enum. Default is "low" — commit/PR message generation benefits
	// more from speed than from heavy reasoning.
	TextGenerationReasoningEffort string `json:"textGenerationReasoningEffort"`

	// CommitMessageStyle selects the phrasing guidance generated commit
	// messages follow: "conventional" (Conventional Commits, the
	// default), "repo" (match the repository's recent commit subjects),
	// or "custom" (follow CommitMessageStyleCustom). Mirrors t3-code's
	// source-control writing-style options.
	CommitMessageStyle string `json:"commitMessageStyle"`

	// CommitMessageStyleCustom holds the user's free-text style
	// instructions, consumed only when CommitMessageStyle == "custom".
	// Trimmed and length-capped at validation.
	CommitMessageStyleCustom string `json:"commitMessageStyleCustom"`

	// Auto-compact thresholds, percent-of-window. Per provider per tier;
	// model-agnostic by design (the user picks model + active window via
	// the composer's pickers, not via Settings). At session start the
	// resolved threshold is `thread.AutoCompactPercent || settings.<...>`,
	// so editing the slider applies live to the next turn on existing
	// threads unless the user has set a per-thread override.
	//
	// Range 1..90; load-time sanitization clamps out-of-range values to
	// the default (90) rather than rejecting the file.
	ClaudeAutoCompactStandardPercent int `json:"claudeAutoCompactStandardPercent"`
	ClaudeAutoCompactExtendedPercent int `json:"claudeAutoCompactExtendedPercent"`
	CodexAutoCompactStandardPercent  int `json:"codexAutoCompactStandardPercent"`
	CodexAutoCompactExtendedPercent  int `json:"codexAutoCompactExtendedPercent"`

	// Observability — all opt-in. Empty/false defaults leave the app quiet.
	//
	// SECURITY NOTE: this file is stored on disk in plaintext and is
	// read/written without any encryption. settings.json itself lands
	// at 0600 (the default os.CreateTemp picks for the temp file we
	// rename in over the destination), and the parent directory is
	// created at 0700 since this struct now persists per-launch tokens.
	// Even with restrictive perms, anything that could reasonably be
	// called a long-lived secret (API keys, OAuth refresh tokens, user
	// credentials) does NOT belong here: this file lives in a
	// user-visible config dir and may be swept into cloud backup tools
	// without the user realising. Put long-lived secrets in the OS
	// keychain via a dedicated package and keep this struct for
	// preferences plus per-launch bootstrap material only.
	ObservabilityTracingEnabled  bool   `json:"observabilityTracingEnabled"`
	ObservabilityOtlpEndpoint    string `json:"observabilityOtlpEndpoint"`
	ObservabilityEventLogEnabled bool   `json:"observabilityEventLogEnabled"`

	// Network groups LAN-bind preferences. Default zero value keeps
	// the transport on loopback; flipping BindAll triggers a
	// transport-server rebind to 0.0.0.0 without restarting the app.
	Network NetworkSettings `json:"network"`

	// Editor holds the open-in-editor preferences. Default zero value
	// lets internal/editor pick the best available editor via catalog
	// priority; setting Editor.Preference pins a specific one even
	// when later WSL detection finds a higher-priority option.
	Editor EditorSettings `json:"editor"`

	// Retention controls the background TTL sweep. Default
	// Retention.Days = 30 cleans threads, dated provider-event logs,
	// and bug-report bookmark files older than the window. 0 disables.
	Retention RetentionSettings `json:"retention"`

	// BackgroundGitFetch enables the periodic `git fetch` the app runs
	// for the repositories behind the sidebar's projects, so ahead/behind
	// counts reflect the remote instead of freezing at the user's last
	// manual fetch. Default true; one fetch per repository per
	// git.FetchStaleWindow, origin only, never --prune.
	//
	// Turn it off for a metered or VPN-gated connection, or for a
	// monorepo whose fetch is expensive. Off means the counts are only
	// as fresh as the last explicit fetch/pull/prune — nothing else
	// changes. See app_git_background_fetch.go for the cadence.
	BackgroundGitFetch bool `json:"backgroundGitFetch"`

	// GitLabSelfHostedHosts is the user's allowlist of self-hosted
	// GitLab hostnames (bare hosts, e.g. "gitlab.mycompany.com").
	// Origin URLs whose host matches an entry classify as the "gitlab"
	// forge, enabling the Ship Changes wizard, MR labels, and the
	// `glab` CLI integration. `gitlab.com` does not need to be listed;
	// it is recognised by literal hostname match. Entries are stored
	// lowercase, deduped, and stripped of scheme/path on write.
	GitLabSelfHostedHosts []string `json:"gitlabSelfHostedHosts,omitempty"`

	// RemoteEndpoints stores the user's `--connect` targets: remote-
	// hosted backends the desktop binary can attach to instead of
	// booting a local transport. Persisted as a flat list keyed by
	// stable IDs so the settings UI can rename / re-order without
	// disturbing the connect commands the user has already shared.
	//
	// SECURITY NOTE: this list contains ephemeral session tokens. They
	// are stored in plaintext alongside settings.json (file lands at
	// 0600, parent dir at 0700). That matches the threat model
	// documented above — settings.json must not contain anything more
	// sensitive than what a local-process attacker could already read
	// out of running webviews. If the remote endpoints' tokens ever
	// become long-lived bearer tokens, move this field to a
	// keychain-backed store and remove the JSON persistence path.
	RemoteEndpoints []RemoteEndpoint `json:"remoteEndpoints,omitempty"`

	// ProjectSortMode controls sidebar project ordering. One of
	// {"lastActivity", "createdAt", "manual"}. Persisted here rather
	// than in the webview's localStorage because localStorage is
	// ephemeral on some platforms (WebKit2GTK / WSL2).
	ProjectSortMode string `json:"projectSortMode"`

	// UsagePeriod is the selected time period for the usage surfaces
	// (sidebar usage footer + usage modal). One of {"day", "week",
	// "month", "all"} — calendar periods, not rolling windows. Same
	// persistence rationale as ProjectSortMode.
	UsagePeriod string `json:"usagePeriod"`

	// The composer's working-indicator knobs. Two independent axes —
	// the WORD beside the spinner and the sprite that animates — plus the
	// per-axis curation lists.
	//
	// SpinnerVerbsEnabled turns the rotating verb ("Deliberating…",
	// "Reticulating…") on. Default TRUE, and therefore present in
	// DefaultSettings: the verbs are what the indicator has always shown,
	// so an absent key in every settings file that predates this must read
	// as on. Contrast ClaudeTUIEnabled, whose default is the zero value
	// and which stays out of DefaultSettings for exactly the mirrored
	// reason.
	SpinnerVerbsEnabled bool `json:"spinnerVerbsEnabled"`

	// SpinnerAnimationsEnabled turns the animated sprite on. Default
	// false (zero value, so it stays out of DefaultSettings by
	// construction): motion beside the composer is opt-in, and a user who
	// upgrades into this feature should not find their indicator moving
	// unasked.
	SpinnerAnimationsEnabled bool `json:"spinnerAnimationsEnabled,omitempty"`

	// SpinnerCustomVerbs are the user's own verbs, added to the pool the
	// indicator draws from. Trimmed and deduped; bounds and the
	// reject-don't-truncate rule live in spinner.go.
	SpinnerCustomVerbs []string `json:"spinnerCustomVerbs,omitempty"`

	// SpinnerBuiltinVerbsDisabled removes the shipped verbs from the pool,
	// leaving only SpinnerCustomVerbs. Zero-value default: the built-ins
	// are what the indicator shows today.
	SpinnerBuiltinVerbsDisabled bool `json:"spinnerBuiltinVerbsDisabled,omitempty"`

	// SpinnerDisabledAnimations lists the animation ids the user
	// UNCHECKED from the random pool. An EXCLUSION list, not a selection:
	// a sprite dropped into <configDir>/spinners tomorrow, or one a later
	// app version bundles, joins the pool automatically instead of staying
	// invisible until someone goes and ticks it.
	SpinnerDisabledAnimations []string `json:"spinnerDisabledAnimations,omitempty"`

	// SpinnerCompactionAnimation pins one sprite to auto-compaction, which
	// is the one moment the indicator means something specific rather than
	// "working". "" means never chosen and resolves to the default;
	// "none" is the explicit choice of nothing; anything else is an
	// animation id. Whether that id still resolves is the frontend's
	// question — this package cannot see which sprites exist, and the
	// default sprite for "" is the frontend's to name (catalog.ts), so no
	// id is ever baked in here.
	SpinnerCompactionAnimation string `json:"spinnerCompactionAnimation,omitempty"`

	// WorkflowPaused is the global workflow kill switch: while set, no
	// workflow phase starts anywhere and in-flight turns finish. It is
	// persisted so a paused engine stays paused across a restart.
	WorkflowPaused bool `json:"workflowPaused"`

	// KeepAwakeEnabled is the master switch for the OS sleep inhibitor
	// (internal/power). While set, the machine does not idle-sleep, so a
	// long unattended turn is not cut off by a suspend.
	//
	// Deliberately absent from DefaultSettings, the ClaudeTUIEnabled
	// pattern: the default is the Go zero value, false. Holding a system
	// power assertion is the kind of thing a user must ask for, and every
	// settings file that predates this key must read as "off" rather than
	// silently start pinning the machine awake after an upgrade.
	// Defaulting it true would also invert writeSparse — which persists
	// only what differs from DefaultSettings — and make `false` the value
	// that survives a write while `true` was dropped.
	KeepAwakeEnabled bool `json:"keepAwakeEnabled"`

	// KeepAwakeScreen additionally keeps the DISPLAY on while
	// KeepAwakeEnabled is set. With it off, only system sleep is blocked
	// and the screen may blank and lock normally. Meaningless on its own —
	// the master switch is what decides whether anything is held at all.
	//
	// Default TRUE, and therefore present in DefaultSettings (the mirror
	// of KeepAwakeEnabled, and the same reasoning as SpinnerVerbsEnabled):
	// "keep awake" colloquially means the screen stays up, so the axis
	// starts at the meaning of the phrase and the user narrows it. An
	// absent key must read as on, which only DefaultSettings can do.
	KeepAwakeScreen bool `json:"keepAwakeScreen"`

	// Per-client UI view state (pane layout, collapsed projects,
	// sidebar width, …) deliberately does NOT live here: it moved to
	// the store's ui_state table, keyed per client, so two clients of
	// the same backend stop fighting over one value. See
	// internal/store/ui_state.go and frontend stores/appStorage.ts.

	// Window stores the desktop window placement (position, size, and
	// maximized/fullscreen state) so the app reopens where it was last
	// closed. Owned by the Go side, not the frontend — written from
	// window move/resize events, never through the Settings UI. The zero
	// value (Valid=false) means "never saved" and the window centers.
	// Not used by the WSL/Windows launcher, which is a separate window in
	// a separate coordinate space and persists to its own window.json.
	Window windowgeom.Geometry `json:"window"`
}

// DefaultSettings provides sane defaults for all settings fields.
var DefaultSettings = Settings{
	TimestampFormat:        "locale",
	SansFont:               "geist",
	MonoFont:               "geist",
	FontSize:               13,
	DiffWordWrap:           false,
	CollapseDiffPreviews:   true,
	StreamingEnabled:       true,
	LowPowerMode:           false,
	BrowserEnabled:         true,
	BrowserPersistSiteData: true,
	ConfirmArchive:         true,
	ConfirmDelete:          true,
	AutoPinNewThreads:      true,
	ClaudeBinaryPath:       "claude",
	CodexBinaryPath:        "codex",
	ClaudeEnabled:          true,
	CodexEnabled:           true,
	DefaultThreadEnvMode:   "local",
	WorktreeBranchPrefix:   "ao-",
	PaneDensity:            "compact",
	// Collapsed keeps long tool/thinking runs out of the way until the
	// user opens them (default flipped 2026-08-30).
	ActivityRunDefault:    "collapsed",
	ActivityRunWindowRows: 30,
	// Text-generation defaults: Codex is cheap + fast for short JSON
	// responses, so it's the sensible default. The model stays empty
	// so the call site picks the per-provider default; if the user
	// switches provider without updating model, the app still works.
	TextGenerationProvider:        "codex",
	TextGenerationModel:           "",
	TextGenerationReasoningEffort: "low",
	CommitMessageStyle:            "conventional",
	// Both providers ship a 90% default — aggressive enough that the
	// user notices auto-compact when it triggers, conservative enough
	// to leave headroom for the final response. The percent applies to
	// whichever tier matches the live context window.
	ClaudeAutoCompactStandardPercent: 90,
	ClaudeAutoCompactExtendedPercent: 90,
	CodexAutoCompactStandardPercent:  90,
	CodexAutoCompactExtendedPercent:  90,
	// Observability defaults to off so there is zero runtime cost for users
	// who don't opt in. The OTLP endpoint is only meaningful when tracing
	// is enabled; we leave it blank so a misconfigured endpoint can't cause
	// silent failures for default users.
	ObservabilityTracingEnabled:  false,
	ObservabilityOtlpEndpoint:    "",
	ObservabilityEventLogEnabled: false,
	// 30 days is the default retention window. The cleanup loop reads
	// this on every tick, so toggling it doesn't require a restart.
	Retention: RetentionSettings{Days: 30},
	// On by default: a decorative behind-count is worse than no
	// behind-count, and the cost is one `git fetch` per repository per
	// five minutes. Users who can't afford that turn it off.
	BackgroundGitFetch: true,
	ProjectSortMode:    "lastActivity",
	UsagePeriod:        "month",
	WorkflowPaused:     false,
	// The verb beside the working indicator is what the composer has
	// always shown, so it stays on for everyone who upgrades into the
	// spinner settings — an absent key reads true only because the default
	// is here. The animated sprite is the new behavior and is opt-in, so
	// SpinnerAnimationsEnabled deliberately is NOT here (zero value).
	// SpinnerCompactionAnimation is NOT here either, and not because it has
	// no default: "" IS the stored default ("never chosen"), which the
	// FRONTEND resolves to its default sprite. Storing a concrete id here
	// would bake a frontend sprite name into this package and make ""
	// unrepresentable — the settings UI's "Default" choice could then never
	// round-trip (it would echo back as the id and match no option).
	SpinnerVerbsEnabled: true,
	// The screen axis of "keep awake" defaults ON so the phrase means what
	// a user expects when they flip the master switch; the master switch
	// itself (KeepAwakeEnabled) is the zero value and deliberately NOT
	// here. See both fields for why the pair is split this way.
	KeepAwakeScreen: true,
}

// HiddenModelsForProvider returns the hidden-model slug list for the
// given provider name. claude-tui shares the claude list (same binary,
// same catalog). Unknown providers hide nothing.
func (s Settings) HiddenModelsForProvider(provider string) []string {
	switch provider {
	case "claude", "claude-tui":
		return s.ClaudeHiddenModels
	case "codex":
		return s.CodexHiddenModels
	default:
		return nil
	}
}

// AutoCompactPercents returns the per-tier compact thresholds for the
// given provider as (standard, extended). Unknown providers fall back
// to the Claude pair so a stale provider string can't strand a session
// with 0/0 (which would disable auto-compact entirely).
func (s Settings) AutoCompactPercents(provider string) (standard, extended int) {
	switch provider {
	case "codex":
		return s.CodexAutoCompactStandardPercent, s.CodexAutoCompactExtendedPercent
	default:
		return s.ClaudeAutoCompactStandardPercent, s.ClaudeAutoCompactExtendedPercent
	}
}

// Service manages reading and writing the settings JSON file.
type Service struct {
	path        string
	mu          sync.RWMutex
	cached      *Settings
	cachedState fileState
	// unknownFields captures any top-level JSON keys from the on-disk file
	// that do not map to a field on the Settings struct. We preserve them
	// verbatim on writeSparse so downgrading the app, or running a build
	// with forward-compat fields the Settings struct doesn't yet know
	// about, does not silently drop those fields. Written under s.mu.
	unknownFields map[string]json.RawMessage
}

type fileState struct {
	exists  bool
	modTime time.Time
	size    int64
}

// NewService creates a settings service that reads/writes configDir/settings.json.
// The file is not created until the first write.
func NewService(configDir string) *Service {
	return &Service{
		path: filepath.Join(configDir, "settings.json"),
	}
}

// Path returns the full path to the settings file.
func (s *Service) Path() string {
	return s.path
}

// RetiredString reads one RETIRED field's raw string value straight out
// of the settings file.
//
// A retired field is deliberately unreachable through Settings: it is
// gone from the struct, so unmarshalling drops it, and it is listed in
// retiredSettingsFieldNames so unknownFields does not republish it
// either. That is the right posture for a field nobody should keep
// writing — and it leaves exactly one legitimate reader, the ONE-TIME
// migration that moves the old value to wherever it now lives (today:
// "theme" → <configDir>/themes/appearance.json, per
// docs/architecture/theme-system.md §6.2).
//
// Reads the file rather than the cache on purpose: the cache is typed,
// so it cannot hold a field the type no longer has. Every failure —
// absent file, unparseable JSON, absent key, non-string value — answers
// "" and the caller falls through to its own default.
func (s *Service) RetiredString(field string) string {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	value, ok := raw[field]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return ""
	}
	return text
}

// Get returns the current settings, merging file contents over defaults.
// If the file is missing or malformed, defaults are returned.
func (s *Service) Get() Settings {
	currentState := readFileState(s.path)

	s.mu.RLock()
	if s.cached != nil && s.cachedState.equal(currentState) {
		result := *s.cached
		s.mu.RUnlock()
		return result
	}
	s.mu.RUnlock()

	// Cache miss: load from file under write lock.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	currentState = readFileState(s.path)
	if s.cached != nil && s.cachedState.equal(currentState) {
		return *s.cached
	}

	loaded := s.loadFromFile()
	s.cached = &loaded
	s.cachedState = currentState
	return loaded
}

// Update applies a partial patch to the current settings, persists the result
// with sparse serialization, and returns the new full settings.
//
// The "remoteEndpoints" key is rejected at the patch boundary: applyPatch
// merges top-level keys via wholesale assignment, so a caller doing
// `GetSettings -> mutate one field -> Update(full struct)` would clobber
// every saved endpoint's token with the redacted (empty) values returned
// by GetSettings. Tokens are only mutated through the dedicated CRUD
// helpers (AddRemoteEndpoint / UpdateRemoteEndpoint / DeleteRemoteEndpoint
// / TouchRemoteEndpoint) which read the persisted token before writing.
// This guard keeps a future caller — including a refactor or remote
// loopback path — from regressing the contract.
func (s *Service) Update(patch map[string]any) (Settings, error) {
	if _, ok := patch["remoteEndpoints"]; ok {
		return Settings{}, fmt.Errorf("settings: use AddRemoteEndpoint / UpdateRemoteEndpoint / DeleteRemoteEndpoint to mutate remote endpoints")
	}
	for _, key := range []string{"claudeCustomEnv", "codexCustomEnv"} {
		if _, ok := patch[key]; ok {
			// Same trap as remoteEndpoints: GetSettings redacts sensitive
			// values, so a read-mutate-write round trip through this path
			// would persist the redaction and destroy them.
			return Settings{}, fmt.Errorf("settings: use SetProviderEnvVar / DeleteProviderEnvVar to mutate %s", key)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadFromFile()

	patched, err := applyPatch(current, patch)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: apply patch: %w", err)
	}
	patched, err = validateSettings(patched)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: validate: %w", err)
	}

	if err := s.writeSparse(patched); err != nil {
		return Settings{}, err
	}

	s.cached = &patched
	s.cachedState = readFileState(s.path)
	return patched, nil
}

// AddRecentWorkspace pushes a workspace path to the front of the recent list,
// deduplicating and capping at 10 entries.
func (s *Service) AddRecentWorkspace(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadFromFile()

	// Build new list: path first, then existing entries minus duplicates.
	seen := map[string]bool{path: true}
	recent := []string{path}
	for _, ws := range current.RecentWorkspaces {
		if !seen[ws] {
			seen[ws] = true
			recent = append(recent, ws)
		}
	}
	if len(recent) > 10 {
		recent = recent[:10]
	}

	current.RecentWorkspaces = recent

	if err := s.writeSparse(current); err != nil {
		log.Printf("settings: persist recent workspace: %v", err)
		return
	}

	s.cached = &current
	s.cachedState = readFileState(s.path)
}

// loadFromFile reads the settings file and merges over defaults.
// Returns DefaultSettings if the file is missing or malformed. Must be called
// with s.mu held (either read or write). Captures any unknown top-level keys
// into s.unknownFields so a follow-up write preserves them.
//
// On a JSON parse failure we rename the broken file aside as
// `settings.json.corrupt-<unix>` BEFORE returning defaults, so a
// subsequent writeSparse can't silently overwrite the original. The
// corrupt file is left on disk for the user to inspect or copy fields
// out of — losing remote-endpoint tokens or recent workspaces because
// of one bad write would erase work the user expects to be durable.
// The rename is best-effort; if it fails we still return defaults so
// the app can boot, but we log loudly.
func (s *Service) loadFromFile() Settings {
	data, err := os.ReadFile(s.path)
	if err != nil {
		// Missing file is normal on first run.
		s.unknownFields = nil
		return copyDefaults()
	}

	// Start from defaults, then overlay file values.
	result := copyDefaults()
	if err := json.Unmarshal(data, &result); err != nil {
		preservedPath := s.path + fmt.Sprintf(".corrupt-%d", time.Now().Unix())
		if renameErr := os.Rename(s.path, preservedPath); renameErr != nil {
			log.Printf("settings: malformed JSON in %s and could not preserve original (%v); falling back to defaults: %v", s.path, renameErr, err)
		} else {
			log.Printf("settings: malformed JSON in %s; original preserved at %s, falling back to defaults: %v", s.path, preservedPath, err)
		}
		s.unknownFields = nil
		return copyDefaults()
	}
	s.unknownFields = captureUnknownFields(data)
	return sanitizeLoadedSettings(result)
}

// captureUnknownFields returns a map of top-level JSON keys from raw that
// do not correspond to a field on the Settings struct. Used to preserve
// forward-compat / downgrade fields across a write.
func captureUnknownFields(raw []byte) map[string]json.RawMessage {
	var fileMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fileMap); err != nil {
		return nil
	}
	known := knownSettingsFieldNames()
	retired := retiredSettingsFieldNames()
	unknown := make(map[string]json.RawMessage)
	for k, v := range fileMap {
		if _, ok := known[k]; ok {
			continue
		}
		if _, ok := retired[k]; ok {
			continue
		}
		unknown[k] = v
	}
	if len(unknown) == 0 {
		return nil
	}
	return unknown
}

// knownSettingsFieldNames returns the set of JSON field names the
// Settings struct serializes. Computed by reflecting on the struct's
// fields rather than marshalling DefaultSettings — `omitempty` fields
// with zero defaults (e.g. RemoteEndpoints) would be missing from the
// marshalled view, which would mis-classify a user-written value as
// "unknown" and double-publish it through unknownFields preservation.
//
// The reflection path keeps the set in sync with the struct definition
// automatically as fields are added or renamed, same as the marshal
// approach, but without the `omitempty` blind spot.
func knownSettingsFieldNames() map[string]struct{} {
	t := reflect.TypeOf(Settings{})
	known := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// JSON tag may be "name,omitempty" — split and keep just the name.
		name := tag
		if idx := strings.Index(tag, ","); idx >= 0 {
			name = tag[:idx]
		}
		if name == "" {
			continue
		}
		known[name] = struct{}{}
	}
	return known
}

func retiredSettingsFieldNames() map[string]struct{} {
	return map[string]struct{}{
		"defaultProvider":        {},
		"defaultModelClaude":     {},
		"defaultModelCodex":      {},
		"modelContextWindows":    {},
		"defaultMode":            {},
		"defaultRuntimeMode":     {},
		"defaultReasoningEffort": {},
		"defaultFastMode":        {},
		"defaultContextWindow":   {},
		// Moved to <configDir>/themes/appearance.json — theme is a property
		// of the client machine, not of a backend (docs/architecture/theme-system.md
		// §6.2). Listed here so it is not round-tripped through unknownFields
		// preservation: an upgrading user's value is CONSUMED ONCE at boot
		// (initThemeDirectory, via RetiredString) and DROPPED by the next
		// sparse write. That is the intended lifecycle for a field nobody
		// should keep writing, and it is why the boot read has to happen
		// before any Update can reach the file.
		"theme": {},
	}
}

// writeSparse persists only the fields that differ from DefaultSettings.
// Uses atomic write (temp file + rename). Unknown fields previously read
// from the file are preserved alongside the sparse known fields so
// forward-compat / downgrade values are not dropped by an Update.
//
// Stamps SchemaVersion = CurrentSchemaVersion on every write so a
// future loader can branch on a missing/older version and run a
// migration. Older files written before versioning load as 0; future
// writes by this build re-stamp them to the current version.
func (s *Service) writeSparse(current Settings) error {
	current.SchemaVersion = CurrentSchemaVersion
	sparse, err := buildSparseMap(current)
	if err != nil {
		return fmt.Errorf("settings: build sparse map: %w", err)
	}

	// Merge unknown fields under the sparse known fields. Known keys win
	// if the unknown-fields map somehow contains a clashing key — this
	// can happen only if Settings gained a field since loadFromFile was
	// called, and the new field is a known one now.
	merged := make(map[string]any, len(sparse)+len(s.unknownFields))
	for k, v := range s.unknownFields {
		merged[k] = v
	}
	for k, v := range sparse {
		merged[k] = v
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}
	data = append(data, '\n')

	// Ensure the directory exists. 0700 because this struct now stores
	// per-launch tokens (RemoteEndpoints[*].Token); even though the
	// renamed temp file lands at 0600 itself, a 0755 parent would let
	// other local accounts list the dir contents. MkdirAll is a no-op
	// when dir already exists with looser perms — that's acceptable
	// because the file's own 0600 still gates the contents.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settings: create config dir: %w", err)
	}

	// Atomic write: temp file in same directory, then rename.
	tmp, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings: create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("settings: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("settings: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("settings: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("settings: rename temp file: %w", err)
	}
	return nil
}

// buildSparseMap returns a map containing only fields that differ from defaults.
func buildSparseMap(current Settings) (map[string]any, error) {
	currentBytes, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	defaultBytes, err := json.Marshal(DefaultSettings)
	if err != nil {
		return nil, err
	}

	var currentMap, defaultMap map[string]any
	if err := json.Unmarshal(currentBytes, &currentMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(defaultBytes, &defaultMap); err != nil {
		return nil, err
	}

	sparse := make(map[string]any)
	for k, v := range currentMap {
		defaultVal, exists := defaultMap[k]
		if !exists || !jsonEqual(v, defaultVal) {
			sparse[k] = v
		}
	}
	return sparse, nil
}

// jsonEqual compares two values after JSON round-tripping to handle type
// normalization (e.g., float64 vs int).
func jsonEqual(a, b any) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(aj) == string(bj)
}

// applyPatch merges a partial map into a Settings struct using JSON
// marshal/unmarshal for type-safe conversion.
func applyPatch(base Settings, patch map[string]any) (Settings, error) {
	baseBytes, err := json.Marshal(base)
	if err != nil {
		return Settings{}, err
	}

	var merged map[string]any
	if err := json.Unmarshal(baseBytes, &merged); err != nil {
		return Settings{}, err
	}

	for k, v := range patch {
		merged[k] = v
	}

	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return Settings{}, err
	}

	var result Settings
	if err := json.Unmarshal(mergedBytes, &result); err != nil {
		return Settings{}, err
	}
	return result, nil
}

// copyDefaults returns a copy of DefaultSettings with nil slice fields so
// callers can append without aliasing package-level defaults.
func copyDefaults() Settings {
	d := DefaultSettings
	d.RecentWorkspaces = nil
	d.GitLabSelfHostedHosts = nil
	return d
}

func readFileState(path string) fileState {
	info, err := os.Stat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{
		exists:  true,
		modTime: info.ModTime(),
		size:    info.Size(),
	}
}

func (s fileState) equal(other fileState) bool {
	return s.exists == other.exists && s.modTime.Equal(other.modTime) && s.size == other.size
}
