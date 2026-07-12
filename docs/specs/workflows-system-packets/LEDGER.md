# M0 Delegation Ledger

Campaign: workflows-system M0 (four parallel lanes off one base commit).
Lanes live under `~/repos/ao-lanes/`; logs under the orchestrating session's
scratchpad. Review protocol: WIP-commit the lane immediately on return, then
scope/assumptions/gaming audits + independent gate re-runs before merge.

| Packet | Branch | Lane | Model / effort | Session id | Status |
|---|---|---|---|---|---|
| P0.1 turn-observer registry | `m0/p01-turn-observers` | `~/repos/ao-lanes/p01` | gpt-5.6-sol / high | `019f5435-f377-7cf3-a60f-54a6ca10a902` | BLOCKED (baseline test-race) |
| P0.2 project slugs + config dirs | `m0/p02-project-slugs` | `~/repos/ao-lanes/p02` | gpt-5.6-sol / high | `019f5436-71b2-7c21-becc-69506f21bc46` | **merged** |
| P0.3 OS notifications | `m0/p03-os-notifications` | `~/repos/ao-lanes/p03` | gpt-5.6-sol / high | `019f5437-1334-7ad1-9d71-4b04bb7b8439` | PARKED (valid BLOCKED) |
| P0.4 docs hygiene | `m0/p04-docs-hygiene` | `~/repos/ao-lanes/p04` | gpt-5.6-sol / high | `019f5434-5f98-7ba2-9bea-3b89c2d30656` | **merged** |

## Events

- Base commit for all lanes: `c49e2bd7` (packets committed on main).
- Lane bootstrap finding: `make go-build` fails in a fresh worktree because
  `main.go` embeds `all:frontend/dist` (gitignored build output) — every Go
  lane needs `pnpm install && pnpm run build` in `frontend/` before the Go
  gates run. Done for p01/p02/p03 before dispatch; baselines green.
- p03 pre-dispatch baseline included a full `make e2e` pass in the lane
  (harness + Playwright work under worktree isolation).
- All four dispatched with `--dangerously-bypass-approvals-and-sandbox`,
  banner-verified sol/high. Logs: session scratchpad `p0N-codex.log`.
- p04 log shows a startup `rmcp` MCP worker error (`AuthorizationRequired`) —
  an unauthenticated MCP server in the user's codex config; unrelated to the
  packet, run proceeds.
- **P0.1 BLOCKED at baseline (valid).** `make test-race` timed out (10m,
  root package) on the UNTOUCHED base tree while three other lanes saturated
  the machine; the named test had 0s elapsed at panic (predecessors ate the
  budget). Claude repro running on a quieter machine to split
  pre-existing-regression vs load-flake before resume/fix.
- **P0.3 PARKED on a valid BLOCKED with partial work** (WIP-committed on the
  lane branch). Blocker 1: the real Windows path splits UI (native launcher)
  from backend (`-tags nogui` in WSL) — no notification bridge exists between
  them; needs a scope/design decision (user earlier deferred remote/notif
  delivery fixes). Blocker 2 VERIFIED by Claude in the pinned fork
  (`pkg/services/notifications/notifications_linux.go:658`): dbus close
  reason 2 (dismiss) is surfaced as `DefaultActionIdentifier` — dismiss and
  click are indistinguishable, so activation nav would misfire; fix belongs
  in the user-owned wails fork. Also flagged: sync macOS auth can block
  startup up to 180s (must go async); cold-start activations need a bounded
  pending queue until frontend hydration. Its `go.mod` change is only the
  indirect `go-toast` dep pulled by the mandated wails package — not a
  violation. Rev2 packet to be authored (target: M3 boundary) with the
  bridge decision, fork patch, async auth, activation queue.
- **P0.2 flagged an internal packet contradiction** (standing rules ban all
  git ops; a gate asked for `git diff --stat` output) — resolved
  conservatively, no git run. Future packets: standing rules should permit
  read-only git inspection explicitly.
- **P0.2 reviewed + merged.** Migration v21 + deterministic backfill
  (ordered `created_at,id`, in-memory collision set, unique index created
  post-dedupe in the same tx), single-sourced slugifier at the CreateProject
  chokepoint (caller-supplied slug ignored; single-connection SQLite makes
  check-then-insert race-free), `EnsureForWorkspace` reloads the persisted
  row (flagged in manifest), bindings regen slug-only, schema.md pointer +
  slug docs. It experimented with a bound-method change and RESTORED it for
  scope compliance (flagged). Claude re-ran go-test + frontend check/build
  independently: green. One rider fix at merge: reverted a stray unicode
  quote artifact in an existing v20 test comment. Known gap (deliberate,
  assumption-flagged): bound `App.CreateProject` returns the pre-insert
  struct with `Slug == ""`; every store read returns the real slug. M1
  consumers read via store; consider returning the persisted row when a
  caller actually needs it.
- **P0.4 reviewed + merged.** Scope exact (2 permitted files), 5 benign
  assumptions, ran full repo gates unprompted (honest output pasted).
  Claude audit: 7 verification-map anchors spot-checked against code
  (threadmode immutability, /design/ loopback guard, 2 MCP tools +
  8-tile cap + clip note, watcher .tmp suppression + polling fallback,
  prompt override path, .picked/LatestUnpickedOptionSet, workdirs under
  dbDir) — all accurate. Report preserved at `reports/P0.4-report.md`.
