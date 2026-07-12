# M0 Delegation Ledger

Campaign: workflows-system M0 (four parallel lanes off one base commit).
Lanes live under `~/repos/ao-lanes/`; logs under the orchestrating session's
scratchpad. Review protocol: WIP-commit the lane immediately on return, then
scope/assumptions/gaming audits + independent gate re-runs before merge.

| Packet | Branch | Lane | Model / effort | Session id | Status |
|---|---|---|---|---|---|
| P0.1 turn-observer registry | `m0/p01-turn-observers` | `~/repos/ao-lanes/p01` | gpt-5.6-sol / high | `019f5435-f377-7cf3-a60f-54a6ca10a902` | dispatched |
| P0.2 project slugs + config dirs | `m0/p02-project-slugs` | `~/repos/ao-lanes/p02` | gpt-5.6-sol / high | `019f5436-71b2-7c21-becc-69506f21bc46` | dispatched |
| P0.3 OS notifications | `m0/p03-os-notifications` | `~/repos/ao-lanes/p03` | gpt-5.6-sol / high | `019f5437-1334-7ad1-9d71-4b04bb7b8439` | dispatched |
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
- **P0.4 reviewed + merged.** Scope exact (2 permitted files), 5 benign
  assumptions, ran full repo gates unprompted (honest output pasted).
  Claude audit: 7 verification-map anchors spot-checked against code
  (threadmode immutability, /design/ loopback guard, 2 MCP tools +
  8-tile cap + clip note, watcher .tmp suppression + polling fallback,
  prompt override path, .picked/LatestUnpickedOptionSet, workdirs under
  dbDir) — all accurate. Report preserved at `reports/P0.4-report.md`.
