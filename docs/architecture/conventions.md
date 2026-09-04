# Conventions

Read this before any change. These are the "keep it clean" guardrails.
Each one exists because we've either paid for violating it or because the
top-level principles demand it.

If you're looking for step-by-step recipes rather than rules, see
[`how-to.md`](how-to.md). For the load-bearing rules that must never bend,
see [`invariants.md`](invariants.md).

## File Size Targets

| Kind | Target | Hard ceiling |
|---|---|---|
| Go source file | ≤ 500 lines | 800 |
| Svelte component | ≤ 300 lines | 500 |
| Function / method | ≤ 80 lines | 150 |

When you cross a target, split. Don't stretch. The triage router is the
live example: `internal/triage/router.go` is the dispatch spine, and every
concern (tool lifecycle, turn lifecycle, stream state, approvals, payload
items) lives in a sibling file with a single owner. See the "Layout"
section in `internal/triage/AGENTS.md` for the map.

Recipe for splitting a file lives in [`how-to.md`](how-to.md#split-a-file-when-it-crosses-its-size-ceiling).

## Naming Conventions

Prefix verbs carry meaning. Pick the one whose implied contract you're
honoring:

| Prefix | Meaning | Example |
|---|---|---|
| `handleFoo` | Triage event handler: consumes one `ProviderEvent`, emits zero or more routing decisions. | `Router.handleTurnStart`, `Router.handleToolComplete` |
| `persistFoo` | Store write + event emission. Single chokepoint for persisted timeline rows. | `Router.persistItem` |
| `parseFoo` | Wire-format parsing: turns bytes / JSON into a typed value. Never writes state. | `Parser.ParseLine`, `appendToolUseEvent` |
| `emitFoo` | `app.Event.Emit` wrapper or frontend channel publish. | `Router.emitItemUpsert`, `App.emitErrorToThread` |
| `buildFoo` | Pure construction: takes inputs, returns a value. No I/O, no side effects. | `buildDiscussionParticipantPlans`, `buildRevertAffectedFiles` |

Svelte file suffix rules:

- `.svelte.ts`: reactive state owner (`$state`, `$derived`, `$effect`).
  Example: `stores/thread.svelte.ts`, `stores/threads.svelte.ts`.
- Plain `.ts`: pure helpers, no runes. Example: `utils/patchFiles.ts`,
  `utils/subagentGrouping.ts`.

If you're unsure which suffix to use, ask: "does this file need to be
reactive on its own?" If no, plain `.ts`.

## Error Handling

- Wrap with `%w` only when callers need to `errors.Is` / `errors.As` on the
  returned value. Otherwise use `%v`. Wrapping pulls the original into a
  chain; most of ours exist so a higher layer can test a sentinel. See
  `ErrUnhandledEventKind` in the triage router for a sentinel worth
  wrapping; see `fmt.Errorf("commit legacy migration backfill: %v", err)`
  in `store/migrate.go` for the normal case.
- **Never** `_ = err` without a comment explaining why the error is
  provably ignorable at this point.
- **No silent swallow.** At minimum log with enough context to reproduce.
  Parser errors in Claude's read loop are the canonical example. They
  log and keep reading so one bad line doesn't kill the session.
- **No panics in production code paths.** Panics are for programmer error
  (e.g., "invariant violated"), not for user data or wire input. Tests may
  use `t.Fatal` freely; production code returns errors.
- Errors the user should see are user-facing state, not log lines
  (core principle 5). If a failure matters to the user, surface it as a
  toast, status banner, or error row. Don't bury it in a log file.

## Magic Numbers

Every magic number gets a named constant with clear scope. The constant
lives in the package that owns the behavior it encodes.

Canonical examples:

- `defaultQueueSize = 4096` (`internal/observability/replay/manager.go`):
  the replay queue capacity. Tests reference the same constant rather
  than hardcoding `4096`.
- `defaultIdleTimeout = 5 * time.Minute` (replay manager): idle reaper
  threshold.
- `defaultMaxBytes = 100 * 1024 * 1024` (replay writer): rotation size.

If a number lives in more than one file, either the constant is in the
wrong place or a helper should own the logic that uses it. Don't grep-fix.

## Test Discipline

- **No `time.Sleep` in tests.** None. If the code needs to coordinate
  something asynchronous, expose a seam the test can deterministically
  synchronize on. The triage router's `SetEventHook` (see `router.go`)
  is the pattern: production leaves the hook nil, tests install one
  and block on the channel it fires.
- **No real `setTimeout` in Vitest tests without fake timers.** Real
  timers make tests flaky and slow. Use `vi.useFakeTimers()` and
  `vi.advanceTimersByTime` when the code under test uses timers.
- **Prefer deterministic channels and state** over polling-until-true.
  `synctest` is fine where it fits (Go 1.25+); `eventually` loops are a
  smell.
- **Test seams go in production code, not test files**, but they stay
  nil / no-op in production paths. `SetEventHook` is the reference.
- Tests must be deterministic. If a test needs timing, use
  `t.Setenv("TMPDIR", t.TempDir())` or per-test fixtures. Never scan
  shared system state. Past flakes that violated this are documented in
  the test-flake history.

## SQL / Store Patterns

- **Always use `?` placeholders.** Never `fmt.Sprintf` or `+` values into
  a query string. We've had zero SQL-injection incidents and intend to
  keep it that way.
- **Index every column used in a `WHERE`.** SQLite will table-scan
  otherwise. Partial indexes (`WHERE col <> ''`) keep the index small on
  sparse columns. See `idx_items_parent`, `idx_items_completion_of`,
  `idx_items_payload_id`, `idx_items_meta_task_id` in
  `internal/store/migrate.go`.
- **Narrow SELECT projections.** Prefer `SELECT id, kind, summary`
  over `SELECT *`. The payload `data` BLOB is the reason: pulling it
  implicitly on every item list would defeat the on-demand model.
- **Every migration has a test.** Every new column, index, or CHECK
  constraint. `internal/store/migrate_test.go` is where they live; the
  bar is "the schema assertion would fail without this migration."
- **CHECK constraints belong in SQL**, not Go. Enum columns use
  `CHECK(kind IN ('...', '...'))` and the test proves the constraint
  fires.
- **Never edit a shipped migration.** Add a new one. Migrations are
  forward-only and append-only.

## Performance Heuristics

- **No O(N²) on hot paths.** Summary updates for streaming text append,
  they don't read+rewrite. Tool completion lookup by task id uses the
  partial index `idx_items_meta_task_id` rather than scanning every item
  on a thread.
- **No full-blob reads for previews.** Use `payload.meta` for the
  preview/stats card, `payload.data` only when the user expands. The
  frontend `ListItems` path deliberately omits `data`; `ReadPayload`
  is the explicit on-demand fetch.
- **Incremental derivation in Svelte.** If you add a derived index over
  timeline data, derive per-unit (per turn, per card, per item) and
  update only the affected unit on upsert. Rebuilding the whole item
  list on every upsert is a bug unless the list is explicitly tiny and
  bounded.
- **No ad-hoc indexes.** If the frontend needs to look up items by
  parent_id, the store already has the partial index. Use it via a
  store query rather than scanning the full items array in Go.

## Memory Hygiene

Every long-lived map has a cleanup path. If a map keeps growing with
thread lifetime, per-turn state, or correlation data, the cleanup is
mandatory. The triage router's `AGENTS.md` (see
[`internal/triage/AGENTS.md`](../../internal/triage/AGENTS.md)) documents the current set:

- Per-turn state clears on `EventTurnComplete` (and on the errored-turn
  branch).
- Per-thread state clears on `CleanupThread`.
- Approval / interrupt-queue entries clear when their correlated event
  resolves.

**If you add a new map, document its cleanup path in the owning
`AGENTS.md` in the same commit.** No silent long-lived state.

Soft caps on dedup sets: if you add a long-lived dedup/correlation set,
put a cap or an explicit clear at the lifetime boundary. The current
Claude task lifecycle avoids parser-level completion dedupe; coalescing
happens through stable sibling ids in triage.

## Svelte 5

- **Runes only.** `$state`, `$derived`, `$effect`, `$props`. No
  `export let`, no `$:` reactive labels, no legacy stores.
- **Components stay small.** Extract before stretching. See the
  file-size targets above.
- **Reactive state files end in `.svelte.ts`.** Pure helpers are plain
  `.ts`.
- **No business logic in templates.** Derive in `<script>`, render in
  the template. Templates should read like HTML with variable
  substitution, nothing more.
- **Typed bindings.** Import from `stores/bindings.ts`. Never call
  `window.runtime` directly; never edit `frontend/bindings/` by hand.
  Regenerate with `wails3 generate bindings -ts`. Always pass `-ts` so
  Wails emits TypeScript rather than JS bindings. `build/Taskfile.yml`'s
  `generate:bindings` target runs it with `-clean=true` and the build flags.
- **`make build` always rebuilds the frontend and the bindings.** The
  `build:frontend` and `generate:bindings` tasks run with `method: none`,
  no source fingerprint. Task's checksum walker follows symlinks and
  swallows a glob error, so a single dangling symlink under the tree (a
  gitignored `node_modules` left behind by a relocated package was
  enough) empties the source set and freezes the fingerprint at "up to
  date" forever; on
  2026-09-03 that shipped a pre-merge page inside a post-merge backend,
  and the app booted to nothing but HTTP 404 toasts. Don't reintroduce
  `sources:` on a task whose output is embedded in the binary.
- **Heavy content on demand.** Fetch diffs, command output, and thinking
  via a Wails binding when the user expands. Don't preload. The `items`
  list the frontend receives already omits `payload.data`; fetching it
  is the explicit action.

## Comments

- **No WHAT-comments.** The code already says what. `// increment i`
  is noise; if the intent isn't clear from the line itself, rename the
  variable or extract the operation.
- **WHY-comments are the only kind worth writing.** Why is this guard
  here? Why this order of operations? Why does this handler defer the
  index assignment? The router's `handleTurnComplete` is full of these.
  Each paragraph explains a subtle ordering constraint that would
  otherwise be reintroduced as a bug.
- **Never reference PRs or tickets in comments.** Git history is the
  record; comments are for future readers who don't have that context
  at hand. `// see #1234` rots the moment the ticket tracker moves.
- **Comments are maintained with the code.** If you change behavior,
  update the comment in the same commit. A stale comment is worse than
  no comment.

## Maintaining the Guides

The `AGENTS.md` files and the `docs/` tree are a cache over the code:
useful exactly as long as they are true. Four rules keep them true.

- **Route a new fact to where a future reader looks first, once.** A
  rule agents must obey in one area → that area's `AGENTS.md`. A
  cross-cutting mechanism → `docs/architecture/`. Rationale for a
  choice → the commit message, or an ADR when it is load-bearing. An
  owner ruling, a rejected proposal, or a thing built and torn out →
  `docs/decisions.md`, one line each, so no session re-proposes it. A
  coined term → `docs/GLOSSARY.md`. A code-local subtlety → a
  WHY-comment. If the environment already answers it (a Makefile
  target, `--help`, a config file), leave it there: a doc restating a
  lookup goes stale, the lookup cannot.
- **Sweep for falsified claims before reporting a change done.** A
  behavior change can invalidate doc prose far from the edited files,
  so `rg` the changed symbols and the behavior phrases they implement
  across `**/AGENTS.md` and `docs/`, read each hit, and fix every
  claim the change made false, in the same commit. Done means every
  doc claim about the touched behavior is verified true or updated.
  The class this closes: the 2026-08-29 eventbus change made "no later
  frame announces a drop" false in two documents at once.
- **Retire prose that enforcement replaced.** When a rule gains a
  tripwire test, lint, or type shape, shrink its guide bullet to the
  claim plus a pointer at the enforcement; the test carries the weight
  from then on. Delete a doc nothing cites (the spec-graduation rule
  in `docs/README.md`, generalized to the whole tree); git history
  keeps it. Guides earn their load by staying short enough to read.
- **Keep the indexes in step.** Adding, renaming, or deleting a doc
  updates its `docs/README.md` row in the same commit. A new package
  updates the `internal/AGENTS.md` table and ships the `CLAUDE.md`
  symlink (§ Adding a package there).

When writing the entry itself: cache what the code cannot say — the
unwritten convention, the reason, the gotcha. One meaning lives in one
place; elsewhere, point. An incident citation is one sentence, the date
and the mechanism.

## Before You Commit

Every task leaves these passing:

- `make go-build`
- `make go-test`
- `cd frontend && pnpm run check`
- `cd frontend && pnpm run build`

Plus the falsified-claim sweep above: every doc claim about the
behavior you changed is verified true or updated.

If any are broken, fix them before the commit lands. "Out of scope" is
not a valid reason to leave a check red. See the Ownership section of
the root `AGENTS.md`.
