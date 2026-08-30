# internal/provider/

Owns every wire-level interaction with a coding-agent subprocess. Triage and
store consume the normalized output; nothing downstream knows which provider
produced an event. Three sub-packages, each with its own guide: `claude/`
(Claude Code CLI, NDJSON over stdio), `claudetui/` (the real Claude TUI in a
PTY), `codex/` (Codex `app-server`, JSON-RPC 2.0 over stdio).

Belongs here: process lifecycle, wire parsing into provider-agnostic events,
option hydration (`Config.From(SessionOptions)`). Belongs elsewhere: SQLite
writes, `app.Event.Emit`, cross-thread orchestration and reconnect policy.

## Account probes

`claude.ProbeAccount`, `codex.ProbeAccount` and `codex.ProbeIdentity` enforce
two rules in code rather than by caller discipline.
- **`ProbeConfig.WorkDir` is required and absolute** (`ValidateProbeWorkDir`,
  `probeworkdir.go`). Both CLIs walk up from their cwd for project-scoped
  config, and a project `.claude/settings.json` env block can repoint the CLI
  at another backend, so an inherited cwd makes "who is logged in" depend on
  where AO was launched from.
- **`ProbeCacheKey` carries `(Binary, AccountID, WorkDir, EnvFingerprint)`**
  (`probecache.go`). Dropping a dimension serves one environment's identity as
  another's; the fingerprint is a digest, since a key outlives its call.

Every probe runs in the one directory `providerProbeWorkDir` pins (the user
home) because probe consumers are global; making probes follow the active
thread's workspace is a product change, not a bug fix.

## Child environment

`BuildEnvironment` / `FilterEnvironment` (`env.go`) assemble every provider
child environment and both apply `appimage.Scrub`, as does
`runVersionCommand` (`detect.go`). The additive `PATH` merge reads the
inherited half off the *scrubbed* base and never `os.Getenv`, which keeps an
AppImage mount's bin directory from re-entering through the override path.
`ReservedEnvNames` (`pinnedenv.go`) is the source of truth for the variables
AO sets or clears itself, and therefore the ones a user's custom environment
may not override. `internal/settings` keeps a copy because it cannot import
this package; `TestReservedEnvNamesMatchTheProviderPins` fails on drift either
way. A new pin in any spawn path joins that list in the same commit.

## Model catalogs

`ClaudeModels` / `CodexModels` (`models.go`) are the shipped catalogs, and the
`ModelCatalogSource` constants in `capabilities.go` document what each
provider does with one (Codex REPLACES from `model/list`, so a miss there is
authoritative; Claude MERGES the probe's list and never subtracts, with the
policy in `internal/claudemodels`). Two rules bind every caller here.

- **A model id is never its context tier.** `NormalizeModelSlug` trims Claude's
  trailing `[1m]` marker before a lookup; a `FindModel` miss quietly downgrades
  effort tiers, fast mode and context windows to provider defaults.
- **"No reasoning tiers" is an argv answer, not a stored one.** Argv builders
  ask `ModelDeclaresNoReasoningEffort` so a model like Haiku gets no effort
  flag. The `Coerce*` family cannot answer it: the `reasoning_effort` columns
  are NOT NULL under a CHECK, so what persists is always a legal tier.

## RuntimeMode

Five tiers on one axis (`runtime_modes.go`), most to least restrictive on
mutation. `AllRuntimeModes` is the canonical list, which both
`NormalizeRuntimeMode` and `threadmode.ParseRuntime` derive membership from,
so a new tier cannot be legal in one place and coerced away in another.

| Tier | Claude (CLI 2.1.219) | Codex (app-server, floor 0.143) |
|---|---|---|
| `read-only` | `--permission-mode dontAsk` + `--disallowedTools Write,Edit,NotebookEdit` | approval `never`, sandbox `read-only`, reviewer `user` |
| `approval-required` | `--permission-mode default` (no flag) | approval `untrusted`†, sandbox `read-only`, reviewer `user` |
| `auto-accept-edits` | `--permission-mode acceptEdits` | approval `on-request`, sandbox `workspace-write`, reviewer `user` |
| `auto` | `--permission-mode auto` | approval `untrusted`†, sandbox `read-only`, reviewer `auto_review` |
| `full-access` | `--permission-mode bypassPermissions` + `--allow-dangerously-skip-permissions` | approval `never`, sandbox `danger-full-access`, reviewer `user` |

† A codex >= 0.149.0 gets `on-request` for both instead: 0.149 deleted the
known-safe command allowlist that made `untrusted` mean "reads run, everything
else escalates" (upstream `942af8447b`). The remap keys off the `initialize`
userAgent and fails safe (`codex/options.go#approvalPolicyForCodexVersion`).

`read-only` and `auto` refuse rather than ask, and neither is a fallback.
`read-only` denies by rule, so an unattended workflow phase is refused rather
than left waiting; `auto` denies by judgement, is billed per decision, and
stays interactive because both providers can fall back to a real request.

- **Never make either a fallback.** `DefaultRuntimeMode` is `full-access`, and
  an unknown value must not collapse into a restricted session, widen a
  restricted one, or opt a thread into a billed reviewer.
- **Per-provider mappings are exhaustive by test.** `read-only` needs both
  Claude halves, since the mode alone is defeated by any settings-source allow
  rule (`docs/references/claude-wire.md` §"Permission modes for read-only
  sessions"). `auto` on Codex keeps `approval-required`'s policy pair: the
  reviewer changes *who answers*, and relaxing the sandbox would put workspace
  writes outside that reviewer's jurisdiction.
- **Codex's reviewer is explicit wherever it can take effect.** The handshake
  `thread/start` / `thread/resume` and every `turn/start` name it; the mid-life
  reconcile resumes (`session_probe.go`, `collab_rehydrate.go`) deliberately
  name nothing, for the reasons in `codex-wire.md` §`approvalsReviewer`.
- **Transitions, not just states.** On Codex every axis is a `turn/start`
  override, so all 20 ordered tier pairs are live. On Claude only `read-only`
  needs a restart (`--disallowedTools` is spawn-only), which `PlanLiveUpdate`
  enforces; `ApplyLiveUpdate` separately refuses an escalation to
  `bypassPermissions` on a process spawned without the skip-permissions flag.

`Capabilities.EnforcesRuntimeMode` is false for `claude-tui`, where approvals
live inside the TUI, so a caller treating a runtime mode as a guarantee must
refuse that provider rather than run it unenforced
(`claudetui.TestConfigFromOptionsIgnoresEveryRuntimeMode`).

## SessionOptions and ThreadView

`SessionOptions` here and `store.ThreadView` (`internal/store/thread_view.go`)
translate a raw thread row into a per-provider Config: hydrate a `ThreadView`,
derive `SessionOptions`, hand those to `Config.From(opts)`. Provider-specific
knobs live in the options, so provider packages stay free of SQLite types.
`SessionOptions.DisabledTools` is the exception: it comes from Settings rather
than the row, and each provider reads it in its own vocabulary (raw Claude
names UNIONED with the read-only strip in `claude/options.go`, the same names
taken ALONE on `claude-tui`, curated toggle ids expanded into `config` keys by
`codex/disabled_tools.go`). Neither list may displace the other. It is
spawn-only everywhere, which is why the reconcile path pins it rather than
diffing it; `docs/specs/prompt-tool-overrides.md` owns that stamp-and-pin
contract and the `SystemPrompt` axis that resolves instead.

## Interactive requests

- Both providers produce `ApprovalRequest` values with a `Kind` the frontend
  branches on (`permission`, `file-change`, `file-read`, `command`,
  `mcp-elicitation`). Structured answer collection is not an approval: it
  leaves as a `UserInputRequest` and resolves through `RespondToUserInput`.
- `ApprovalRegistry` (`approval_registry.go`) is the one ledger both providers
  hold for what is outstanding and who may answer each request ONCE. It never
  emits, never writes to a provider and never knows a thread id, which keeps
  its mutex a leaf: `Drain` hands the released requests back so emission
  happens after the lock is gone. A release's wire meaning stays
  provider-side.
- A new request type needs a new `Kind` or `UserInputRequest` shape here AND a
  frontend branch in the same PR: a prompt nothing renders is a dead end.

## Anti-patterns

- Do NOT leak provider-native types (SDK structs, JSON-RPC frames) into shared
  `provider/` types. Keep them inside the subpackage.
- Do NOT guess wire behavior from this repo. Confirm it in
  `docs/references/claude-wire.md` or `docs/references/codex-wire.md`, and
  spike-test when both are silent (`docs/references/spike-policy.md`).

## References

`docs/architecture/providers.md` (shared contract),
`docs/architecture/how-to.md#add-a-new-provider-adapter` (adding one),
`docs/references/claude.md`, `docs/references/codex.md` (reading upstream).
