# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Ground-up rewrite of [`forge`](/Users/randy/repos/forge) optimizing for
performance, memory efficiency, and minimal code.

## Stack

- **Backend**: Go 1.26, Wails v3 (system webview shell only), SQLite via
  `modernc.org/sqlite` (pure Go, no CGO). WAL mode. Syntax-highlight
  spans via tree-sitter (`internal/highlight`) — the one cgo dependency
  besides the platform webview glue; grammars compile in with the
  standard toolchain (Windows WSL payload builds with gcc in WSL).
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript.
- **IPC**: HTTP+WebSocket via `internal/transport/`. Wails' binding generator
  still emits the typed TS wrappers; in production `@wailsio/runtime` resolves
  to `frontend/src/lib/transport/runtime.ts`, which forwards calls over WS.
  Server push goes through the per-channel event ring on the same connection.
  The same wire shape backs the embedded webview and `agent-overflow --connect`.
- **Providers**: Claude Code CLI (NDJSON over stdio) and Codex app-server
  (JSON-RPC 2.0 over stdio).

## Commands

Requires Go 1.26.2+, Node 24+, and pnpm 10+. On Linux, install
`libgtk-4-dev`, `libwebkitgtk-6.0-dev`, `pkg-config`, and `gcc`
before `make install` (the GTK4 / WebKitGTK 6.0 stack ships on
Ubuntu 23.04+ / Debian 13+).

- `make install` — installs `wails3` CLI (via `go.mod` tool directive) + pnpm deps
- `make dev` — dev mode, hot reload (local supervisor)
- `make build` — production build (`wails3 build`)
- `make go-build` — `go build ./...` with repo-standard platform env
- `make go-test` — `go test ./...` with repo-standard platform env
- `make check` — `make go-build` + `cd frontend && pnpm run check`
- `make test` — `make go-test` + `cd frontend && pnpm test`
- `make verify` — full release gate
- `make release` — builds direct-install artifacts in `dist/release/<version>/`
- `make harness` — boots the agent test harness (real app, isolated data
  dir, mocked providers); `make harness-window` opens the real webview
  window on it; `make e2e` runs the Playwright suite against it.
  `bin/ao-harness` (built by `make harness-build`) drives any instance
  from a shell: boot, seed, scenario, events, read-only db, semantic UI
  snapshots, perf runs, bench workloads, health rollup.
  See [docs/architecture/agent-harness.md](docs/architecture/agent-harness.md).
- `make soak` — boots a SECOND, fully isolated instance of the real
  Windows app (own profile: instance id, window, WebView2 dir, log, data
  dir) with harness-grade provider mocking, streaming background-subagent
  activity indefinitely. For hours-long renderer/hang reproductions
  beside your own running app; `make soak-check` summarizes it.
  `make soak-window` is the native-window equivalent on linux/macOS.
  See [docs/architecture/soak-rig.md](docs/architecture/soak-rig.md).
- `make provider-smoke` — manual real-provider gate; **spends real model
  tokens** and needs authenticated `claude` + `codex` CLIs on PATH. Run it
  before a release and after upgrading either provider CLI.
  See [providersmoke_test.go](providersmoke_test.go).
- `make import-corpus-smoke` — manual session-import gate; spends no tokens
  and spawns nothing, but needs a **copy** of your provider homes. The
  committed importer tests run on synthetic fixtures, which only know the
  shapes whoever wrote them knew about; this runs the Claude transcript
  reader, the Codex rollout reader, and the store writer over a real corpus
  and reports what it found (warnings by code, unknown wire types, corrupt
  lines, peak heap). Format drift shows up as a new code or a new unknown
  type; only a session that fails to load, convert, Build, or apply its batch
  to the throwaway store fails the gate.
  Point it at copies via `AO_IMPORT_CORPUS_CLAUDE` /
  `AO_IMPORT_CORPUS_CODEX` — a root that overlaps the live `~/.claude` or
  `~/.codex` is refused outright, and there is no fallback to a real home.
  Run it after upgrading either provider CLI and before shipping importer
  changes. Unlike `provider-smoke` it carries no build tag: `make go-test`
  compiles it and both legs skip when their variable is unset. See
  [importcorpussmoke_test.go](importcorpussmoke_test.go).

Every task must leave `make go-build`, `make go-test`,
`cd frontend && pnpm run check`, and `cd frontend && pnpm run build` passing.
On macOS, use the Make targets rather than bare `go build ./...` /
`go test ./...`; the Makefile exports the cgo deployment target flags Wails
needs to keep Objective-C objects and final binaries on the same minimum macOS
version.

## Core Principles

1. **Go is triage + pipe.** No event sourcing, no orchestration engine,
   no in-memory read models. The deliberate exceptions — lightweight
   coordination when brokering between multiple provider processes and the
   frontend (deliberation turn tracking, design option flow), and the workflows
   engine (`internal/workflow/`; spec: `docs/specs/workflows-system.md`), which
   sequences phases over the same thread/provider runtime — are coordination,
   not orchestration, and are called out where they live.
2. **Provider process is the source of truth during a turn.** Don't duplicate
   its state. Provider session files (`~/.claude/`, `~/.codex/`) are the
   authoritative history for crash recovery.
3. **SQLite is a history cache, not an event store.** Persist per-item on
   completion, not per-turn. Derived, version-stamped render metadata
   (`pathRefs`, highlight span blobs) may persist alongside history —
   it's cache content too: stale entries are dropped and recomputed,
   never migrated. Raw content stays canonical.
4. **Frontend memory is bounded by the visible thread.** Heavy payloads
   (diffs, command output, thinking) live in SQLite and load on demand.
5. **Errors are user-facing state, not log entries.**
6. **Provider-specific code stays in provider-specific packages.** Don't
   force a unified abstraction across Claude and Codex.
7. **Project ≠ workspace.** A project is the git repo. A workspace is where
   the provider operates (project root, or a separate worktree). Threads
   track both.

## Improving As You Go

This is a ground-up rewrite optimizing for performance, memory efficiency,
and minimal code. Treat those goals as ongoing: if you spot a chance to
improve architecture, cut allocations, tighten a hot path, or delete dead
code while working on something else, take it. Don't be afraid to change
existing code — nothing here is a cathedral yet. Don't leave the codebase
slightly worse than you found it because the improvement wasn't in the
ticket.

Guardrails:

- **Surface it.** Call out opportunistic changes alongside the primary
  change so they can be reviewed on their own merits, not buried in the
  diff.
- **Stay adjacent.** Fix what you're touching or immediately adjacent to.
  If a larger refactor looks warranted, propose it before starting.
- **Don't shortcut by duplicating.** If the right fix lives in shared
  code, change shared code — don't copy-paste a local workaround to avoid
  a broader edit. "Not my file" isn't a reason to work around a bug.
- **Don't violate Core Principles.** A cleanup that reintroduces in-memory
  read models, forces a unified Claude/Codex abstraction, etc. is not an
  improvement.
- **Reliability under partial/failure conditions counts as quality.**
  Streaming reconnects, provider restarts, partial NDJSON lines, session
  resume — if you notice brittle handling while you're in the area, fix
  it.

## Repo Map

```
/                             root guides (this file)
/main.go, /app.go, /app_*.go  Wails entry + bound methods
/cmd/                         alternative entry-point binaries (Windows WSL launcher, ao-mockprovider)
/internal/                    Go packages (see internal/AGENTS.md)
/frontend/                    Svelte 5 app (see frontend/AGENTS.md)
/e2e/                         Playwright suite for the agent test harness (see e2e/AGENTS.md)
/docs/architecture/           deep-dive design docs
/docs/GLOSSARY.md             coined vocabulary + terms with conflicting meanings across subsystems (wave, lane, spine, ghost, envelope, ...)
/docs/references/             external reference repos + spike policy
/docs/archive/                historical specs + ralph-loop artifacts
```

Area guides live alongside their code as `AGENTS.md` (with a `CLAUDE.md`
symlink). Start at the area closest to what you're touching — it will link
down if more depth is needed.

## Conventions

- Go: `internal/` for every non-main package. No `pkg/`.
- Svelte: runes only (`$state`, `$derived`, `$effect`, `$props`). No legacy
  stores or reactive `$:` syntax.
- Tailwind v4: CSS-native config via `@theme` in `app.css`. No
  `tailwind.config.js`.
- Wails bindings live in `frontend/bindings/` and are regenerated —
  never edit by hand. Always pass `-ts` to `wails3 generate bindings`
  so Wails emits TypeScript files instead of JS bindings.
- Events go Go → frontend via `a.emit(name, data)` (the transport-aware
  helper on `*App`); frontend calls Go via the typed wrappers in
  `frontend/src/lib/stores/bindings.ts`. Both flow through
  `internal/transport/` over the same WebSocket.

## When Behavior Is Unclear

If you're uncertain how Claude Code, Codex, or an external tool behaves,
**do not guess from this repo**. Write a small isolated spike test outside
the project to confirm the behavior, then port the learning in. See
[docs/references/spike-policy.md](docs/references/spike-policy.md).

## Reference Repos

- **forge** (`/Users/randy/repos/forge`) — the Node/Effect project this one
  rewrites. UX and provider-handling reference. See
  [docs/references/forge.md](docs/references/forge.md).
- **Claude Code source** (`/Users/randy/repos/claude-code-source-code`) —
  TypeScript source of an older Claude Code release. Use when binary
  behavior is unclear; cross-check against the installed binary because
  the local copy can lag. See
  [docs/references/claude.md](docs/references/claude.md).
- **Codex source** (`/home/rmurphy/repos/codex`, upstream
  https://github.com/openai/codex) — authoritative Codex CLI and
  app-server behavior.
- **CodexMonitor** (https://github.com/Dimillian/CodexMonitor) — Tauri,
  feature-complete reference implementation of a Codex app-server client.

See [docs/references/codex.md](docs/references/codex.md) for how to use
these when touching Codex code, and
[docs/references/claude.md](docs/references/claude.md) for Claude.

**Codex background terminals:** `exec_command` can yield back to the
model while the PTY keeps running; `source: "unifiedExecStartup"` is the
wire-typed signal for these background terminals. Per-process
termination is available since codex 0.140.0
(`thread/backgroundTerminals/terminate {threadId, processId}`), alongside
`list` and the thread-wide `clean`. See
[docs/references/codex.md §Background terminals](docs/references/codex.md#background-terminals)
and [invariant 25](docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics).
What remains client-unreachable is killing a spawned collab-agent child
thread — `close_agent` is a model tool only.

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
  Wails3 dev watcher.** Claude Code's parallel-agent harness creates
  full-repo worktrees under `.claude/worktrees/agent-*/` whenever an
  agent is spawned with `isolation: "worktree"`. Each contains
  hundreds of `.go` / `.ts` files matching the dev_mode
  watched_extension patterns; without the explicit exclude in
  `build/config.yml#dev_mode.ignore.dir`, Wails3 registers thousands
  of fsnotify watches at startup and the dev process crashes (incident
  2026-05-02 — backend rebuild storm + WebSocket disconnect cascade,
  visible as repeated HMR-update messages in the dev log). `git_ignore:
  true` is set, but it has not been enough on its own — keep the
  explicit dir-level exclude in place. The same exclusion is mirrored
  defensively in `frontend/vite.config.ts#server.watch.ignored` even
  though those paths sit outside Vite's project root.

- **Tests MUST never reach a real provider binary or the developer's
  real provider homes.** `make go-test` runs on machines whose
  `~/.claude` / `~/.codex` hold live logins. Claude refresh tokens are
  single-use: a test that spawns the real CLI and then kills it (every
  fixture teardown does) can consume a refresh token without persisting
  the rotation, which destroys the developer's login hours later — and
  every leaked session burns real, billed tokens (incidents 2026-07-29:
  HOME-unisolated startup test pruned all saved credential slots;
  2026-08-03: workflow wake delivery spawned 143 real Claude sessions
  over nine days and killed the active account's OAuth grant). Spawning
  a real CLI is what `make provider-smoke` is for — an explicit, manual,
  token-spending gate — never `make go-test`. Enforcement:
  `setupE2EApp` and `newTestAppWithStore` both poison the provider
  binary settings, stub text generation and the live Codex model
  catalog, detach HOME/USERPROFILE, and fail any test that still spawns
  (`app_e2e_isolation_test.go`, thin glue over `internal/kerneltest`,
  which holds the importable guard so a fixture in ANY package can
  install it); `resolveTextGenerationExecutor`
  additionally refuses real CLI execution inside any test binary, and
  the boot prune refuses a metadata store whose `providerHome` stamp
  does not match the credential home; a session-starting test installs
  `testutil.WriteMockClaudeScript` / `WriteMockCodexSession` over the
  poison. Any NEW fixture that constructs an `*App` able to start
  sessions, and any new spawn path (probes, catalogs, textgen-style
  side effects), must be wired into the same guard — via
  `kerneltest.IsolateSpawns` if it lives outside package `main` —
  mocking stays mandatory-by-default, never opt-in per test.

## Deferred (Not Currently in Scope)

These are intentional non-goals for the current phase — don't implement
them without a scope conversation first.

- **Correction-needed / mid-turn correction flow.** Forge has this as
  a workflow/gate mechanic (`thread.correct` command, guidance channel
  projection, `correction-needed` interactive-request kind). Workflows are
  landing under `docs/specs/workflows-system.md`, but this correction flow
  remains deferred pending its own scope conversation. It does not map to a
  Codex or Claude wire-level event, and t3-code (the
  reference UX we most closely track) doesn't implement either.
  If a "let me course-correct mid-turn" primitive is wanted independently,
  it becomes its own feature, not forge parity.
