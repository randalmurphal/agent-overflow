# Triage Routing

Every normalized `ProviderEvent` flows through `Router.Handle` in
`internal/triage/router.go`. This page is the routing table — one line per
`EventKind`, source of truth is `Router.Handle` plus
`internal/provider/types.go` `AllEventKinds`.

## Routing Table

| EventKind | Disposition |
|---|---|
| `init` | `handleInit` — emit inline + upsert `session_ref` on `threads`. |
| `text_delta` | `handleTextDelta` — accumulate per-thread text, emit inline. |
| `tool_start` | `handleToolStart` — persist tool-use shape, capture pending inline diff, emit. |
| `tool_complete` | `handleToolComplete` — persist tool result (inc. inline diffs), emit. |
| `turn_start` | `handleTurnStart` — emit inline, open turn span, capture git baseline. |
| `turn_complete` | `handleTurnComplete` — drain accumulators, persist text/reasoning/plan, close span. |
| `approval_request` | Inline emit. |
| `approval_resolved` | Inline emit. |
| `session_status` | Inline emit. |
| `token_usage` | `handleTokenUsage` — compute cost from model, emit enriched meta. |
| `error` | Inline emit. |
| `background_start` | Inline emit (tray marker; SQLite write happens on complete). |
| `background_delta` | Dropped. Accumulated in the provider package. |
| `background_complete` | `persistHeavy` → `full_text` payload + `background_done` item. |
| `tool_progress` | Inline emit. |
| `compact_boundary` | Inline emit. |
| `rate_limits` | Inline emit. |
| `model_rerouted` | `handleThreadModelUpdate` — persist new model, emit. |
| `thread_renamed` | `handleThreadRename` — persist new title, emit. |
| `diff` | `handleDiff` — `persistHeavy` + upgrade earlier summary-only tool results. |
| `command_output` | `persistHeavy` → `command_output` payload + `command_execution` item. |
| `thinking` | `handleThinking` — persist if item-scoped, else accumulate for turn-complete drain. |
| `proposed_plan` | `persistHeavy` → `proposed_plan` payload + item. |
| `plan_update` | Inline emit (Codex `turn/plan/updated` mid-turn stream). |

Inline emits go out on the `"provider:event"` Wails channel. Heavy
persistence also emits a `"provider:meta"` event carrying the rendered
preview; the full body loads on demand.

## The Sentinel

The `default` branch in `Handle` calls `r.emit("provider:event", evt)`
(best-effort passthrough) and returns `fmt.Errorf("%w: %s",
ErrUnhandledEventKind, evt.Kind)`. The sentinel exists so
`TestHandleEveryEventKindCovered` can loop `provider.AllEventKinds` and
fail loudly if any kind falls through — see `router_test.go:22`.
`TestAllEventKindsListIsComplete` (`router_test.go:56`) guards the
complementary drift: a new const in `types.go` that isn't added to
`AllEventKinds`.

## Adding a New EventKind

When a provider surfaces a new event type:

1. Add the constant to the `const` block in `internal/provider/types.go`.
2. Append it to `provider.AllEventKinds` in the same file.
3. Add a `case` in `Router.Handle` (`internal/triage/router.go:161`).
4. Add the matching case in the frontend switch in
   `frontend/src/lib/stores/events.ts` — the `never` guard at the
   default branch will fail the TypeScript build if you forget.
5. Update the reference map in `TestAllEventKindsListIsComplete` so the
   new kind is expected there too.

Skipping any step produces either a silent drop (no case in router) or
a CI failure (missing from `AllEventKinds` or the frontend `never`
guard). Both are intentional — a kind the UI doesn't render is a dead
end users can't see.
