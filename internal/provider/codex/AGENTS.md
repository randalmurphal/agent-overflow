# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Layout

- `session.go` — process lifecycle, constructor, send/steer/interrupt,
  close, dynamic-tool handler registration, and the shared `Session`
  state struct.
- `jsonrpc.go` — JSON-RPC request/response/notification writes,
  pending response correlation, read loop, and raw line dispatch.
- `account.go` — shared `account/read` decoding plus the cached app-server
  identity used to verify which saved login a live process is serving.
- `identity_probe.go` — fresh-process `account/read` probe used to detect
  native credential changes without a rate-limit request or model turn.
- `launch.go` — shared app-server CLI arguments. Agent Overflow pins Codex to
  its native `auth.json` credential store so account switching is a
  deterministic atomic file replacement even where Codex `auto` mode would
  otherwise choose an OS keyring.
- `session_notifications.go` — notification pre-processing, child
  routing, classifier invocation, event enrichment, turn-state tracking,
  and final event emission.
- `child_routing.go` — bounded/deadlined fail-closed quarantine for
  notifications and server requests from not-yet-owned child threads.
- `collab_rehydrate.go` — bounded read-only descendant-history traversal and
  active-child subscription recovery on root resume.
- `session_rollout_notifications.go` — narrow tailing of the active
  Codex rollout file for detached-child mailbox notifications that
  resumed app-server sessions do not expose as raw events.
- `server_requests.go` — server-initiated request dispatch:
  approvals, MCP elicitation, structured user input, and dynamic tools.
- `turn_input.go` — outbound `turn/start` / `turn/steer` user-input
  payload shaping.
- `interactive_requests.go` — pending approval/user-input tracking,
  dedupe, response claiming, interrupt/close drain, and lost-prompt
  resolution events.
- `plan_buffer.go` — proposed-plan delta buffering and fallback
  completion content.
- `raw_tool_calls.go` — raw `rawResponseItem` function-call tracking,
  `write_stdin`/`wait_agent`/`spawn_agent` enrichment, and event-log
  redaction for stdin payloads.
- `collab_agents.go` — spawned child-thread parent mapping, agent path
  mapping, receiver metadata enrichment, wait-target preservation, and
  child metadata refresh.
- `session_helpers.go` — thread start/resume params, sandbox policy,
  approval/user-input metadata builders, and route-field parsing.
- `session_probe.go` — `thread/read` liveness probe used on reopen to
  reconcile stale running rows. `session_probe_testhelpers.go` hosts
  the fake probe reply harness.
- `session_fork.go` — the `thread/fork` RPC wrapper (`Fork` / `ForkAt`);
  a `lastTurnId` cut (Codex >= 0.143) is the history-truncation
  primitive that replaced the deprecated `thread/rollback`.
- `protocol.go` — top-level notification dispatch plus small shared
  notification helpers.
- `protocol_item.go` — `item/*` lifecycle, tool completion, item type
  normalization, and tool-result content extraction.
- `protocol_turn.go` / `protocol_thread.go` — turn lifecycle,
  thread/account/model notifications, and token usage normalization.
- `usage_accounting.go` — per-turn token accounting derived from the
  cumulative `thread/tokenUsage/updated` totals (Codex has no per-turn
  usage signal and no USD cost on the wire). Tracks
  accounted-vs-latest cumulative per session, handles the resume
  baseline (skips the first post-resume turn when no pre-turn seed
  arrived) and the ContextWindowExceeded sentinel that destroys the
  cumulative; `attachTurnUsage` stamps the delta onto the parent
  turn-complete meta in `updateNotificationState`. Wire `inputTokens`
  includes `cachedInputTokens` — the normalized delta splits them.
- `protocol_json.go` — JSON navigation, compact/pretty JSON, retry-count,
  and flexible wire-shape helpers.
- `protocol_rate_limits.go` — rate-limit event normalizer.
- `approval.go` — sandbox approval-method translation into the shared
  `ApprovalRequest` Kind. MCP elicitation lands here too as
  `Kind: "mcp-elicitation"`; the dispatch lives in `server_requests.go`'s
  server-request handler.
- `subagent_notifications.go` — `<subagent_notification>` XML-tag parser
  for detached-child-agent terminal signals in raw mailbox carriers,
  rollout `response_item` records, and legacy standalone user-message
  carriers. Pure parsing (regex + JSON decode), no Session state.
- `options.go` — `SessionOptions → Config` hydration, binary probe.
- `mcpstatus.go` — ephemeral MCP status fetcher (`MCPStatusFetcher`,
  drives `mcpServerStatus/list` via an inline JSON-RPC client) plus
  the wire-shape projectors (`MCPStatusFromList`,
  `MCPStatusFromNotif`) consumed by `internal/mcpstatus` via the
  shared `Fetcher` interface. Backs both the inactive-thread fallback
  and the `mcpServer/startupStatus/updated` /
  `mcpServer/oauthLogin/completed` notification paths.

## Methods we call

- `thread/start`, `thread/resume`, `thread/fork` (optionally cut at a
  `lastTurnId` anchor — the revert/fork history primitive; upstream
  deprecated `thread/rollback` and AO no longer calls it).
- `thread/read` — on-reopen liveness probe. Called by `Session.Probe`
  to fetch the current `thread.status.type` so the app-layer reconciler
  can flip stale running background tool rows.
- `turn/start`, `turn/steer`, `turn/interrupt` — deliver, steer, and stop
  user turns. `turn/start` carries per-turn config overrides (`model`,
  `effort`, `serviceTier`, `approvalPolicy`, `sandboxPolicy` — upstream
  documents each as applying "for this turn and subsequent turns"),
  which is how a mid-session model / effort / fast-mode / runtime-mode
  change lands without a session restart (`live_update.go`).
  `provider.SendOptions.OutputSchema` is sent as `outputSchema` on every
  schemaed `turn/start`; it is per-turn and never sticky. The final completed
  agentMessage text is JSON-parsed into the turn-complete structured payload.
  `turn/steer` takes NO config fields, so an in-flight turn can never
  be reconfigured.
- Sandbox/approval method family — `file/write`, `file/delete`,
  `file/mkdir`, `command/execute`, etc. These arrive as requests we
  must respond to.

## Notifications we handle

⚠ **Authoritative wire reference**:
[`docs/references/codex-wire.md`](../../../docs/references/codex-wire.md).
Read that before adding or changing handler logic — it has the
canonical JSON-RPC examples, pinned citations into codex-source
and CodexMonitor, and the rules for how `item/*`, `turn/*`, and the
collab-agent lifecycle interact.

Summary:

- `turn/started`, `turn/completed` — **turn lifecycle**. Authoritative
  turn-complete signal; emit
  `EventTurnComplete` which triage forwards as
  `provider:turn_completed` to the frontend.
- `item/started`, `item/completed` — **tool lifecycle**. Every tool
  produces both (one-shot upsert pattern). Items carry their own
  `status` field (`inProgress | completed | failed | ...`) directly
  on the wire.
- `thread/status/changed` — session-level state.
- Rate-limit, model-reroute, reasoning-delta, thread-rename,
  thread-compact events.
- `error` notifications (user-facing state, not log entries).

## Lifecycles we drive

- **Tool lifecycle** — every `item/*` fires `EventToolStart` /
  `EventToolComplete` for the item's own id. Status comes from the
  wire `item.status`. See
  [`turn-lifecycle.md §Tool lifecycle`](../../../docs/architecture/turn-lifecycle.md#1-tool-lifecycle).
- **Turn lifecycle** — `turn/completed` carries `{threadId, turn}`.
  `turn.status` is the lifecycle authority; current Codex does not
  include a final assistant message id on this envelope.
- **Background terminals.** Codex has no `run_in_background` flag, but
  `exec_command` can yield back to the model while its PTY keeps
  running. Typed `item/started` / `item/completed` commandExecution events are
  the UI history source; raw `exec_command` tool output is model-facing text
  that may enrich live process metadata but must not create, delay, or reorder
  chat history. `TerminalInteractionNotification` is the source for waited and
  interacted marker rows (per
  [invariant 25](../../../docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics)).
  This package surfaces item lifecycle fields via `enrichItemMeta` and emits
  an internal `EventCodexExecResult` from raw `exec_command` output only for
  live-state enrichment.
  Heuristic event-ordering classifiers are still forbidden as the
  authorization — the yield moment is just the observable trigger for
  an already wire-authorized commitment. Per-row stop for backgrounded
  commands is blocked on an upstream protocol change; see
  [`codex.md §Known upstream constraints`](../../../docs/references/codex.md#known-upstream-constraints).

## Collab agent lifecycle (MultiAgentV1 and MultiAgentV2)

Codex's closest analog to Claude's backgrounded tools, but
structurally different — **a spawn creates a child thread**, not
a backgrounded tool. See
[`codex-wire.md §Collab agent lifecycle`](../../../docs/references/codex-wire.md#collab-agent-lifecycle-multiagentv1-and-multiagentv2)
for the wire sequence. Key points:

- V1 ownership arrives on `collabAgentToolCall spawnAgent`; V2 ownership
  arrives on completed `subAgentActivity kind:"started"`.
- V2 may start/stream the child before the ownership item. Every unknown
  non-root thread is quarantined fail-closed, including recursively spawned
  grandchildren; child lifecycle never becomes root lifecycle.

- `spawn_agent` tool_call completes **immediately** (status:
  completed) when the spawn request is accepted; child runs on a
  separate `thread_id`.
- Parent's `turn/completed` does NOT wait for spawned children.
- Child completion changes launch live state, but transcript presentation waits
  until Codex delivers the result into parent model context. MultiAgentV2 records
  that boundary as a parent-rollout `inter_agent_communication` containing a strict
  `Message Type: FINAL_ANSWER` envelope. Agent Overflow emits the flat completion
  row only from that record. Legacy flows use either explicit `wait` or a
  mailbox-delivered raw message carrying a `<subagent_notification>` XML tag.
  Codex exposes the legacy marker
  as contextual `role:"user"` input. Fresh `thread/start` sessions can
  expose it through `rawResponseItem/completed` when raw events are
  enabled; resumed sessions cannot opt into that raw stream, so
  Agent Overflow also tails the active rollout JSONL from EOF and parses
  appended `inter_agent_communication` records. Traces
  and older flows can expose the same marker inside a serialized
  `InterAgentCommunication` assistant/commentary message or a standalone
  user-message carrier.
- Wire enum for the wait tool is `"wait"` (not `"waitAgent"`).

## Responsibility boundary

- What BELONGS here:
  - JSON-RPC frame marshal / unmarshal and v2 ThreadItem shape handling.
  - Sandbox-method → `ApprovalRequest` translation.
  - Session request/response/notification correlation.
  - `options.go` binary probe and `Config.From(SessionOptions)`.
- What does NOT belong here:
  - Raw JSON-RPC shape inspection outside this package. Other callers
    should only see the normalized `provider.Event` types.
  - SQLite writes, `app.Event.Emit`, or cross-thread orchestration.

## Interactive Requests

Sandbox method requests (`file/write`, `command/execute`, ...) are
translated into the shared `ApprovalRequest` shape before leaving this
package. MCP elicitation comes in as its own method and currently maps
to `Kind: "mcp-elicitation"` — confirm the frontend has a branch for
this before relying on it.

`item/tool/requestUserInput` is a separate structured user-input flow, not
an approval kind. Validate that at least one question is present before
emitting, track it with the user-input resolve kind, and answer through
`RespondToUserInput`.

## Extension points

- To add a new provider notification: add it to the matching
  `protocol_*.go` classifier, update `session_notifications.go` only if
  it needs session state/routing, then unit test the normalized
  `provider.Event` output.
- To add a new server request: dispatch it in `server_requests.go`, add
  or reuse the metadata builder, and test the JSON-RPC response path.
- To add a new sandbox approval method: extend `approval.go`'s kind
  table and the shared `provider.ApprovalRequest`. Spike against
  `codex app-server` if the method shape isn't obvious from
  `codex-source`.

## Anti-patterns

- Do NOT inspect raw JSON-RPC shape from outside this adapter. Callers
  see normalized `provider.Event` values only.
- Do NOT invent `ThreadItem.status` values. Respect the v2 status enum
  from upstream — if a new status is needed, update `protocol.go` and
  the reconciler together.
- Do NOT fall back to a generic "unknown notification" path. Each
  notification type has an explicit dispatch or an explicit "ignored"
  log line.

## References

- **Codex source** — https://github.com/openai/codex
  Local: `/home/rmurphy/repos/codex`. Canonical wire format. If
  our parser disagrees with the source, the source wins.
- **CodexMonitor** — https://github.com/Dimillian/CodexMonitor
  Tauri, feature-complete client. Reference for client-side patterns:
  process lifecycle, reconnect, interruption, rollback, fork.
- Codex App Server docs: https://developers.openai.com/codex/sdk/#app-server
- `docs/references/codex.md` — workflow for reading these sources.
- `docs/references/spike-policy.md` — when both are silent.
