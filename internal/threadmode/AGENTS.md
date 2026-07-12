# internal/threadmode/

Pure validators and parsers for the two thread-mode axes:

- **Interaction mode** — `chat`, `plan`, `design` (or engine-owned
  `discussion` / `workflow`, which are set only by their owning sagas). Controls the UX shell and how
  user input is routed.
- **Runtime mode** — `approval-required`, `auto-accept-edits`,
  `full-access`. Controls how the provider session treats tool calls.

The package owns the legal value sets, normalisation (trim), and a clear
error path for unknown values. Everything stateful — persisting, emitting
`thread:mode_changed` / `thread:runtime_mode_changed`, restarting active
sessions — lives in the main package's `app_thread_interaction_mode.go`
and `app_runtime_mode.go`, which compose this package's pieces.

## Surface

| Function | Purpose |
|---|---|
| `ValidateCreate(mode) (string, error)` | Normalises an interaction mode for `CreateThread`. Empty → `DefaultCreateMode` ("chat"). Accepts `chat`/`plan`/`design`. Rejects `discussion` / `workflow` (their owning sagas create them directly) and unknown values. |
| `ValidateSet(mode) (string, error)` | Normalises an interaction mode for `UpdateThreadMode`. Accepts `chat`/`plan` only — `design`, `discussion`, and `workflow` are immutable thread *types* set by their owners. Rejects empty (UpdateThreadMode is always an explicit user action). |
| `IsPostCreationMode(mode) bool` | Reports whether a *current* mode is one the UI is allowed to flip post-creation. Used to gate `UpdateThreadMode` on threads whose type is immutable. |
| `ParseRuntime(mode) (provider.RuntimeMode, error)` | Validates a runtime-mode string. Empty is rejected — use `ParseOptionalRuntime` for the optional case. |
| `ParseOptionalRuntime(mode) (provider.RuntimeMode, bool, error)` | Optional-input variant: empty returns `("", false, nil)` so callers can branch on "no value supplied" without sentinel checks. |

## Constants

- `DefaultCreateMode = "chat"` — what an empty `mode` field in
  `CreateThread` normalises to.
- `ManualSelectionModes` — the set the UI is allowed to set at creation
  time. Excludes engine-owned `discussion` and `workflow`.
- `PostCreationModes` — the set the UI is allowed to mutate into via the
  agent-mode toggle. Only `chat` and `plan`.

## Design notes

- This package depends on `internal/provider` for the `RuntimeMode`
  constants. It does not import anything else.
- The validators are pure: same inputs always produce the same outputs,
  no clocks, no globals, no I/O. That keeps the active-session restart
  orchestration in the main package easy to test against fixed inputs.
- Discussion and workflow threads are intentionally not validated here. The
  `StartDiscussion` saga creates discussion-mode threads directly via
  the store; routing those through `ValidateCreate` would let any UI
  caller produce orphaned discussion shells with no deliberation channel.
  Workflow phase threads similarly require an engine run record and schema.
