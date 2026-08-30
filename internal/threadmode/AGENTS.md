# internal/threadmode/

Pure validators and parsers for the two thread-mode axes:

- **Interaction mode**: `chat`, `plan`, `design` (or saga-owned
  `discussion` / `workflow` / `workflow-studio` / `workflow-triage`, which are
  set only by their owning sagas). Controls the UX shell and how
  user input is routed.
- **Runtime mode**: `read-only`, `approval-required`,
  `auto-accept-edits`, `auto`, `full-access`. Controls how the provider
  session treats tool calls. The legal set is `provider.AllRuntimeModes`,
  not a local list. `ParseRuntime` derives membership from it so a tier
  added in `internal/provider` cannot be rejected here (`auto`, the
  AI-reviewed tier, became legal here with no edit to this package).

The package owns the legal value sets, normalisation (trim), and a clear
error path for unknown values. Everything stateful (persisting, emitting
`thread:mode_changed` / `thread:runtime_mode_changed`, restarting active
sessions) lives in the main package's `app_thread_interaction_mode.go`
and `app_runtime_mode.go`, which compose this package's pieces.

## Surface

| Function | Purpose |
|---|---|
| `ValidateCreate(mode) (string, error)` | Normalises an interaction mode for `CreateThread`. Empty → `DefaultCreateMode` ("chat"). Accepts `chat`/`plan`/`design`. Rejects every saga-owned mode (`discussion` and all workflow modes) and unknown values. |
| `ValidateSet(mode) (string, error)` | Normalises an interaction mode for `UpdateThreadMode`. Accepts `chat`/`plan` only. Immutable thread types (`design`, `discussion`, and all workflow modes) stay owned by their sagas. Rejects empty (UpdateThreadMode is always an explicit user action). |
| `IsPostCreationMode(mode) bool` | Reports whether a *current* mode is one the UI is allowed to flip post-creation. Used to gate `UpdateThreadMode` on threads whose type is immutable. |
| `IsLegal(mode) bool` | Reports whether persistence accepts the mode. |
| `IsSagaOwned(mode) bool` | Reports whether creation belongs to a coordinating saga. |
| `IsHidden(mode) bool` | Single definition for exclusion from normal listings, global search, and pickers. |
| `HiddenModes() []string` | Stable copy of the hidden set for SQL query construction. |
| `ParseRuntime(mode) (provider.RuntimeMode, error)` | Validates a runtime-mode string. Empty is rejected. Use `ParseOptionalRuntime` for the optional case. |
| `ParseOptionalRuntime(mode) (provider.RuntimeMode, bool, error)` | Optional-input variant: empty returns `("", false, nil)` so callers can branch on "no value supplied" without sentinel checks. |

## Constants

- `DefaultCreateMode = "chat"` is what an empty `mode` field in
  `CreateThread` normalises to.
- `ManualSelectionModes` is the set the UI is allowed to set at creation
  time. Excludes all saga-owned modes.
- `PostCreationModes` is the set the UI is allowed to mutate into via the
  agent-mode toggle. Only `chat` and `plan`.

## Design notes

- This package depends on `internal/provider` for the `RuntimeMode`
  constants. It does not import anything else.
- The validators are pure: same inputs always produce the same outputs,
  no clocks, no globals, no I/O. That keeps the active-session restart
  orchestration in the main package easy to test against fixed inputs.
- Saga-owned threads are intentionally not accepted by the public validators. The
  `StartDiscussion` saga creates discussion-mode threads directly via
  the store; routing those through `ValidateCreate` would let any UI
  caller produce orphaned discussion shells with no deliberation channel.
  Workflow phase threads similarly require an engine run record and schema;
  `workflow-triage` threads require the run's hand-off entry point. Nothing
  creates `workflow-studio` threads any more (D32). The mode stays legal
  because shipped databases hold rows in it and the hidden-mode exclusion has
  to keep hiding them.
