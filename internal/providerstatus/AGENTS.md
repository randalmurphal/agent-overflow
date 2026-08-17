# internal/providerstatus/

Wire shape + pure mapping helpers behind the `provider:status` event
channel. Provider-level health (install / version / auth) is pushed
to the frontend so `ProviderStatusBanner` can render actionable
guidance. This is a separate channel from the per-turn
`provider:item_event` stream — those describe timeline content;
these describe the binary itself.

App-bound glue (`emitProviderStatus`, `emitProviderStatusesFromDetect`,
`probeStartupProviderStatuses`, `emitClaudeUnauthenticatedStatus`,
`emitProviderStatusOnSessionStartError`) stays in
`app_provider_status.go` because those methods read `a.settings`,
call `provider.DetectProvider`, and emit through `a.emit`.

## Surface

| Symbol | Purpose |
|---|---|
| `Event` | Wire shape for `provider:status`. JSON tags pin the field names the frontend reads (`provider`, `status`, `message`, `version`, `actionable`, `actionUrl`). The matching TS interface in `frontend/src/lib/types/events.ts` is hand-written — keep both in sync. |
| `ActionURL(providerName, status) string` | Canonical docs / login URL for each `(provider, status)` pair. Go owns this table so the frontend can't invent URLs. Returns `""` for combinations with no useful link (ready, error, version_too_old). |
| `EventFromDetect(ps provider.ProviderStatus) Event` | Converts the pull-shape `DetectProvider` result into a push-shape `Event`. The `Status=="ready"` branch short-circuits to an all-default event (UI treats ready as "clear the banner"). |
| `ClaudeUnauthenticated(info provider.AccountInfo) bool` | "No identity evidence at all" is the only logged-out signal: any of token source / email / display name, or an `apiProvider` other than `firstParty`, means authenticated. `subscriptionType` is deliberately NOT evidence — it is a storage echo a destroyed login still reports (spike 2026-08-16, 2.1.232). Wired into `providerProbeRunner.unauthenticated` in `app_claude_probe.go`. Codex leaves the slot nil because its empty planType is ambiguous. |

## Design notes

- Imports only `agent-overflow/internal/provider` and stdlib.
- The wire shape lives here rather than in `app_*.go` so the URL
  table, mapping branch, and authentication heuristic can be tested
  without spinning up an `App` — see `providerstatus_test.go`. The
  integration tests in `app_provider_status_test.go` keep covering
  the App-coupled emit flow (`testEmitHook` capture +
  `GetProviderStatuses` round trip + idempotent re-emit).
- `ClaudeUnauthenticated` is the ONE definition of "logged out" for
  Claude — `app_claude_probe.go`, `app_claude_ratelimits.go`,
  `app_provider_accounts.go`, and the `providersmoke` gate all call it
  rather than comparing `AccountInfo` fields themselves. Do not add a
  second copy: the rule is not derivable from the field names, because
  which fields the CLI populates depends on the backend (see the
  function's own comment, and `internal/provider/usage.go`'s note on
  `AccountInfo`). If you change it, also update the Recheck Auth UI in
  `frontend/src/lib/components/banner/`.
- It is Claude-only by contract. Codex hardcodes `apiProvider:"openai"`
  even when it knows nothing about the account, so feeding a Codex
  `AccountInfo` here always answers "authenticated"; `app_codex_probe.go`
  leaves its hook nil instead.
