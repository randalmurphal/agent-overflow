# How-To: Extension Playbooks

Recipes for common changes. Each playbook is a numbered checklist a
contributor follows end-to-end, pointing to the live example in the
codebase so you can copy the shape rather than guessing.

If you need the underlying rules instead of recipes, see
[`invariants.md`](invariants.md) and [`conventions.md`](conventions.md).

---

## Add a New Event Kind

Use when the provider starts emitting a new wire-level event type and we
need to route it through triage to the frontend (or to SQLite + meta).

Live example: the `proposed_plan` / `rate_limits` kinds added during the
chat rewrite.

1. **Provider adapter.** In `internal/provider/{claude,codex}/parser.go`
   (or a sibling `parse_*.go`), parse the wire message into a typed value
   and emit a `provider.ProviderEvent` with the new `Kind`. No store
   access, no `app.Event.Emit`. The adapter only produces events.
2. **`EventKind` constant.** Add the new kind to the `const` block in
   `internal/provider/types.go`, then append it to
   `provider.AllEventKinds` in the same file. The roundtrip test
   `TestAllEventKindsListIsComplete` in
   `internal/triage/router_test.go` will fail until you do.
3. **Triage handler.** Add a `case` in `Router.Handle`
   (`internal/triage/router.go`). If the handler is substantial, add
   it as `handleFooBar` in the appropriate sibling file
   (tool_lifecycle.go, stream_items.go, payload_items.go, etc.). Route
   through `persistItem` for anything that should land as a timeline
   row; route through `emitItemUpsert` / `emitApproval` /
   `emitUsage` / `emitStatus` for typed channel emissions.
4. **Frontend listener (if user-facing).** Subscribe to the
   appropriate typed channel in
   `frontend/src/lib/stores/events.ts` via `wailsEventOn`. The
   frontend branches on routing channel (`provider:item_event`,
   `provider:approval`, `provider:usage`, `provider:status`) rather
   than on individual EventKinds. If your new kind's payload fits
   an existing channel, no new subscription is needed. A new
   channel is a bigger decision; see "Add a New Routing Channel"
   below.
5. **Test.** Minimum bar: a router test that drives a representative
   event through `Router.Handle` and asserts the correct store write +
   emission. If the frontend path is new, add a Vitest covering the
   new case in the events switch.
6. **Update docs.** Add the new row to the routing table in
   [`triage-routing.md`](triage-routing.md).

Skipping any step produces either a silent drop (no case in router)
or a CI failure (missing from `AllEventKinds` or the frontend `never`
guard).

---

## Add a New Item Kind

Use when a new class of timeline entity needs to exist: not a new
tool variant, not a new payload shape, but a fundamentally new
top-level kind distinct from the 7 we have (user_text, assistant_text,
thinking, tool_call, tool_completion, error, compaction).

Live example: the chat-rewrite introduction of `tool_completion` as a
distinct kind from `tool_call`.

This is a big change. Read [`invariants.md`](invariants.md) §1–§5
first.

1. **Schema migration.** Add a migration to `internal/store/migrate.go`
   that extends the `CHECK(kind IN (...))` constraint. CHECK constraints
   can't be ALTERed in SQLite; you either rebuild the `items` table
   (like v15's wipe) or add a new migration that creates a
   replacement `items` table and copies forward. Prefer the copy-forward
   path. Wipes are reserved for shape breaks.
2. **Upsert path.** Update `store.UpsertItem` in
   `internal/store/items.go` if the new kind needs special handling
   (e.g., a new required column, a deterministic id format). Most
   kinds just work through the generic upsert.
3. **ID format.** Add the deterministic id format to the table in
   [`chat-rewrite.md`](chat-rewrite.md) under "Item ID schemas" and
   make sure the triage code that mints ids for this kind follows it.
   Invariant #2 says ids are stable start-to-completion.
4. **Triage routing.** Whichever event kind produces the new item kind
   must route through `persistItem` with the right `kind` value.
5. **Frontend kind dispatch.** `MessageTimeline.svelte` in
   `frontend/src/lib/components/chat/` dispatches per kind. Add a
   branch (or a new sub-component) for the new kind.
6. **Test.** Migration test (up) + lifecycle tests (upsert, render) +
   routing test.

---

## Add a New Tool Renderer

Use when a provider adds a new tool name that deserves a distinct
header / icon / preview than the generic fallback.

Live example: the Bash / Edit / Read / Write / Grep / Task renderers in
`ToolCallCard.svelte`.

1. **Classification.** Add a case to `classifyToolName` in
   `frontend/src/lib/components/chat/toolCardHeader.ts`. This returns
   a `ToolKindVisual` with the icon identifier and any header
   metadata flags. Pure function, no side effects.
2. **Icon.** If the tool needs a new icon, add it to `ToolKindIcon.svelte`
   in the same directory. Prefer reusing one of the existing
   `ToolKindIcon` cases if the tool's semantics match an existing
   family (e.g., a new file-editing tool uses the edit icon).
3. **Header component (only if substantially different).** If the tool
   needs a dedicated header beyond what `ToolCallCard.svelte`'s
   generic path renders, extract a sibling component and dispatch from
   `ToolCallCard.svelte`. Keep it small. See the file-size targets in
   [`conventions.md`](conventions.md).
4. **Test.** Add a case to `ToolCallCard.test.ts` that renders the new
   tool and asserts the header / icon appear.

The adapter side usually needs no change. The new tool name flows
through `tool_name` on the item as-is.

---

## Add a New Schema Migration

Use for any change to the SQLite schema: new column, new index, new
constraint, new table.

Live example: migration v13 (`turns` inflight partial index), v22
(`payloads` highlight-span columns).

1. **Append to `migrations`.** In `internal/store/migrate.go`, add a
   new entry to the `migrations = []Migration{...}` slice. The
   `Version` is the next contiguous integer; `Name` is short
   snake_case; `SQL` is the migration body. Never edit an existing
   entry. Migrations are forward-only and append-only (invariant
   #20).
2. **Write the migration body.** Prefer narrow changes:
   - New column: `ALTER TABLE foo ADD COLUMN bar TEXT NOT NULL DEFAULT '';`
   - New index: `CREATE INDEX idx_foo_bar ON foo(bar);` (use
     `WHERE bar <> ''` for sparse columns, like the partial indexes
     in v15).
   - Enum change: rebuild the table (`DROP` + `CREATE` + copy) rather
     than try to edit a CHECK constraint in place.
3. **Update the migration count test.** `migrate_test.go` has a count
   assertion that catches "forgot to add the new migration to the
   test file." Bump the expected count.
4. **Add the migration's own test.** Minimum bar: apply migrations up
   to N-1, insert a fixture row, apply migration N, assert the
   schema state you intended (column present, index present, CHECK
   fires). See `TestMigrateV15ChatRewriteUnifiedItems` as a
   reference.
5. **Update `schema.md`.** Add the new column/index/constraint to the
   human-readable summary in [`schema.md`](schema.md). If this is a
   load-bearing rename or semantics shift, also update the affected
   `AGENTS.md` under `internal/store/`.
6. **Down migration? No.** We don't support downgrades. The archive
   docs explain the rationale: forward-only is simpler, and rollback
   is a restore-from-backup operation for the user.

---

## Add a New Provider Adapter

Use when adding a third provider (e.g., Gemini, OpenAI Responses).
This is substantial. Expect hundreds of lines of parser and a session
lifecycle implementation.

Templates: `internal/provider/claude/` and `internal/provider/codex/`.
Claude is NDJSON-over-stdio; Codex is JSON-RPC 2.0 over stdio. Pick
whichever is closer to the new provider's wire format.

1. **Directory.** Create `internal/provider/<name>/` with an
   `AGENTS.md` describing the provider's wire format, session model,
   and approval shape. Add a `CLAUDE.md` symlink.
2. **Parser.** `parser.go` owns `Parser` + `ParseLine` dispatch.
   Split wire envelopes per file (`parse_system.go`, `parse_assistant.go`,
   etc.) the way Claude does. Every message either handled or
   explicitly logged as "unknown type, ignored." Never silently drop.
3. **Session lifecycle.** `NewSession` spawns and `Close` tears down
   (`session.go`); the stdout pump is `readLoop` (`session_readloop.go`).
   Input methods (`Send`, `Interrupt`, `RespondToApproval`) are methods
   on `Session` (`session_send.go`, `session_approvals.go`).
4. **Probe.** `probe.go` checks the binary exists, is on a supported
   version, and the user is authenticated. Returns a
   `ProviderStatusEvent` kind the frontend's status banner knows how
   to render.
5. **Options.** `options.go` maps `provider.SessionOptions` to the
   provider's native config shape. This is the adapter boundary that
   keeps provider-specific knobs out of the store.
6. **Approvals.** Translate every request-response approval method
   into the shared `provider.ApprovalRequest` shape with the right
   `Kind`. Preserve the native shape where we need it for the
   response.
7. **Register in `app.go`.** `App.startSessionNow` picks a provider
   package based on `thread.Provider`. Add the new `case`.
8. **Test.** Protocol tests (parser roundtrip), session tests
   (lifecycle), approval tests. Claude's suites are a good bar.
9. **Reference docs.** Add `docs/references/<name>.md` that points at
   the upstream spec + any reference client implementations.

---

## Add a New Approval Kind

Use when a provider starts emitting a new kind of approval the
frontend doesn't yet know how to render.

Live examples: `user-input`, `permission`, `file-change`, `file-read`,
`command`, `mcp-elicitation`.

1. **Adapter normalization.** In the provider adapter, populate
   `ApprovalRequest.Kind` with the new value. The adapter owns the
   translation from provider-specific shape to the shared
   `ApprovalRequest` (see `internal/provider/claude/approvals.go` /
   `internal/provider/codex/approval.go`).
2. **Per-kind panel component.** Add a component under
   `frontend/src/lib/components/` that renders the new kind's UI:
   question form, permission grant toggle, diff preview, etc. Compose
   the shared `primitives/` shells (Menu, Modal, Popover) rather than
   rolling positioning / focus-trap yourself.
3. **Dispatch.** Add a branch in the approval-rendering component
   (whichever component currently dispatches on
   `ApprovalRequest.kind`). Use a `never`-style default branch.
   An unhandled kind is a silent dead-end; TypeScript's
   exhaustiveness check is the only compile-time guard we have
   against shipping with a missing branch.
4. **Decision chip.** If the approval is tool-flavored (affects a
   specific tool_call row), add coverage to `ToolDecisionChip.svelte`
   so the post-resolution chip renders correctly ("approved",
   "declined", "amended", "timeout").
5. **Response encoding.** Update the provider adapter's approval
   response encoder (e.g., `claude.EncodeApprovalResponse`) to handle
   the new kind's response shape.
6. **Test.** Component test for the new panel + approval roundtrip
   test (request → user decision → response → provider sees it).

---

## Split a File When It Crosses Its Size Ceiling

Use when a Go source file exceeds ~800 lines or a Svelte component
exceeds ~500.

Live example: `internal/triage/` was one giant `router.go` before the
split into `tool_lifecycle.go`, `turn_lifecycle.go`, `stream_items.go`,
`stream_state.go`, `block_events.go`, `approvals.go`, `payload_items.go`,
`meta.go`, `maps.go`.

1. **Identify the boundary.** Grep for the logical clusters inside
   the file. In Go: functions that share state (a map, a counter, a
   helper). In Svelte: sub-components that could own their own props.
   A split that leaves ten unrelated functions in each file is the
   wrong boundary.
2. **Sibling file naming.** Pick a name that describes the sub-concern,
   not the type. `tool_lifecycle.go`, not `toolcall_funcs.go`.
   Svelte: `PanelFooBar.svelte` where `Foo` scopes and `Bar` is the
   concrete concern.
3. **File header comment.** Every split file opens with a one-sentence
   purpose:
   ```go
   / Package triage — tool_lifecycle.go.
   / Owns tool_call launch/completion rows, background-task pairing,
   / summary/status derivation.
   ```
4. **Update the Layout section in the package's `AGENTS.md`.** The
   layout map is how a reader finds the right file; a split file that
   isn't listed there defeats the split.
5. **Keep the old file as the entry point.** If the parent file was
   `router.go`, `router.go` keeps `Router`, `Handle`, and the
   dispatch switch, and the siblings own narrow concerns. Don't create
   a file called `router_misc.go`. Every split file earns its name.
6. **Test.** Tests usually don't need to move; they keep testing the
   package surface. If a test was scoped to one sub-concern, move it
   to a sibling test file so it's easier to find.

---

## Add a New Routing Channel

Use sparingly. Prefer the existing typed routing surface
(`provider:item_event`, approval, usage/status, turn lifecycle,
subagent/background notifications, and app-shell channels) unless the
new payload has a distinct lifecycle that cannot fit cleanly there.

Only add a new channel when:

- The new payload shape can't fold into an existing channel without
  producing an "action" discriminator that every subscriber has to
  branch on, AND
- The new payload has its own subscribe/unsubscribe lifecycle on the
  frontend (e.g., "only the diff panel cares").

1. **Define the event payload struct.** In `internal/provider/types.go`
   or a triage-package types file, add a new Go struct with JSON tags.
2. **Name the channel once.** Add the constant to
   `internal/eventchan/channels.go`. Nothing else may spell the string.
3. **Register the policy row.** Add it to the table in
   `internal/transport/event_channels.go` — audience, retention, scope,
   whether it is `EntityFiltered`, and a `Why` a reviewer can check. The
   package's tests fail the build without a row.
   Read [`internal/transport/AGENTS.md`](../../internal/transport/AGENTS.md)
   before picking the columns; `EntityFiltered` in particular means every
   consumer must be pane-scoped or watched-thread-scoped, and that audit
   is the work.
4. **Add it to the harness roll call.** `cmd/ao-harness/channels.go`
   names the constants `ao-harness events` can tail;
   `TestKnownChannelsCoversTheEventChannelRegistry` fails without it.
5. **Router method.** Add `Router.emitFooBar` in `router.go` that calls
   the router's `r.emit` seam with the new constant.
6. **Frontend listener.** Add `wailsEventOn('provider:foo_bar', ...)`
   in `frontend/src/lib/stores/events.ts`. Route to the appropriate
   per-pane handler, and cancel it in the same teardown.
7. **Test.** Router test for the emit path + frontend test for the
   listener. The replay log should also record the new channel, so
   verify the replay hook picks it up.

---

## Add a New `AGENTS.md`

Use when adding a new package under `internal/` or a new top-level
area. Don't add one for every directory. They should provide
direction the file layout itself doesn't.

1. **File.** Create `AGENTS.md` at the package root.
2. **Symlink.** `CLAUDE.md -> AGENTS.md` in the same directory.
3. **Shape.** One-sentence purpose → rules → layout → testing.
   Reference the nearest parent `AGENTS.md` when stating cross-cutting
   rules, rather than restating.
4. **Cross-link.** Add the new area to the package map in
   `internal/AGENTS.md` (or the root `AGENTS.md` for top-level areas).
5. **Tone.** Short, action-oriented. The root `AGENTS.md` sets the
   voice. Match it.

---

## WSL Dev Box Gotchas

- Every `.exe` suddenly exits 126 (`node.exe`, `powershell.exe`, the
  launcher): WSL lost its binfmt interop registration. `sudo systemctl
  restart systemd-binfmt` restores it. If it recurs, hunt what flushes
  binfmt after boot.

## See Also

- [`invariants.md`](invariants.md): the load-bearing rules each recipe
  must preserve.
- [`conventions.md`](conventions.md): contributor guardrails
  (file sizes, naming, tests).
- [`chat-rewrite.md`](chat-rewrite.md): the full item-model spec that
  these recipes operate inside.
- [`adrs/`](adrs/): architecture decisions that constrain how these
  playbooks can evolve.
