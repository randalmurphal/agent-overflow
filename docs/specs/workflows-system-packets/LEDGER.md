# M0 Delegation Ledger

Campaign: workflows-system M0 (four parallel lanes off one base commit).
Lanes live under `~/repos/ao-lanes/`; logs under the orchestrating session's
scratchpad. Review protocol: WIP-commit the lane immediately on return, then
scope/assumptions/gaming audits + independent gate re-runs before merge.

| Packet | Branch | Lane | Model / effort | Session id | Status |
|---|---|---|---|---|---|
| P0.1 turn-observer registry | `m0/p01-turn-observers` | `~/repos/ao-lanes/p01` | gpt-5.6-sol / high | run1 `019f5435-f377…` (BLOCKED, valid); run2 `019f545b-1d9d-7f41-be57-997dd740ddfe` | redispatched on `bc1d28b9` |
| P0.2 project slugs + config dirs | `m0/p02-project-slugs` | `~/repos/ao-lanes/p02` | gpt-5.6-sol / high | `019f5436-71b2-7c21-becc-69506f21bc46` | **merged** |
| P0.3 OS notifications | `m0/p03-os-notifications` | `~/repos/ao-lanes/p03` | gpt-5.6-sol / high | `019f5437-1334-7ad1-9d71-4b04bb7b8439` | PARKED (valid BLOCKED) |
| P0.4 docs hygiene | `m0/p04-docs-hygiene` | `~/repos/ao-lanes/p04` | gpt-5.6-sol / high | `019f5434-5f98-7ba2-9bea-3b89c2d30656` | **merged** |
| P1.1 `internal/workflow/def` | `m1/p11-workflow-def` | `~/repos/ao-lanes/p11` | gpt-5.6-sol / high | `019f545e-ab3a-78f2-a7bc-66e4f65a8164` | **merged** |
| P1.2 `internal/workflow/profile` | `m1/p12-profile` | `~/repos/ao-lanes/p12` | gpt-5.6-sol / high | `019f5478-b246-7863-b9ac-80977313ffc5` | dispatched (base `c20c5293`) |
| P1.3 `ao` CLI skeleton | `m1/p13-ao-cli` | `~/repos/ao-lanes/p13` | gpt-5.6-sol / high | `019f5478-bdb3-75c0-b1c4-4ee5139ae19a` | dispatched (base `c20c5293`) |

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
- **User rulings on P0.3 (mid-campaign):** (1) if the Windows launcher
  needs the remote/bridge changes, do them — rev2 scope includes the
  launcher↔backend notification bridge rather than deferring it. (2) Wails
  fork changes must be logged as upstream candidates once fully verified —
  user wants to submit fix PRs upstream. **Upstream verification DONE for
  the dismiss-vs-click bug:** identical code in `wailsapp/wails` master at
  `v3/pkg/services/notifications/notifications_linux.go:658` — dbus
  `NotificationClosed` reason 2 (user dismissed, per freedesktop spec)
  synthesizes a `NotificationResponse` with `DefaultActionIdentifier`,
  indistinguishable from a real click (real clicks arrive via
  `ActionInvoked`, line 566/576). Apps navigate on dismiss. macOS
  distinguishes these (`UNNotificationDismissActionIdentifier`), so the
  cross-platform surface is inconsistent too. → UPSTREAM PR CANDIDATE.
  **STANDING CONSTRAINT (user, verbatim intent): may fix the issue and
  create the branch off latest upstream, but do NOT open the upstream PR
  until the user has explicitly approved it.**
- **P0.1 root-cause CONFIRMED (not a load flake):** `make test-race` fails
  identically on a quiet machine — the root package hits the 600s per-binary
  `-timeout` with tests still progressing (same test reached at panic in
  both runs = deterministic order + honest slowness, no hang). The harness
  commit (6c2bfcbb) grew the root package's -race runtime past the budget.
  Fix: measure honest runtime (`go test -race -timeout 1800s .` timing run),
  raise the Makefile `test-race` timeout with margin for parallel-lane load,
  land on main, reset the p01 lane, fresh dispatch (per skill: tree reset
  under a session → fresh, not resume; it had produced only BLOCKED.md).
  Observed debt (not fixed now): root-package -race runtime >10min deserves
  a test-speed pass someday.
- **Gate fix verified on main (`bc1d28b9`):** full `make test-race` green;
  root package 679s under two-codex-lane load (would have failed the old
  600s budget; 1800s has ~2.6x headroom), triage 374s.
- **P1.1 reviewed + merged.** 2795-line def package, scope exact. Claude
  audit highlights: D2a envelope generator byte-deterministic (sorted
  required + Go map-key marshal order); ValidateEnvelope enforces strict
  three-shape mutual exclusion + all-findings-sorted feedback errors + size
  cap with write-to-a-file guidance; "ancestor" formalized as DOMINANCE on
  the loop-free forward graph (statically sound choice — loop targets are
  guaranteed executed on every path; consistent with D2/D5 producer rules);
  unbounded forward cycles rejected (bounded loop routes only); first-match
  route-order dead-route detection; optionality propagates through dotted
  paths via required-field absence; interpolation provably inert
  (ReplaceAllStringFunc, no rescan) with `(not provided)`; prompt files
  template-validated against declared inputs with symlink-confined paths;
  BindingsUnchecked is a distinct visible status. Assumptions all
  reasonable (per-phase inputs redeclare consumer schema; 1MiB/4MiB read
  caps; scheduler blocks live outside the workflow doc per spec §11;
  sub-workflows post-v1). Claude re-ran go-build + focused -race + full
  go-test independently: green.
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
