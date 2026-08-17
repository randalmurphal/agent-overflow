# internal/provider/

Owns every wire-level interaction with a coding-agent subprocess. Triage
and store consume the normalized output; nothing downstream should need
to know which provider produced an event.

## Layout

- `provider/` — shared types and interfaces: event shapes, approval
  kinds, `SessionOptions`, `RuntimeMode`, session-registry helpers.
- `provider/claude/` — Claude Code CLI integration. NDJSON over stdio.
  See its `AGENTS.md`.
- `provider/codex/` — Codex `app-server` integration. JSON-RPC 2.0 over
  stdio. See its `AGENTS.md`.

## Account probes

`claude.ProbeAccount`, `codex.ProbeAccount`, and `codex.ProbeIdentity`
share two rules, enforced in code rather than by caller discipline:

- **`ProbeConfig.WorkDir` is required and must be absolute**
  (`ValidateProbeWorkDir`, `probeworkdir.go`). Both CLIs discover
  project-scoped configuration by walking up from their cwd, and a
  project `.claude/settings.json` env block can repoint the CLI at a
  different backend — so an inherited cwd makes "who is logged in"
  depend on where the app was launched from.
- **`ProbeCacheKey` carries `(Binary, AccountID, WorkDir,
  EnvFingerprint)`** (`probecache.go`). Dropping any dimension serves one
  environment's identity as another's. `EnvFingerprint` is a digest (not
  the values — a cache key outlives the call that built it and a custom
  environment may hold a credential) of the user's configured environment
  for the provider: `ANTHROPIC_BASE_URL` and a proxy decide which backend
  answers "who am I", so the answer must not outlive the setting.

`ReservedEnvNames` (`pinnedenv.go`) is the source of truth for the
variables AO sets or clears itself, and therefore the ones a user's custom
environment may not override. `internal/settings` keeps a copy (it cannot
import this package) and `TestReservedEnvNamesMatchTheProviderPins` in the
root package fails if the two drift in either direction. A new pin in any
spawn path belongs in that list in the same commit.

The app pins one directory for every probe (`providerProbeWorkDir` in
`app_provider_account_env.go`, the user home) because AO's probe
consumers are global: managed-account adoption, external-login
reconciliation, and Claude's canonical-home OAuth refresh. Making probes
reflect the active thread's workspace instead is a product change, not a
bug fix — see `t3-improvements.md` §2.3.

## Child environment

`BuildEnvironment` and `FilterEnvironment` (`env.go`) are the two
entry points every provider child environment is assembled through —
`Spawn`, `claude.Login`, both MCP-status fetchers, `claudetui`'s full
`[]string` environment, and `textgen.ExecCLI`. Both apply `appimage.Scrub`
to the inherited environment, so a provider CLI launched from an AppImage
build resolves its runtime against the user's real system rather than a
squashfs mount that disappears when Agent Overflow exits (see
`internal/appimage/AGENTS.md`). It is marker-gated and idempotent: every
other launch shape is untouched, and an already-scrubbed environment
passed back in stays as it is.

The additive `PATH` merge reads the inherited half back off the *scrubbed*
base, not `os.Getenv`. That ordering is load-bearing — reading the live
variable would re-admit the mount's bin directory through the override
path, which is the one hole a scrub applied "at the end" would leave open.
`runVersionCommand` (`detect.go`) takes the same scrub via
`appimage.ScrubInherited`.

## Model catalogs

`ClaudeModels` / `CodexModels` (`models.go`) are the shipped catalogs.
`Capabilities.ModelCatalog` declares what each provider does with its own:

| Source | Provider | Meaning |
|---|---|---|
| `StaticModelCatalog` | (default) | The shipped list is the whole answer. |
| `CodexLiveModelCatalog` | codex | app-server `model/list` REPLACES it; a miss is authoritative. |
| `ClaudeProbeEnrichedCatalog` | claude, claude-tui | The shipped list is MERGED with the `models` array the zero-token account probe's `initialize` response carries. |

Claude's enrichment lives in `internal/claudemodels` (policy + rationale in its
`AGENTS.md`) and rides on `claude.ProbeConfig.OnModels` — no second subprocess,
keyed by the probe's own `ProbeCacheKey`. Two rules it enforces that callers
here depend on:

- **The wire never subtracts.** Its list is the CLI's five-row picker
  shortlist, which omits still-usable older models, so absence is not a denial
  and no catalog model is ever dropped or reordered.
- **The catalog owns context windows; the wire owns capability flags.** The
  wire reports no windows at all, and it is the running binary's own answer
  about what a model supports. Disagreements are logged as drift, never toasted.

Per-model CLI-version minimums (`t3-improvements.md` §2.5) are deliberately NOT
built: the running binary's model list is a better answer than a hand-maintained
version table. `detect.go`'s whole-provider Codex gate is unrelated — it guards
protocol features, not model availability.

Two rules that keep a catalog lookup from silently degrading:

- **A model id is never its context tier.** `NormalizeModelSlug` trims the
  trailing `[1m]` marker for Claude (`TrimContextMarker` / `HasContextMarker`
  are the shared pair; `claude.claudeModelForContextWindow` is the inverse).
  Marker-carrying ids arrive from both directions — the CLI's model list bakes
  them into id strings, and anything reading a launched model id back sees what
  we sent — and a `FindModel` miss would quietly downgrade effort tiers,
  fast-mode support, and context-window options to provider defaults.
- **"No reasoning tiers" is an argv answer, not a stored one.**
  `ModelDeclaresNoReasoningEffort` is what every argv builder asks
  (`claude.claudeEffortForModel`, `coerceTextGenerationEffort`) so a model like
  Haiku gets no `--effort` flag at all. The `Coerce*` family cannot answer it:
  `threads.reasoning_effort` and `chat_model_profiles.reasoning_effort` are NOT
  NULL under a per-provider CHECK, so what persists is always a legal tier.
  Coercing an effortless model onto the provider default instead would raise
  cost silently — it did, on the text-generation path, until the flag moved
  into `textgen.RunClaude` / `RunCodex`.

## Responsibility boundary

- What BELONGS here:
  - Process lifecycle: `Start → stream → Stop`. Spawn, read, write, signal.
  - Wire-format parsing and normalization into provider-agnostic events.
  - Per-provider option hydration (`Config.From(SessionOptions)`).
- What does NOT belong here (goes elsewhere):
  - SQLite writes — `triage` decides what persists and `store` runs the
    SQL.
  - `app.Event.Emit` calls — `app.go` owns the frontend fan-out.
  - Cross-thread orchestration or reconnect policy — the session
    registry in `app.go` owns lifecycle decisions.

## RuntimeMode

Five tiers on one axis (`runtime_modes.go`), ordered most- to
least-restrictive on mutation: `read-only`, `approval-required`,
`auto-accept-edits`, `auto`, `full-access`. Provider packages own the wire
mapping; `AllRuntimeModes` is the canonical list and both
`NormalizeRuntimeMode` and `threadmode.ParseRuntime` derive membership from it,
so a new tier cannot be legal in one place and coerced away in another.

| Tier | Claude (CLI 2.1.219) | Codex (app-server, floor 0.143) |
|---|---|---|
| `read-only` | `--permission-mode dontAsk` + `--disallowedTools Write,Edit,NotebookEdit` | approval `never`, sandbox `read-only`, reviewer `user` |
| `approval-required` | `--permission-mode default` (no flag) | approval `untrusted`, sandbox `read-only`, reviewer `user` |
| `auto-accept-edits` | `--permission-mode acceptEdits` | approval `on-request`, sandbox `workspace-write`, reviewer `user` |
| `auto` | `--permission-mode auto` | approval `untrusted`, sandbox `read-only`, reviewer `auto_review` |
| `full-access` | `--permission-mode bypassPermissions` + `--allow-dangerously-skip-permissions` | approval `never`, sandbox `danger-full-access`, reviewer `user` |

Two tiers can **refuse** an action rather than ask about it, for different
reasons, and neither is a fallback:

- `read-only` is the unattended tier — the only mode that denies by rule, so a
  workflow phase running with no human present is refused rather than left
  waiting.
- `auto` denies by judgement — a model reviewer adjudicates each escalation.
  It is an interactive tier: both providers can still fall back to a real
  interactive request (Claude on `safety_check` / `ask_rule` /
  `plan_mode_floor`), so the ordinary approval plumbing stays live and a human
  has to be there for the fallback. It also costs money per decision (Claude
  bills a Haiku classifier turn, Codex an `auto_review` subagent), which is why
  the composer copy states both caveats. Workflow phases deliberately cannot
  reach it — `def.Access` is a closed `read-only` / `write` set and
  `workflowPhaseRuntimeMode` maps only onto those two tiers.

Rules that follow:

- **Never make either a fallback.** `DefaultRuntimeMode` is `full-access`; an
  unknown or corrupt value must not collapse into a restricted session, must
  not silently widen a restricted one, and must not opt a thread into a billed
  reviewer. Every per-provider mapping switch enumerates all five modes and the
  trailing return exists only for the value-that-cannot-occur — a mode reaching
  it is a bug, not a safe default.
- **Per-provider mappings are exhaustive by test.** `read-only` needs both
  Claude halves: the mode alone is defeated by any settings-source allow rule
  (see `docs/references/claude-wire.md` §"Permission modes for read-only
  sessions"). `auto` on Codex deliberately keeps `approval-required`'s policy
  pair — `approvalsReviewer` changes *who answers* an escalation, and relaxing
  the sandbox would take workspace writes out of the reviewer's jurisdiction
  while the tier still promised to review each sensitive tool use.
- **Codex's reviewer is always explicit where it can take effect.**
  `approvalsReviewer` is thread state Codex keeps across a resume, so omitting
  it on the way OUT of `auto` would leave a thread auto-reviewing under a mode
  that promises a human. The handshake `thread/start` / `thread/resume` and
  every `turn/start` name it. The mid-life reconcile resumes
  (`session_probe.go`, `collab_rehydrate.go`) deliberately do not: Codex
  ignores every override on a resume of a LOADED thread, and a divergent one
  is what arms its shutdown-and-cold-resume branch. See
  `docs/references/codex-wire.md` §`approvalsReviewer`.
- **Codex's start/resume response is the only reviewer-support probe.**
  `ThreadStartParams` has no `deny_unknown_fields`, so a codex predating
  `approvalsReviewer` accepts the field, drops it, and starts a `user`-reviewer
  thread. `initialize` carries no version or capability list and
  `thread/started` does not carry the reviewer, so `verifyApprovalsReviewerEcho`
  reads it back off the handshake response and fails the session on a mismatch
  — a user-facing error, never a silent downgrade. Later drift is reconciled
  from `thread/settings/updated` (`thread_settings.go`).
- **Transitions, not just states.** On Codex every runtime-mode axis is a
  `turn/start` override, so all 20 ordered tier pairs are live. On Claude only
  `read-only` needs a restart (its `--disallowedTools` is spawn-only);
  `PlanLiveUpdate` enforces that by comparing `Config.DisallowedTools`, and
  `auto ↔ {approval-required, auto-accept-edits, full-access}` are all one
  `set_permission_mode`. The one exception is layered separately:
  `ApplyLiveUpdate` refuses an escalation to `bypassPermissions` on a process
  spawned without `--allow-dangerously-skip-permissions`, which is a property
  of the process, not of the plan.
- **AO is never in Claude's headless posture.** Auto's fallback-to-ask becomes
  a hard deny (and a denial streak aborts the turn) when
  `toolPermissionContext.shouldAvoidPermissionPrompts` is set. Disassembly of
  2.1.219 shows two producers, both nested-loop constructors — the
  `avoid_prompts` permission layer pushed for a subagent that does not share
  the parent's app state, and the `agentType`/`requestDialog` sub-context
  builder. AO's top-level stream-json session gets neither and installs a
  CanUseTool responder (`--permission-prompt-tool stdio`), so it takes the
  "falling back to prompting" path. Detail lives on
  `claudeAutoPermissionMode` in `claude/options.go`.

`Capabilities.EnforcesRuntimeMode` marks providers whose session config
actually applies the mode. It is false for `claude-tui` (approvals live inside
the real TUI), so callers that treat a runtime mode as a guarantee rather than
a preference must refuse those providers instead of running unenforced. Every
tier — `auto` included — must be inert there by construction, which
`claudetui.TestConfigFromOptionsIgnoresEveryRuntimeMode` pins by iterating
`AllRuntimeModes` rather than naming tiers.

## SessionOptions + ThreadView

`SessionOptions` (this package) and `store.ThreadView` (in
`internal/store/thread_view.go`) are the translation layer between the
raw thread row and the per-provider Config. Callers hydrate a
`ThreadView` from `store.Thread`, derive `SessionOptions` from it, then
hand those options to the provider-specific `Config.From(opts)`.
Provider-specific knobs (context window, reasoning effort, fast mode)
live in the options; the provider packages stay free of SQLite types.

## Interactive Requests

The two providers disagree about the wire format for interactive prompts,
so normalize early and keep the frontend branches explicit:

- Both produce `ApprovalRequest` values with a `Kind` the frontend can
  branch on (`permission`, `file-change`, `file-read`, `command`,
  `mcp-elicitation`).
- Structured answer collection is not an approval. It leaves this package
  as a `UserInputRequest` and resolves through `RespondToUserInput`, matching
  t3-code's separate `user-input.requested` / `user-input.resolved` flow.
- The original provider shape is preserved where we need it to send a
  response back, but the frontend never sees it.
- When the provider introduces a new request type, add a new `Kind`
  or `UserInputRequest` shape here and make sure the frontend has a branch
  for it before shipping. A prompt the frontend doesn't render is a silent
  dead-end.

## Extension points

- To add a new provider: follow
  `docs/architecture/how-to.md#add-a-new-provider`. Sub-package under
  `provider/<name>/`, implement the shared Session interface, register
  in `app.go`.
- To add a new approval Kind or structured user-input shape: add the shared
  type here, add the frontend branch, then wire the adapter — tests for both
  sides in the same PR.

## Anti-patterns

- Do NOT write to `store` from here. No writes to store. No events
  emitted directly to the UI channel. Return structured values; `app.go`
  and `triage` decide where they land.
- Do NOT leak provider-native types (SDK structs, JSON-RPC frames) into
  shared `provider/` types. Keep them inside the subpackage.
- Do NOT guess wire behavior from this repo. Confirm with the references
  below; spike-test if both are silent.

## References

- Claude → upstream `@anthropic-ai/claude-agent-sdk` and
  `forge/apps/server/src/provider/Layers/ClaudeAdapter.ts`.
- Codex → `/home/rmurphy/repos/codex` (wire format) and
  `CodexMonitor` (client patterns). See `docs/references/codex.md`.
- `docs/architecture/providers.md` — shared contract.
- `docs/references/spike-policy.md` — when and how to spike-test.
