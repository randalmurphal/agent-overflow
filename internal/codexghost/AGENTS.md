# internal/codexghost/

Pure summary-rewrite helpers used by Codex runtime retirement, the
rule that on every Codex session end or app restart transitions persisted
background `tool_call` rows from a now-dead subprocess into the
timeline's `errored` / `lost` state.

The App-side bookkeeping (the store call and emit fan-out), startup
`RecoverBackgroundRuntimeOnStartup`, and warm-reconnect reconciler stay in
`internal/codexthread` because it owns the store + transport boundary. This
package only owns the summary rewrite contract those call sites share.

## Surface

| Symbol | Purpose |
|---|---|
| `SessionEndedSuffix` (`" — session ended"`) | Suffix appended to non-empty ghost summaries so the timeline reads e.g. "Editing src/foo.go — session ended". Public so the App tests that compare expected summaries can reference the same source. |
| `GhostSummary(summary) string` | Idempotent rewrite. Empty / whitespace-only input returns `"Session ended"` (the leading em-dash would look cosmetic on its own); non-empty input gets the suffix appended once (HasSuffix guard prevents repeat-pass accumulation). |

## Responsibility boundary

- What BELONGS here: stdlib-only summary-shape contract.
- What does NOT belong here: SQLite writes, transport emissions, or
  any awareness of the `is_background=1 AND status='running' AND
  kind='tool_call'` row predicate (that's the store's job).

## Anti-patterns

- Do NOT change the contract without also updating the integration
  tests in `internal/app`. The App-side saga depends on the
  idempotence and the trim semantics.
