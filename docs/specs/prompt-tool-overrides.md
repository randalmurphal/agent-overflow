# System-prompt overrides + tool toggles

Status: implemented. Wire/source facts backing every
mechanism here:
[claude-wire.md §System prompt assembly](../references/claude-wire.md#system-prompt-assembly--system-prompt-replacement-verified-21234)
and [codex-instructions-tools.md](../references/codex-instructions-tools.md).

## Goal

A settings surface (Agents group) where the user can:

1. **Replace the provider's default system prompt** per provider, scoped
   to selected models — Claude via `--system-prompt`, Codex via
   `baseInstructions`. Toggleable per entry; takes effect on the next
   session started, never disturbing running sessions.
2. **Disable built-in tools** per provider so their schemas never reach
   the model context (e.g. Claude's `Workflow` — the user runs AO's own
   workflow engine instead). Claude's tools payload is ~92KB of schemas,
   so this is the bigger context lever.

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
  file's content] — full body replacement, same as headless. Only the
  identity line differs from the headless SDK one; it is not
  replaceable on either.
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
  must stay inert. It still passes through the shared argv-safety pass
  — `claude.SanitizeDisallowedTools`, exported so the two transports
  cannot drift on what "one safe CLI argument" means.
- The prompt file is written by `claude.WriteSystemPromptFile` (one
  writer, one mode, one removal contract) and removed by
  `claudetui.Session.Close`, which `NewSession`'s deferred cleanup also
  runs — so every failed-launch path drops it.
- `{{MEMORY_DIR}}` and `ensureClaudeMemoryDir` cover claude-tui:
  same binary, same `<claudeHome>/projects/<slug>/memory`, and the CLI
  stops creating it under a replaced prompt either way.
- Reconcile is unchanged: `pinSettingsOwnedAxes` is generic and runs
  before the diff, and claudetui has no live-update surface (any diff
  is a restart), so the pin is what keeps a settings edit from
  restarting a live TUI session.

### Why Claude gets the FILE flag

`--system-prompt-file <path>` is wire-identical to `--system-prompt
<text>` (byte-identical `/v1/messages` requests, verified 2.1.234 —
[claude-wire.md §Related flags](../references/claude-wire.md#related-flags-verified-21234)),
so the choice is purely about what argv can safely carry, and argv is
the wrong channel twice over:

- **`MAX_ARG_STRLEN`** caps one argv string at 128KB on Linux and is not
  tunable. A rendered override can cross it (`{{GIT_BLOCK}}` expands to
  a repository snapshot), and then every spawn fails with E2BIG — a
  session that simply refuses to start.
- **argv is world-readable** through `/proc/<pid>/cmdline`. The rendered
  prompt carries workspace paths and git state; the temp file is 0600.

`writeSystemPromptFile` materializes it at spawn; `Session.Close` and
every failed-spawn path remove it. The rendered prompt is additionally
capped at `maxRenderedSystemPromptBytes` (256KB) at composition time —
rendering is multiplicative, so a prompt inside the settings length
limit can still render to tens of megabytes. Over the cap the spawn
fails with both sizes named; it is never silently truncated.

## Settings shape

```go
// internal/settings/settings.go
type PromptOverride struct {
    Enabled bool     `json:"enabled"`
    Models  []string `json:"models"`  // normalized slugs, no context-tier markers
    Prompt  string   `json:"prompt"`
}

ClaudePromptOverrides []PromptOverride `json:"claudePromptOverrides,omitempty"`
CodexPromptOverrides  []PromptOverride `json:"codexPromptOverrides,omitempty"`
ClaudeDisabledTools   []string         `json:"claudeDisabledTools,omitempty"`
CodexDisabledTools    []string         `json:"codexDisabledTools,omitempty"`
```

Decisions (2026-08-17, with user):

- **List of entries per provider**, first enabled entry whose `Models`
  contains the session's model wins. Rationale: prompt text is
  model-shaped on both sides — the default Claude prompt differs Fable
  vs Opus, and Codex ships a different catalog template per slug.
- **Tool disabling is provider-wide**, not model-scoped.
- Matching normalizes via `provider.NormalizeModelSlug` and strips the
  context-tier marker (`[1m]`) before comparison.

`ClaudeDisabledTools` holds raw tool names (unknown names are harmless
to the CLI). `CodexDisabledTools` holds **curated toggle ids** — Codex
has no flat disallow list; each id maps to config keys (see below).

## Prompt application

Composition point: `app_session_prompt_override.go
applySettingsOwnedAxes`, called by the spawn path right after
`buildSessionOptions`. Precedence:

1. Feature-owned prompts win: if `designCfg.Prompt` or
   `threadSystemPrompt` (discussions) is non-empty, `buildSessionOptions`
   has already written it into `opts.SystemPrompt` and the override is
   **skipped** — those features already run fully-custom prompts.
   `featureOwnedSystemPrompt` is the one definition of that rule, and
   after `buildSessionOptions` a non-empty `opts.SystemPrompt` means
   exactly "a feature owns this", so nothing downstream re-derives it.
2. Otherwise the matching enabled override is rendered and becomes
   `opts.SystemPrompt`. Workflow-engine unit sessions are plain
   sessions and deliberately DO get the override.

`applySettingsOwnedAxes` takes ONE `settings.Get()` snapshot and returns
a `promptOverrideResolution` (the matched entry plus whether it applied),
so every downstream consumer — today `ensureClaudeMemoryDir` — acts on
the same decision from the same snapshot. A settings save landing
mid-spawn cannot make the created directory disagree with the rendered
prompt.

### Placeholder rendering

Rendered at spawn from values AO already has:

| Placeholder | Source |
|---|---|
| `{{WORKDIR}}` | thread workspace path |
| `{{IS_GIT_REPO}}` / `{{GIT_BLOCK}}` | git probe at spawn; `GIT_BLOCK` = branch/status/recent-commits snapshot (`git.PromptSnapshot.PromptBlock`), empty outside a repo. A prompt using only `IS_GIT_REPO` is answered by `gitroot.MainRoot` — a filesystem read, zero subprocesses |
| `{{PLATFORM}}` / `{{OS_VERSION}}` | runtime |
| `{{MODEL_NAME}}` / `{{MODEL_ID}}` | thread model |
| `{{MEMORY_DIR}}` | `<claudeHome>/projects/<slug>/memory` (slug = workdir, non-alphanumerics → `-`; same layout `internal/sessionimport` walks) |

Claude **needs** env templating — replacement kills the CLI's
Environment section. Codex mostly does not: `include_environment_context`
(default true) keeps env context flowing independently of
`baseInstructions`; the same renderer runs for both anyway.

Only known tokens are substituted; any other `{{...}}` text passes
through literally (user decision 2026-08-17: typos stay visible in the
session rather than erroring or being stripped, and prose containing
braces never fights the renderer). The settings UI shows a legend of
the available placeholders next to the prompt editor.

### Memory (Claude)

Under a replaced system prompt the CLI stops mkdir-ing the memory dir,
but recall (MEMORY.md → first-user-message system-reminder) still works.
When an override containing `{{MEMORY_DIR}}` is active, AO creates the
directory at spawn so the prompt's "it already exists" claim holds.
`ensureClaudeMemoryDir` runs on the spawn path only, takes the
resolution rather than re-matching, and is never fatal: a failure lands
as thread error state and the session starts anyway.

### Codex specifics

- Send `baseInstructions` on `thread/start` **and** `thread/resume`: a
  cold resume without it inherits the rollout's stored instructions, not
  the catalog default. `buildThreadParams` covers both, which is what
  also carries the disabled-tool `config` keys onto a resume.
- `thread/fork` carries **neither** — `ForkAt` sends only `threadId` and
  the optional `lastTurnId` anchor, and that is safe for the same reason
  it is safe for model / sandbox / reviewer: nothing executes on a forked
  thread until AO spawns against it, and that spawn is a `thread/resume`
  which re-asserts every axis. So the axes to get right on a fork are the
  ones on its NEXT launch config, not on the fork call.
- Replacement is verbatim-total on the Responses `instructions` field.
  AGENTS.md content, personality consistency, model-switch re-injection,
  and compaction all survive (see reference doc). The stock prompt's
  AGENTS.md-interpretation, `apply_patch`, and `update_plan` guidance
  do not — a Codex override prompt should carry its own versions.

## Tool disabling

**Claude:** union the settings list into `cfg.DisallowedTools` alongside
the read-only strips (`claudeDisallowedTools`). `--disallowedTools`
removes the schema from the request entirely (verified 2.1.234) and
composes with the system-prompt replacement. Spawn-only — consistent
with the existing read-only doctrine in
`internal/provider/claude/options.go`. `mergeDisallowedTools` re-validates
each name at the argv boundary (dropping empties, names containing
whitespace, and names starting with `-`, each with a log line): Settings
validation is the primary gate and the one that tells the user, but a
name that is not ONE safe CLI argument must be impossible to reach argv
regardless of how `Config` was built. `claudetui` calls the same
exported `SanitizeDisallowedTools` on the settings list ALONE (no mode
strip — see the claude-tui section above), so both transports share one
definition of an admissible name.

Note when reading a Claude list: `Task` and `Agent` are aliases in the
CLI, so disallowing either removes both.

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

## "Next sessions only" semantics

Settings are read at spawn, so new sessions pick changes up naturally.
The live-config reconciler (`app_session_config.go
liveApplySessionConfig`) diffs freshly built options against
`sess.launchOpts` and escalates non-live-appliable diffs to a deferred
restart — so it calls **`pinSettingsOwnedAxes`**, which adopts
`SystemPrompt` and `DisabledTools` from `sess.launchOpts` before
diffing. Otherwise editing the setting (or the workspace's git state
moving under a rendered `{{GIT_BLOCK}}`) would queue restarts on every
running session. Restarts that happen for other reasons build fresh
options and adopt the current setting; that is a new session and
intended.

`applySettingsOwnedAxes` (spawn) and `pinSettingsOwnedAxes` (reconcile)
are a **pair**, and `buildSessionOptions` stamps neither axis so the
pairing cannot be forgotten:

- Adding a settings-owned axis means touching BOTH. An axis stamped only
  on the spawn side would queue a deferred restart on every live session
  the next time anything reconciled — the failure the pin exists to
  prevent, re-introduced silently.
- The prompt pin is conditional (`if opts.SystemPrompt == ""`) because
  `SystemPrompt` is a SHARED axis: design mode and discussions own it for
  their threads, and an edit to a feature-owned prompt must keep
  converging through the reconciler exactly as it did before this feature
  existed.
- The reconcile path composes **nothing** — no `Match`, no `Render`, no
  git subprocess. It runs under the per-thread config lock on every
  model / effort / runtime-mode change, and the old shape paid for up to
  two git subprocesses there to produce a value it discarded on the next
  line.

## UI

New section in the Agents group (`sections.ts`): per provider, a list
of override entries (enable toggle, model multi-select chips — same
interaction as `ProviderModelChips` but a selection set, not a
hide-list — and a prompt editor), plus the disabled-tools row (curated
toggle set for Codex; known-tool chips + free-form add for Claude).
Settings JSON needs no migration (sparse write + unknown-field
preservation already handle new fields); frontend `types/settings.ts`
and the section registry are the only wiring.

## Non-goals / later

- Per-thread overrides (settings-level only for now).
- Editing the *default* prompts (there is no supported way to read
  Codex's effective default over the wire; Claude's is capturable via
  the ANTHROPIC_BASE_URL sink method in claude-wire.md).
- MCP tool toggles (already covered by MCP server management).
- `developerInstructions` (Codex) — exists on the same RPCs if a
  lighter-than-replacement channel is ever wanted.
