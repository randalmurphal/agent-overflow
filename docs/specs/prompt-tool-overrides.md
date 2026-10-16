# System-prompt overrides + tool toggles

Status: implemented. Wire/source facts backing every
mechanism here:
[claude-wire.md §System prompt assembly](../references/claude-wire.md#system-prompt-assembly----system-prompt-replacement-verified-21234)
and [codex-instructions-tools.md](../references/codex-instructions-tools.md).

## Goal

A settings surface (Agents group) where the user can:

1. **Replace the provider's default system prompt** per provider, scoped
   to selected models: Claude via `--system-prompt`, Codex via
   `baseInstructions`. Toggleable per entry. A change applies at each
   thread's next session start, never to a running session
   ([Reconcile semantics](#reconcile-semantics-what-a-settings-edit-does-to-a-live-session)).
   Each prompt's text is a file the user or an agent edits directly
   ([Prompt files](#prompt-files)).
2. **Disable built-in tools** per provider so their schemas never reach
   the model context (e.g. Claude's `Workflow`, because the user runs
   AO's own workflow engine instead). Claude's tools payload is ~92KB of schemas,
   so this is the bigger context lever.
3. **Turn the app guide on or off**: the short text Agent Overflow
   appends so interactive Claude, claude-tui and Codex sessions know what
   the chat pane renders and which of the app's tool servers are on.
   Default on (see [App guide](#app-guide)).

Both providers are already fully plumbed for prompt replacement:
`cfg.SystemPrompt` → `--system-prompt-file`
(`internal/provider/claude/session.go`) and → `baseInstructions` on
`thread/start` (`internal/provider/codex/session_helpers.go`). The
feature is settings + composition + UI, not new provider work.

### claude-tui is covered (supersedes the original exclusion)

**Decision 2026-08-18 (with user), superseding the 2026-08-17
exclusion.** Claude's override list and disabled-tool list apply to
interactive TUI sessions too, mirroring how `claudeHiddenModels` covers
both. The original exclusion assumed AO could not pass either flag to
the real TUI. A PTY + wire capture against claude 2.1.234 disproved
that:

- **`--system-prompt-file <path>` works interactively.** The request's
  `system` array becomes [billing header, the TUI's fixed identity line
  `"You are Claude Code, Anthropic's official CLI for Claude."`, the
  file's content], a full body replacement, same as headless. The
  identity line is not replaceable on either transport;
  [claude-wire.md §Request shape](../references/claude-wire.md#request-shape)
  records the line each one sends.
- **Repeated `--disallowedTools <name>` works interactively.** The
  named tools' schemas are absent from the request. Quirk worth
  knowing when reading a user's list: the CLI **aliases `Task` and
  `Agent`**, so disallowing `Task` removes the `Agent` tool too.

Mechanically these are the same class of flag AO already passes on the
PTY launch (`--model`, `--effort`, `--resume` in
`internal/provider/claudetui/launch.go`), and:

- `settings.PromptOverridesForProvider` / `DisabledToolsForProvider`
  route `claude-tui` onto the Claude lists, so the generic spawn path
  (`applySettingsOwnedAxes`) stamps both axes with no provider branch.
- `claudetui.Config` gains `SystemPrompt` + `DisallowedTools`.
  `ConfigFromOptions` maps the prompt through and takes the tool list
  **raw** from `opts.DisabledTools` rather than off
  `claude.ConfigFromOptions`'s merged field: `EnforcesRuntimeMode` is
  false here (the real TUI owns approvals), so the read-only mode strip
  must stay inert. It still passes through the shared argv-safety pass,
  `claude.SanitizeDisallowedTools`, exported so the two transports
  cannot drift on what "one safe CLI argument" means.
- The prompt file is written by `claude.WriteSystemPromptFile` (one
  writer, one mode, one removal contract) and removed by
  `claudetui.Session.Close`, which `NewSession`'s deferred cleanup also
  runs, so every failed-launch path drops it.
- `{{MEMORY_DIR}}` and `ensureClaudeMemoryDir` cover claude-tui:
  same binary, same `<claudeHome>/projects/<slug>/memory`, and the CLI
  stops creating it under a replaced prompt either way.
- Reconcile pins for claudetui: claudetui has no live-update surface
  (any diff is a restart), so the pin in `reconcileSettingsOwnedAxes` is
  what keeps a settings edit from restarting a live TUI session.

### Why Claude gets the FILE flag

`--system-prompt-file <path>` is wire-identical to `--system-prompt
<text>` (byte-identical `/v1/messages` requests, verified 2.1.234, see
[claude-wire.md §Related flags](../references/claude-wire.md#related-flags-verified-21234)),
so the choice is purely about what argv can safely carry, and argv is
the wrong channel twice over:

- **`MAX_ARG_STRLEN`** caps one argv string at 128KB on Linux and is not
  tunable. A rendered override can cross it (`{{GIT_BLOCK}}` expands to
  a repository snapshot), and then every spawn fails with E2BIG, a
  session that simply refuses to start.
- **argv is world-readable** through `/proc/<pid>/cmdline`. The rendered
  prompt carries workspace paths and git state; the prompt file is 0600.

`claude.WriteSystemPromptFile` materializes it at spawn in the app data
directory's `claude-prompts/`; `Session.Close` and every failed-spawn path
remove it, and boot empties the directory for a backend that exited
without closing its sessions. The rendered prompt is additionally
capped at `promptoverride.MaxRenderedPromptBytes` (256KB) at composition time.
Rendering is multiplicative, so a prompt inside the settings length
limit can still render to tens of megabytes. Over the cap the spawn
fails with both sizes named; it is never silently truncated.

## Settings shape

```go
// internal/settings/promptoverrides.go
type PromptOverride struct {
    ID      string   `json:"id"`                // canonical UUID, names the prompt file
    Enabled bool     `json:"enabled"`
    Models  []string `json:"models"`            // normalized slugs, no context-tier markers; [] never null
    Prompt  string   `json:"prompt,omitempty"`  // text of <configDir>/prompts/<id>.md; never on the wire
    Path    string   `json:"path,omitempty"`    // computed, never persisted
    Problem string   `json:"problem,omitempty"` // computed, never persisted
    Blank   bool     `json:"blank,omitempty"`   // wire only: the file holds no text that applies
}

ClaudePromptOverrides []PromptOverride `json:"claudePromptOverrides,omitempty"`
CodexPromptOverrides  []PromptOverride `json:"codexPromptOverrides,omitempty"`
ClaudeDisabledTools   []string         `json:"claudeDisabledTools,omitempty"`
CodexDisabledTools    []string         `json:"codexDisabledTools,omitempty"`
ClaudeTodoRemindersDisabled bool       `json:"claudeTodoRemindersDisabled,omitempty"`
ClaudeMaxCommandTimeoutHours int       `json:"claudeMaxCommandTimeoutHours"` // default 5, range 2-168
AgentGuideEnabled     bool             `json:"agentGuideEnabled"` // default true
```

Decisions (2026-08-17, with user):

- **List of entries per provider**, first enabled entry whose `Models`
  contains the session's model wins. Rationale: prompt text is
  model-shaped on both sides: the default Claude prompt differs Fable
  vs Opus, and Codex ships a different catalog template per slug.
- **Tool disabling is provider-wide**, not model-scoped.
- Matching normalizes via `provider.NormalizeModelSlug` and strips the
  context-tier marker (`[1m]`) before comparison.

On the wire (`GetSettings`, `UpdateSettings` and the provider env
replies, all through `redactedSettings`) an entry carries `id`,
`enabled`, `models`, `path`, `problem` and `blank`, never `prompt`
(`settings.PromptOverridesForWire`). `blank` is true when the file is
empty, whitespace only, or has a `problem`. `Prompt` still decodes from a
stored row, because `MigratePromptFiles` reads the inline text of rows
from before the files.

`ClaudeDisabledTools` holds raw tool names (unknown names are harmless
to the CLI). `CodexDisabledTools` holds **curated toggle ids**. Codex
has no flat disallow list; each id maps to config keys (see below).

### Prompt files

Decisions 2026-10-11 and 2026-10-14 (with user): each override's text
lives only in its file, which the user or an agent edits; Settings has no
text editor. `internal/settings/promptfiles.go` owns the mechanism.

- **Location.** `<configDir>/prompts/<id>.md` (directory 0700, file
  0600, written atomically through `atomicfile`). The settings row in the
  user tier holds `id`, `enabled` and `models` only. The load fills
  `Prompt` from the file in memory; it never crosses the wire.
- **Ids.** `Service.mutate` mints a UUID for an entry without one. A
  write that names an id no current entry has gets a fresh one, so a
  client never chooses the filename. An id that is not a UUID, or that
  repeats within or across the two provider lists, is rejected on write
  and cleared on load. A `.md` file in the directory with no entry is
  ignored.
- **Writes.** A write ignores any text a client sends
  (`restoreStoredPromptTexts`). Adding an entry creates its empty file;
  removing one deletes its file. `Path` and `Problem` are computed on
  load; a value sent by a client is ignored.
- **External edits.** `Get` stats every prompt file and reloads when one
  changed, so the next read sees an edit. `assetwatch.PromptWatcher`
  watches the directory; on an edit the app calls `ReloadPromptFiles`,
  which announces both override keys to attached clients, and the
  settings change observer then flags the live threads the edit affects
  ([Reconcile semantics](#reconcile-semantics-what-a-settings-edit-does-to-a-live-session)).
  The watcher does not suppress the app's own writes: they only create or
  delete an entry's file, and the reload they cause changes nothing.
- **Limits.** A file over `MaxPromptOverrideLen`, or one that carries its
  provider's list past `MaxPromptOverridesTotalLen`, loads as an empty
  prompt with `Problem` naming the path and size. The entry stays in the
  list, so a later save cannot drop it, and an empty prompt never matches.
- **README.** Boot writes `<configDir>/prompts/README.md`
  (`settings.PromptReadmeName`) from `promptoverride.ReadmeMarkdown`: the
  file naming and Settings mapping, the placeholder table
  (`promptoverride.Placeholders`), the limits, and when an edit applies.
  A user can point an agent at it.
- **Migration.** `MigratePromptFiles` runs after `AttachTierStore` at
  startup: a row that still carries inline text, or lacks an id, gets an
  id and a file, and the row is persisted without the text. Before the
  tier store is attached (the pre-database boot readers and a store-less
  service) the text stays inline in the row.

## Prompt application

Composition point: `app_session_prompt_override.go
applySettingsOwnedAxes`, called by the spawn path right after
`buildSessionOptions`. Precedence:

1. Feature-owned prompts win: if `designCfg.Prompt` or
   `threadSystemPrompt` (discussions) is non-empty, `buildSessionOptions`
   has already written it into `opts.SystemPrompt` and the override is
   **skipped**. Those features already run fully-custom prompts.
   `featureOwnedSystemPrompt` is the one definition of that rule, and
   after `buildSessionOptions` a non-empty `opts.SystemPrompt` means
   exactly "a feature owns this", so nothing downstream re-derives it.
2. Otherwise the matching enabled override is rendered and becomes
   `opts.SystemPrompt`. Workflow-engine unit sessions are plain
   sessions and deliberately DO get the override.

`applySettingsOwnedAxes` takes ONE `settings.Get()` snapshot and returns
a `promptOverrideResolution` (the matched entry plus whether it applied),
so every downstream consumer (today `ensureClaudeMemoryDir`) acts on
the same decision from the same snapshot. A settings save landing
mid-spawn cannot make the created directory disagree with the rendered
prompt.

### Placeholder rendering

Rendered at spawn from values AO already has. The user-facing meaning of
each token and the providers that render it are
`promptoverride.Placeholders`, which the Settings legend and the prompts
README both follow:

| Placeholder | Source |
|---|---|
| `{{WORKDIR}}` | thread workspace path |
| `{{IS_GIT_REPO}}` / `{{GIT_BLOCK}}` | git probe at spawn; `GIT_BLOCK` = branch/status/recent-commits snapshot (`git.PromptSnapshot.PromptBlock`), empty outside a repo. A prompt using only `IS_GIT_REPO` is answered by `gitroot.MainRoot`: a filesystem read, zero subprocesses |
| `{{PLATFORM}}` / `{{OS_VERSION}}` | runtime |
| `{{MODEL_NAME}}` / `{{MODEL_ID}}` | thread model |
| `{{MEMORY_DIR}}` | `<claudeHome>/projects/<slug>/memory` (slug = workdir, non-alphanumerics → `-`; same layout `internal/sessionimport` walks) |

Claude **needs** env templating. Replacement kills the CLI's
Environment section. Codex mostly does not: `include_environment_context`
(default true) keeps env context flowing independently of
`baseInstructions`; the same renderer runs for both anyway.

Only known tokens are substituted; any other `{{...}}` text passes
through literally (user decision 2026-08-17: typos stay visible in the
session rather than erroring or being stripped, and prose containing
braces never fights the renderer). The settings UI shows a read-only
legend of the available placeholders beside each provider's overrides.

### Memory (Claude)

Under a replaced system prompt the CLI stops mkdir-ing the memory dir,
but recall (MEMORY.md → first-user-message system-reminder) still works.
When an override containing `{{MEMORY_DIR}}` is active, AO creates the
directory at spawn so the prompt's "it already exists" claim holds.
`ensureClaudeMemoryDir` runs on the spawn path, the one place an
override is rendered, takes the resolution rather than re-matching, and
is never fatal: a failure lands as thread error state and the session
starts anyway.

### Codex specifics

- Send `baseInstructions` on `thread/start` **and** `thread/resume`: a
  cold resume without it inherits the rollout's stored instructions, not
  the catalog default. `buildThreadParams` covers both, which is what
  also carries the disabled-tool `config` keys onto a resume.
- `thread/fork` carries **neither**: `ForkAt` sends `threadId`,
  `excludeTurns: true`, the optional `lastTurnId` anchor, and the
  session's composed `developerInstructions` ([App guide](#app-guide)).
  That is safe for the same reason it is safe for model / sandbox /
  reviewer: nothing executes on a forked thread until AO spawns against
  it, and that spawn is a `thread/resume` which re-asserts every axis. So
  the axes to get right on a fork are the ones on its NEXT launch config,
  not on the fork call.
- Replacement is verbatim-total on the Responses `instructions` field.
  AGENTS.md content, personality consistency, model-switch re-injection,
  and compaction all survive (see reference doc). The stock prompt's
  AGENTS.md-interpretation, `apply_patch`, and `update_plan` guidance
  do not. A Codex override prompt should carry its own versions.

## Tool disabling

**Claude:** union the settings list into `cfg.DisallowedTools` alongside
the read-only strips (`claudeDisallowedTools`). `--disallowedTools`
removes the schema from the request entirely (verified 2.1.234) and
composes with the system-prompt replacement. Spawn-only, consistent
with the existing read-only doctrine in
`internal/provider/claude/options.go`. `mergeDisallowedTools` re-validates
each name at the argv boundary (dropping empties, names containing
whitespace, and names starting with `-`, each with a log line): Settings
validation is the primary gate and the one that tells the user, but a
name that is not ONE safe CLI argument must be impossible to reach argv
regardless of how `Config` was built. `claudetui` calls the same
exported `SanitizeDisallowedTools` on the settings list ALONE (no mode
strip, see the claude-tui section above), so both transports share one
definition of an admissible name.

Note when reading a Claude list: `Task` and `Agent` are aliases in the
CLI, so disallowing either removes both.

### The todo tool group + nudge toggle (2026-08-19, with user)

Claude's todo family (`TodoWrite`, `TaskCreate`, `TaskUpdate`,
`TaskGet`, `TaskList`, listed as `CLAUDE_TODO_TOOL_GROUP` in
`frontend/src/lib/utils/promptOverrides.ts`) is managed as ONE group in
the editor: the tools only make sense as a set, so the group switch adds
or removes all five from `ClaudeDisabledTools` and a per-tool disclosure
covers partial sets. Storage is unchanged. The group is a UI
projection over the same flat list, and the five names are absent from
the flat suggestion row. `TodoWrite` is in the group because pre-gate
models (opus <4.8, sonnet/fable <5) keep it regardless of
`CLAUDE_CODE_ENABLE_TODO_TOOLS`, so `--disallowedTools` is the only off
switch that works on every model.

`ClaudeTodoRemindersDisabled` is the companion nudge toggle: it exports
`CLAUDE_CODE_TODO_REMINDER_MODE=off` into Claude spawns (headless
`withClaudeSessionEnvDefaults` and claudetui `buildEnv`, both as a
DEFAULT a custom-env value outranks, the same posture as the todo-tools
opt-in, see `internal/provider/pinnedenv.go`). It rides
`SessionOptions.DisableTodoReminders`, the third settings-owned axis:
stamped by `applySettingsOwnedAxes`, pinned by
`reconcileSettingsOwnedAxes`.
The CLI double-gates its task_reminder nudge on the reminder mode AND
the tools' presence in the session (verified 2.1.233), so the toggle
only matters while ≥1 group tool is exposed. The UI disables it when
the whole group is off, and turning the group off needs no reminder
write at all.

`ClaudeMaxCommandTimeoutHours` is the Command timeout row in the same
section: exported as `BASH_MAX_TIMEOUT_MS` (hours in ms) into every
Claude spawn, headless (`claude.WithCommandTimeoutEnv` in
`claudeSpawnEnv`) and claude-tui (the same helper on the override map
before `BuildEnvironment`), as a DEFAULT a custom-env value outranks; the
row names an overriding custom-env entry. Precedence on both paths:
Claude's own `settings.json` `env` block, then the custom environment,
then the setting, then an inherited host value. It rides
`SessionOptions.ClaudeMaxCommandTimeoutHours`, stamped and pinned like
`DisableTodoReminders`. The CLI behavior it controls is in
`docs/references/claude.md` (background commands stop at their timeout).

**Codex:** curated toggles, each mapping to per-thread `config` map
entries (all verified per-conversation-settable):

| Toggle id | Config |
|---|---|
| `web_search` | `web_search = "disabled"` |
| `update_plan` | `tools.update_plan.enabled = false` |
| `view_image` | `features.view_image = false` |
| `request_user_input` | `tools.experimental_request_user_input.enabled = false` |
| `collab_agents` | `agents.enabled = false`, `features.multi_agent_v2 = false`, `features.multi_agent = false` |
| `image_generation` | `features.image_generation = false` |
| `tool_suggest` | `features.tool_suggest = false` |

Deliberately not exposed: shell/unified-exec (session-lobotomizing) and
`apply_patch` (catalog-driven, only removable via startup-only
`model_catalog_json`). Enterprise-managed installs can veto some keys;
a refused override surfaces as the thread error it produces.

## Reconcile semantics: what a settings edit does to a LIVE session

Settings are read at spawn, so new sessions always pick changes up. What
a change does to a session that is already running depends on the axis.

The live-config reconciler (`app_session_config.go
liveApplySessionConfig`) diffs freshly built options against
`sess.launchOpts` and escalates non-live-appliable diffs to a deferred
restart. Before diffing it calls **`reconcileSettingsOwnedAxes`**
(`app_session_prompt_override.go`), which resolves the settings-owned
axes:

| Axis | Live session behavior |
|---|---|
| `DisabledTools` | **Pinned** to `sess.launchOpts`. Spawn-only on every provider, so a diff could only queue a restart nobody asked for. |
| `DisableTodoReminders` | **Pinned**, same reason (an env var, fixed at spawn). |
| `ClaudeMaxCommandTimeoutHours` | **Pinned**, same reason. |
| `SystemPrompt`, settings override, every provider | **Pinned; per-thread `prompt_stale` banner.** A prompt change never applies to a running session (decision 2026-10-14): no `set_model.system_prompt` swap, no deferred restart. Nothing is matched or rendered on the reconcile path. It applies at the thread's next session start; until then the thread shows the banner described below. |
| `SystemPrompt`, feature-owned | **Untouched.** A non-empty `opts.SystemPrompt` after `buildSessionOptions` means a discussion owns it; it converges by deferred restart, since `claude.PlanLiveUpdate` carries no prompt and any prompt diff falls through to its spawn-only comparison. |
| `ClaudeThinking`, headless Claude on CLI ≥ 2.1.214 | **Converges live** for every explicit value. `claude.PlanLiveUpdate` carries it as `LiveUpdate.Thinking` and `ApplyLiveUpdate` sends one `set_max_thinking_tokens` control request (`max_thinking_tokens` for off/budget, `thinking_display` for the display axis). Returning to Claude's own choice is a **deferred restart**: `max_thinking_tokens: null` is accepted and does nothing, so only a respawn without the flag resets it. Below the version floor, or with the version unknown, the change is a deferred restart. |
| `ClaudeThinking`, Codex / claude-tui | **Never resolved at all.** `ClaudeThinkingForProvider` answers the zero value for anything but headless `claude`, on BOTH paths. claude-tui drives the same binary but has no control-request channel, so an axis it could never apply must not diff either. |

`applySettingsOwnedAxes` (spawn) and `reconcileSettingsOwnedAxes`
(reconcile) are a **pair**, and `buildSessionOptions` stamps none of
these axes so the pairing cannot be forgotten:

- `claudeThinking` and `claudeCrossSession` are the keys a *settings save
  alone* has to act on: every other axis here is spawn-only, so nothing
  would reconcile until an unrelated thread-config edit happened to.
  `UpdateSettings` therefore fans out `reconcileLiveClaudeSessions` on
  those keys (`liveClaudeSettingsAxes`), off the binding's goroutine.
  Neither prompt override key is in that list.
- Adding a settings-owned axis means touching BOTH. An axis stamped only
  on the spawn side would queue a deferred restart on every live session
  the next time anything reconciled, the failure the pin exists to
  prevent, re-introduced silently.

### Prompt changed banner

`app_prompt_stale.go` owns it, beside the stale-binary banner
(`app_provider_binary_watch.go`).

- **Stale.** A live session not running a feature-owned prompt is stale
  when the stored override text its next start would use
  (`promptoverride.Match` on today's settings for the thread's provider
  and model; an entry whose file is blank or has a `Problem` is no
  override) differs from `launch.SystemPromptOverrideSource`. Decided on
  the stored, unrendered text, so a workspace's git state moving under a
  `{{GIT_BLOCK}}` never flags anything.
- **Triggers.** The settings change observer on either override key (an
  edit in Settings or a prompt file reload), a session start (a fresh
  launch matches, and an edit that landed during the spawn flags it), a
  thread model change and a provider model reroute recompute it. A
  session leaving the session map (stop, restart, death, reap) withdraws
  its flag at once.
- **Wire.** Entering or changing within stale emits a thread-scoped
  `provider:status` `{kind:"prompt_stale", threadId, provider,
  actionable:true, message, revision}`. `revision` increases on every
  change and is never reused. Clearing emits `{status:"ready", threadId,
  provider, withdrawKind:"prompt_stale"}`. The flag is pinned to the
  session token that earned it, and `GetThreadLiveState` mirrors it as
  `promptStale` and `promptStaleRevision` for a reconnecting pane.
- **Dismiss.** `DismissSessionBanner(threadID, "prompt_stale")` records
  the dismissed revision and emits the withdraw; hydration omits a
  dismissed banner, and the next revision shows it again. Ignored or
  dismissed, the change applies when the session next restarts.
  `DismissSessionBanner(threadID, "binary_stale")` dismisses the
  stale-binary banner for the installed version it names, until that
  version changes again; its withdrawals carry
  `withdrawKind:"binary_stale"`.

## App guide

The app guide is a short text Agent Overflow appends to the system
prompt of interactive Claude, claude-tui and Codex sessions: how the chat
pane renders replies, then one sentence per app-managed tool server the
session has on. The text and its composition live in
`internal/app/app_agent_guide.go`. `TestAgentGuideNamesEveryAppManagedServer`
requires a sentence for every name in `appManagedMCPServers`, so a new
app-managed server adds one.

- **Setting.** `AgentGuideEnabled`, user tier, default on, on the App
  guide page of the Agents group. Off, no session carries the guide.
- **Scope.** `agentGuideFor` answers the guide for those three providers
  only, and never for a workflow-mode thread (`threadmode.ModeWorkflow`:
  phases and units, attached to an attempt or not): a workflow session
  runs under its workflow's prompt and structured output, and its tool
  servers are granted per phase.
- **Servers named.** Only servers the session gets tools from
  (`agentGuideServersOn`): the browser server when it is registered, the
  global `BrowserEnabled` switch is on and the thread's browser switch is
  on; the thread tools when registered and `threadToolsEnabledFor` holds;
  the remote server when registered. With none on, the tools paragraph is
  omitted. The set is read at spawn, so a live server switch reaches the
  text at the next start or resume.
- **Spawn-only on every provider.** The guide rides the provider
  `Config`, not `SessionOptions`, so it is outside the
  `applySettingsOwnedAxes` / `reconcileSettingsOwnedAxes` pair: a
  settings change never queues a restart and lands at the session's next
  start or resume.

### Delivery

- **Claude and claude-tui:** `Config.AppendSystemPrompt`, passed as
  `--append-system-prompt-file <path>`: a second 0600 file from
  `claude.WriteSystemPromptFile`, removed with the replacement file on
  `Close` and on a failed spawn. The CLI places it after whichever prompt
  the session otherwise runs with (the default body, a settings override
  or a feature-owned prompt), joined by a blank line
  ([claude-wire.md §Appended prompt and prompt snapshot](../references/claude-wire.md#appended-prompt-and-prompt-snapshot-verified-21284)).
- **Codex:** `codexDeveloperInstructions` joins, with blank lines, the
  app guide, the browser server's guide (`appbrowser.Instructions()`)
  when the browser tools are on, and the thread tools guide when those
  are on. Codex never shows a server's MCP `instructions` to the model,
  so the server guides reach it only this way; they follow their
  servers, not `AgentGuideEnabled` or the phase exclusion. The provider
  appends the result to the cwd's configured `developer_instructions`
  (read with `config/read`) and sends it as `developerInstructions` on
  `thread/start` and `thread/resume`; `ForkAt` carries the composed value
  forward. An empty composition omits the field. A failed `config/read`
  also omits it rather than replace instructions AO could not read, and
  reports a non-fatal error on the thread
  ([codex-instructions-tools.md §App-owned appended guidance](../references/codex-instructions-tools.md#app-owned-appended-guidance)).

### Claude prompt snapshot

By default the CLI records the first request's rendered system prompt in
the transcript and reuses it on every `--resume`. With that default a
resumed session keeps its first guide and ignores a changed or removed
`--system-prompt-file`, so neither the guide nor an edited override
reaches an existing conversation, and the banner's Restart would resume
the old prompt. Both Claude providers therefore pass
`--system-prompt-snapshot off` (`claude.SystemPromptSnapshotArgs`)
whenever the installed version is known and at least 2.1.267, with or
without a guide or override. Off, the prompt renders fresh at every
start; the default body is byte-identical across resumes of one build,
so the prompt cache is unaffected.

The version comes from `installedProviderVersion`: the token the last
probe recorded (the boot status probe in `GetProviderStatuses` or the
binary watcher), trusted only while the binary on disk still has the
probed identity. A build below 2.1.267 rejects the flag and the spawn
fails, so an unknown version omits it and that launch behaves as the CLI
default.

## UI

New section in the Agents group (`sections.ts`): per provider, a list
of override entries plus the disabled-tools row (curated toggle set for
Codex; known-tool chips + free-form add for Claude). There is no prompt
text editor (decision 2026-10-14): the text is the entry's file.

- An entry shows its enable toggle, model multi-select chips (the same
  interaction as `ProviderModelChips`, but a selection set rather than a
  hide-list), its prompt file path with an icon-only Open button
  (`openInEditor`, offered only where the page has host scope), and a
  warning while `blank` is true: its `problem` when it has one,
  otherwise that the file is empty.
- Adding an entry creates its empty file; removing one deletes it.
- A copy-instructions control copies text pointing an agent at the
  prompts directory and its README.
- The placeholder legend is read-only.

## Non-goals / later

- Per-thread overrides (settings-level only for now).
- Editing the *default* prompts (there is no supported way to read
  Codex's effective default over the wire; Claude's is capturable via
  the ANTHROPIC_BASE_URL sink method in claude-wire.md).
- MCP tool toggles (already covered by MCP server management).
- User-authored appended text (Codex `developerInstructions`, Claude
  `--append-system-prompt-file`) as a lighter-than-replacement channel.
  The app uses both only for its own guides.
