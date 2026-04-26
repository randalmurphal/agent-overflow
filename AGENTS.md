# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Ground-up rewrite of [`forge`](/Users/randy/repos/forge) optimizing for
performance, memory efficiency, and minimal code.

## Stack

- **Backend**: Go 1.25, Wails v3 (system webview shell only), SQLite via
  `modernc.org/sqlite` (pure Go, no CGO). WAL mode.
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript.
- **IPC**: HTTP+WebSocket via `internal/transport/`. Wails' binding generator
  still emits the typed TS wrappers; in production `@wailsio/runtime` resolves
  to `frontend/src/lib/transport/runtime.ts`, which forwards calls over WS.
  Server push goes through the per-channel event ring on the same connection.
  The same wire shape backs the embedded webview and `agent-overflow --connect`.
- **Providers**: Claude Code CLI (NDJSON over stdio) and Codex app-server
  (JSON-RPC 2.0 over stdio).

## Commands

Requires Go 1.25+ and Node 24+. On Linux, install
`libgtk-3-dev`, `libwebkit2gtk-4.1-dev`, `pkg-config`, and `gcc`
before `make install`.

- `make install` — installs `wails3` CLI (via `go.mod` tool directive) + npm deps
- `make dev` — dev mode, hot reload (`wails3 dev`)
- `make build` — production build (`wails3 build`)
- `make go-build` — `go build ./...` with repo-standard platform env
- `make go-test` — `go test ./...` with repo-standard platform env
- `make check` — `make go-build` + `npm run check`
- `make test` — `make go-test` + `npm test`

Every task must leave `make go-build`, `make go-test`, `npm run check`, and
`npm run build` passing. On macOS, use the Make targets rather than bare
`go build ./...` / `go test ./...`; the Makefile exports the cgo deployment
target flags Wails needs to keep Objective-C objects and final binaries on the
same minimum macOS version.

## Core Principles

1. **Go is triage + pipe.** No event sourcing, no orchestration engine,
   no in-memory read models. The one deliberate exception — lightweight
   coordination when brokering between multiple provider processes and the
   frontend (deliberation turn tracking, design option flow) — is coordination,
   not orchestration, and is called out where it lives.
2. **Provider process is the source of truth during a turn.** Don't duplicate
   its state. Provider session files (`~/.claude/`, `~/.codex/`) are the
   authoritative history for crash recovery.
3. **SQLite is a history cache, not an event store.** Persist per-item on
   completion, not per-turn.
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
/cmd/                         alternative entry-point binaries (Windows WSL launcher)
/internal/                    Go packages (see internal/AGENTS.md)
/frontend/                    Svelte 5 app (see frontend/AGENTS.md)
/docs/architecture/           deep-dive design docs
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
- **Codex source** (`/Users/randy/repos/codex-source`, upstream
  https://github.com/openai/codex) — authoritative Codex CLI and
  app-server behavior.
- **CodexMonitor** (https://github.com/Dimillian/CodexMonitor) — Tauri,
  feature-complete reference implementation of a Codex app-server client.

See [docs/references/codex.md](docs/references/codex.md) for how to use
these when touching Codex code, and
[docs/references/claude.md](docs/references/claude.md) for Claude.

**Known upstream constraint (Codex):** `exec_command` can yield back to
the model while the PTY keeps running; `source: "unifiedExecStartup"` is
the wire-typed signal for these background terminals. The app-server
protocol exposes only thread-wide `thread/backgroundTerminals/clean` —
per-process termination requires an upstream change. See
[docs/references/codex.md §Known upstream constraints](docs/references/codex.md#known-upstream-constraints)
and [invariant 25](docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics).

## Permanent invariants

- **Transport boundary stays clean.** Go → frontend goes through
  `app.Event.Emit` and Wails bindings only; UI code must never reach
  into Go internals in ways that lock out the existing HTTP+WS
  network transport. The webview path and the `--connect` remote
  client share the same wire shape (`internal/transport/frame.go`);
  any new App-bound method you add immediately becomes both a Wails
  binding and a wire RPC — that's deliberate, don't add a parallel
  back-channel. Methods that touch the local FS, spawn external
  processes (provider CLIs, git, gh), control provider sessions,
  mutate settings, or write attachments must additionally be
  classified into `internal/transport/internalmethods.go`
  `LocalOnlyMethods` so they're refused from non-loopback peers.
  The dispatcher returns the same `method_not_found` shape for both
  unregistered and LAN-blocked methods, so the privileged surface
  stays unenumerable from the wire.

## Deferred (Not Currently in Scope)

These are intentional non-goals for the current phase — don't implement
them without a scope conversation first.

- **Workflow / phase / gate system.** Forge has one
  (`apps/server/src/workflow/`); the underlying idea ported from
  `/Users/randy/repos/orc` (see `docs/specs/TASK_TEMPLATES.md`,
  `docs/decisions/ADR-007-human-gates.md`). Not a core feature for v1.
- **Auto-updater wiring.** Wails v3 ships a built-in updater
  (https://v3alpha.wails.io/guides/distribution/auto-updates/); enable
  it when we're ready to distribute. No custom updater required.
- **Correction-needed / mid-turn correction flow.** Forge has this as
  a workflow/gate mechanic (`thread.correct` command, guidance channel
  projection, `correction-needed` interactive-request kind). Both are
  tightly coupled to workflows — which we've also deferred — and
  neither maps to a Codex or Claude wire-level event. t3-code (the
  reference UX we most closely track) doesn't implement either.
  Revisit only if workflows land. If a "let me course-correct
  mid-turn" primitive is wanted independently, it becomes its own
  feature, not forge parity.

## Implemented (was previously deferred)

- **Remote / web access** — implemented across `internal/transport/`
  (HTTP+WebSocket dispatch + event push), `internal/clientmode/`
  (`agent-overflow --connect <url>` stub), and the LAN-bind toggle in
  Settings. Method-level authz refuses RCE-equivalent and
  settings-mutation methods from non-loopback peers
  (`internal/transport/internalmethods.go` `LocalOnlyMethods`). TLS
  termination is out-of-process — public exposure goes behind
  Tailscale Serve / SSH tunnel / reverse proxy.
