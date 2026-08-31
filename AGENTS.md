# Agent Overflow

Desktop app for driving coding agents (Claude Code, Codex) through one
shared UX. Optimized for performance, memory efficiency, and minimal code.

## Stack

- **Backend**: Go 1.26, Wails v3 (system webview shell only), SQLite via
  `modernc.org/sqlite` (pure Go, no CGO). WAL mode. Syntax-highlight
  spans via tree-sitter (`internal/highlight`), the one cgo dependency
  besides the platform webview glue; grammars compile in with the
  standard toolchain (the Windows WSL payload builds with gcc in WSL).
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4,
  TypeScript.
- **IPC**: HTTP+WebSocket via `internal/transport/`. Wails' binding
  generator still emits the typed TS wrappers; in production
  `@wailsio/runtime` resolves to `frontend/src/lib/transport/runtime.ts`,
  which forwards calls over WS. Server push goes through the per-channel
  event ring on the same connection. The same wire shape backs the
  embedded webview and `agent-overflow --connect`.
- **Providers**: Claude Code CLI (NDJSON over stdio) and Codex app-server
  (JSON-RPC 2.0 over stdio).

## Commands

Requires Go 1.26.2+, Node 24+, and pnpm 10+. On Linux, install
`libgtk-4-dev`, `libwebkitgtk-6.0-dev`, `pkg-config`, and `gcc` before
`make install` (the GTK4 / WebKitGTK 6.0 stack ships on Ubuntu 23.04+ /
Debian 13+).

| Target | What it does |
|---|---|
| `make install` | `wails3` CLI (via the `go.mod` tool directive) + pnpm deps |
| `make dev` | dev mode, hot reload (local supervisor) |
| `make build` | production build (`wails3 build`) |
| `make go-build` / `make go-test` | `go build ./...` / `go test ./...` with repo-standard platform env |
| `make check` | `make go-build` + frontend `pnpm run check` |
| `make test` | `make go-test` + frontend `pnpm test` |
| `make verify` | full hermetic release gate, including a compile-only check of the real-provider smoke tests |
| `make release` | direct-install artifacts in `dist/release/<version>/` |
| `make harness` | real app, isolated data dir, mocked providers; `harness-window` / `harness-wsl` open a real window on it, `make e2e` runs Playwright against it, `bin/ao-harness` drives any instance from a shell. See [agent-harness.md](docs/architecture/agent-harness.md). |
| `make soak` | the harness shell plus the indefinite streaming preset, for hours-long renderer reproductions beside your own app; `soak-check` summarizes, `soak-window` is the native equivalent. See [soak-rig.md](docs/architecture/soak-rig.md). |
| `make provider-smoke` | manual real-provider gate. **Spends real model tokens**; needs authenticated `claude` + `codex` on PATH. Run before a release and after upgrading either provider CLI. See [providersmoke_test.go](internal/app/providersmoke_test.go). |
| `make import-corpus-smoke` | manual session-import gate over a **copy** of your provider homes (`AO_IMPORT_CORPUS_CLAUDE` / `AO_IMPORT_CORPUS_CODEX`; a root overlapping a live home is refused, and there is no fallback). Spends no tokens. Run after provider CLI upgrades and before importer changes. See [importcorpussmoke_test.go](internal/app/importcorpussmoke_test.go). |

Every task must leave `make go-build`, `make go-test`,
`cd frontend && pnpm run check`, and `cd frontend && pnpm run build`
passing. On macOS, use the Make targets rather than bare
`go build ./...` / `go test ./...`: the Makefile exports the cgo
deployment-target flags Wails needs to keep Objective-C objects and
final binaries on the same minimum macOS version.

## Core Principles

1. **Go is triage + pipe.** No event sourcing, no orchestration engine,
   no in-memory read models. The deliberate exceptions are coordination,
   not orchestration, and are called out where they live: lightweight
   brokering between provider processes and the frontend (deliberation
   turn tracking), and the workflows engine
   (`internal/workflow/`; spec: `docs/specs/workflows-system.md`), which
   sequences phases over the same thread/provider runtime.
2. **Provider process is the source of truth during a turn.** Don't
   duplicate its state. Provider session files (`~/.claude/`,
   `~/.codex/`) are the authoritative history for crash recovery.
3. **SQLite is a history cache, not an event store.** Persist per-item on
   completion, not per-turn. Derived, version-stamped render metadata
   (`pathRefs`, highlight span blobs) may persist alongside history as
   cache content: stale entries are dropped and recomputed, never
   migrated. Raw content stays canonical.
4. **Frontend memory is bounded by the visible thread.** Heavy payloads
   (diffs, command output, thinking) live in SQLite and load on demand.
5. **Errors are user-facing state, not log entries.**
6. **Provider-specific code stays in provider-specific packages.** Don't
   force a unified abstraction across Claude and Codex.
7. **Project ≠ workspace.** A project is the git repo. A workspace is
   where the provider operates (project root, or a separate worktree).
   Threads track both.
8. **Every platform is production.** macOS, Windows (the WSL launcher),
   and Linux; embedded webview and `--connect` browser alike. Paths,
   spawning, and filesystem code must hold on all of them: build paths
   with `filepath`, assume nothing about home layout or case
   sensitivity, and put platform behavior behind the existing
   `*_darwin.go` / `*_windows.go` splits rather than runtime guesses.

## Working In This Repo

- **Fix the root cause.** If the fix you are writing is a workaround,
  the code underneath is wrong: fix that, or surface the tradeoff and
  get approval before settling.
- **Close the class, not the instance.** When a bug can recur, make it
  structural: narrow the API, validate inside the function, add the
  tripwire test or lint. Then sweep for siblings of the same pattern.
- **Consider every place.** A change to a shared shape updates every
  caller, every sibling path with the same pattern, and both providers
  when it applies to both. Compiling is not the same as complete.
- **Prefer clean, simple solutions.** Minimal code is a project goal. A
  solution that needs a paragraph of justification is usually wrong;
  resist speculative states, modes, and knobs nobody asked for.
- **Write through the performance lens, always.** Visual performance,
  memory consumption, and the actual work a change causes are weighed on
  every edit, not tuned later: do the least work needed, allocate the
  least that suffices, and stay correct under partial failure. Applies
  to all code, hot path or not.
- **Visible UI behavior changes only with approval.** Perf, refactor,
  and bug-fix work keeps pixels, motion, and interactions identical
  unless the visible change is itself what was requested.
- **A fixed bug ships its lesson.** When the bug's class could recur,
  update the nearest AGENTS.md (or the doc it points to) in the same
  change.
- **A change keeps the guides true.** Before reporting done, sweep
  `**/AGENTS.md` and `docs/` for claims your change falsified and fix
  them in the same commit. Full maintenance rules (fact routing, the
  sweep, retiring enforced prose, index sync):
  [conventions.md § Maintaining the Guides](docs/architecture/conventions.md#maintaining-the-guides).

## Improving As You Go

Performance, memory efficiency, and minimal code are ongoing goals: if
you spot a chance to improve architecture, cut allocations, tighten a
hot path, or delete dead code while working on something else, take it.
Nothing here is a cathedral yet. Don't leave the codebase slightly worse
than you found it because the improvement wasn't in the ticket.

Guardrails:

- **Surface it.** Call out opportunistic changes alongside the primary
  change so they can be reviewed on their own merits.
- **Stay adjacent.** Fix what you're touching or immediately adjacent
  to. Propose larger refactors before starting them.
- **Don't shortcut by duplicating.** If the right fix lives in shared
  code, change the shared code. "Not my file" isn't a reason to work
  around a bug.
- **Don't violate Core Principles.** A cleanup that reintroduces
  in-memory read models or forces a unified Claude/Codex abstraction is
  not an improvement.
- **Reliability under partial/failure conditions counts as quality.**
  Streaming reconnects, provider restarts, partial NDJSON lines, session
  resume: if you notice brittle handling while you're in the area, fix
  it.

## Repo Map

```
/                             root guides + executable bootstrap (`main*.go`, `service.go`)
/internal/app/                Wails service shell, bound methods, integration tests
/cmd/                         alternative entry-point binaries (Windows WSL launcher, ao-mockprovider)
/internal/                    Go packages (see internal/AGENTS.md)
/frontend/                    Svelte 5 app (see frontend/AGENTS.md)
/e2e/                         Playwright suite for the agent test harness (see e2e/AGENTS.md)
/docs/architecture/           deep-dive design docs
/docs/GLOSSARY.md             coined vocabulary + terms with conflicting meanings across subsystems
/docs/references/             provider wire references + spike policy
```

Area guides live alongside their code as `AGENTS.md` (with a `CLAUDE.md`
symlink). Start at the area closest to what you're touching; it links
down if more depth is needed.

## Conventions

- Go: `internal/` for every non-main package. No `pkg/`.
- Svelte: runes only (`$state`, `$derived`, `$effect`, `$props`). No
  legacy stores or reactive `$:` syntax.
- Tailwind v4: CSS-native config via `@theme` in `app.css`. No
  `tailwind.config.js`.
- Wails bindings live in `frontend/bindings/` and are regenerated, never
  edited by hand. Always pass `-ts` to `wails3 generate bindings`.
- Events go Go → frontend via `a.emit(name, data)` (the transport-aware
  helper on `*App`); frontend calls Go via the typed wrappers in
  `frontend/src/lib/stores/bindings.ts`. Both flow through
  `internal/transport/` over the same WebSocket.

## When Behavior Is Unclear

If you're uncertain how Claude Code, Codex, or an external tool behaves,
**do not guess from this repo**. Write a small isolated spike test
outside the project to confirm the behavior, then port the learning in.
See [docs/references/spike-policy.md](docs/references/spike-policy.md).

## References

- **Claude Code source**: a local checkout of the CLI's TypeScript
  source. Location, caveats, and how it lags the installed binary:
  [docs/references/claude.md](docs/references/claude.md).
- **Codex source** (https://github.com/openai/codex): authoritative
  Codex CLI and app-server behavior. How to use it:
  [docs/references/codex.md](docs/references/codex.md).
- **CodexMonitor** (https://github.com/Dimillian/CodexMonitor): Tauri,
  feature-complete reference implementation of a Codex app-server
  client.
- **Wire references**: `docs/references/claude-wire.md` and
  `docs/references/codex-wire.md` are the single sources of truth for
  parser work on either provider.

## Permanent invariants

- **Transport boundary stays clean.** Go → frontend goes through
  `app.Event.Emit` and Wails bindings only; UI code must not add a
  back-channel that bypasses `internal/transport/`. The embedded
  webview, `agent-overflow --connect`, and remote browser access share
  the same HTTP+WS wire shape. Any new App-bound method also becomes a
  wire RPC; if it touches local FS, external processes, provider
  sessions, settings, credentials, or attachments, classify it in
  `internal/transport/internalmethods.go` `LocalOnlyMethods`. See
  `internal/transport/AGENTS.md` for the authz and replay rules.

- **`.claude/` and `.playwright-mcp/` MUST stay excluded from the
  Wails3 dev watcher.** Claude Code's worktree isolation creates
  full-repo checkouts under `.claude/worktrees/agent-*/`; each one
  matches thousands of watched extensions, and without the explicit
  exclude in `build/config.yml#dev_mode.ignore.dir` the fsnotify watch
  storm crashes the dev process (incident 2026-05-02). `git_ignore:
  true` alone was not enough; keep the dir-level exclude, and its
  defensive mirror in `frontend/vite.config.ts#server.watch.ignored`.

- **Tests MUST never reach a real provider binary or the developer's
  real provider homes.** `make go-test` runs on machines whose
  `~/.claude` / `~/.codex` hold live logins. Claude refresh tokens are
  single-use, so a test that spawns and kills the real CLI can destroy
  the developer's login hours later, and every leaked session burns
  real, billed tokens (incidents 2026-07-29 and 2026-08-03: wiped
  credential slots, 143 leaked real sessions, a dead OAuth grant).
  Spawning a real CLI is what `make provider-smoke` is for, never
  `make go-test`. Enforcement lives in `internal/kerneltest` (see its
  AGENTS.md): `setupE2EApp` and `newTestAppWithStore` poison provider
  binaries, stub text generation and the Codex catalog, detach
  HOME/USERPROFILE, and fail any test that still spawns;
  `resolveTextGenerationExecutor` refuses real CLI execution inside any
  test binary; the boot prune refuses a store whose `providerHome`
  stamp mismatches the credential home. Any NEW fixture that constructs
  a session-capable `*App`, and any new spawn path, must wire into the
  same guard (`kerneltest.IsolateSpawns` outside package `main`).
  Mocking is mandatory-by-default, never opt-in per test.

## Deferred (Not Currently in Scope)

Intentional non-goals for the current phase. Don't implement without a
scope conversation first.

- **Correction-needed / mid-turn correction flow.** A workflow/gate
  mechanic for steering an agent mid-turn. It maps to no Codex or
  Claude wire-level event, and t3-code (still a UX reference for some
  surfaces, though core functionality has diverged) doesn't implement
  one either. If a "course-correct mid-turn" primitive is wanted, it
  becomes its own feature with its own design.
