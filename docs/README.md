# Docs

Index of the architecture, spec, and reference docs.
Area-specific rules live in `AGENTS.md` next to the code; this tree is
for cross-cutting design.

## Start Here

- **New to the codebase?** Read the root
  [`AGENTS.md`](../AGENTS.md) first. It defines the stack, the core
  principles, and points at the rest.
- **About to make a change?** Read
  [`architecture/conventions.md`](architecture/conventions.md) and
  [`architecture/invariants.md`](architecture/invariants.md) before
  writing code.
- **Doing a common task?** See
  [`architecture/how-to.md`](architecture/how-to.md) for step-by-step
  recipes.
- **Wondering why we chose X?** See
  [`architecture/adrs/`](architecture/adrs/).

## Architecture

How the app works today. Under [`architecture/`](architecture/).

### Core reference

| File | 1-line summary |
|---|---|
| [`conventions.md`](architecture/conventions.md) | Contributor guardrails: file sizes, naming, error handling, tests, SQL patterns, memory hygiene, Svelte rules, and maintaining the guides themselves. |
| [`invariants.md`](architecture/invariants.md) | Load-bearing rules with rationale and enforcement. Read before touching triage, store, or the item model. |
| [`how-to.md`](architecture/how-to.md) | Extension playbooks: new event kind, new item kind, new tool renderer, new migration, new provider, new approval, file splits. |
| [`adrs/`](architecture/adrs/) | Architecture Decision Records. One file per load-bearing choice. |
| [`refactoring-principles.md`](architecture/refactoring-principles.md) | The five rules a behavior-preserving refactor follows. |
| [`data-flow.md`](architecture/data-flow.md) | How provider output becomes visible state. Pipeline diagram. |
| [`schema.md`](architecture/schema.md) | SQLite schema summary. Tables, indexes, triggers, migration policy. |
| [`triage-routing.md`](architecture/triage-routing.md) | Routing table: every `EventKind` → handler → destination. |
| [`turn-lifecycle.md`](architecture/turn-lifecycle.md) | The three-lifecycle mental model (tool / task / turn). Read before touching provider, triage, or any turn-state UI. |
| [`root-decomposition.md`](architecture/root-decomposition.md) | Field-ownership and seam map of the `*App` root receiver, plus the wire-compat facts that make a split byte-identical on the wire. |
| [`observability.md`](architecture/observability.md) | OpenTelemetry + per-thread NDJSON event log. |
| [`transport.md`](architecture/transport.md) | Wire mechanism deep-dives: port pinning, the gap marker, scoped-token routes, coalescing, keepalive. |

### Providers and sessions

| File | 1-line summary |
|---|---|
| [`providers.md`](architecture/providers.md) | Provider process model, session identity, approval round-trip. |
| [`claude-tui-provider.md`](architecture/claude-tui-provider.md) | The third provider: the real Claude Code TUI in a PTY, with the event stream reconstructed from outside the process. |
| [`recovery.md`](architecture/recovery.md) | Session recovery on restart, thread switch, disconnect. |
| [`revert-modes.md`](architecture/revert-modes.md) | Message anchors, fork-from-message, and Stop/Esc conversation rollback. |
| [`discussion-deliberation.md`](architecture/discussion-deliberation.md) | Multi-agent discussion coordination FSM. |
| [`thread-replica-sync.md`](architecture/thread-replica-sync.md) | The `history_rev`/`history_epoch` stamp pair and the IndexedDB thread-window replica that paints before the sync RPC returns. |
| [`browser-tools.md`](architecture/browser-tools.md) | Built-in browser MCP product contract, lifecycle, authority boundaries, and provider live-apply behavior. |
| [`in-app-browser-spike.md`](architecture/in-app-browser-spike.md) | Measured Wails-webview versus shared-Chrome decision evidence behind the built-in browser. |

### Frontend

| File | 1-line summary |
|---|---|
| [`frontend-scroll.md`](architecture/frontend-scroll.md) | The durable scroll contract for chat and discussion panes. Read before touching `ThreadPane`, `MessageTimeline`, or the virtualizer. |
| [`scroll-contracts.md`](architecture/scroll-contracts.md) | C1–C27: the user-observable scroll behaviors, each with regression provenance and the test that pins it. |
| [`activity-runs.md`](architecture/activity-runs.md) | One maximal stretch of activity rows as a single timeline row: the nested scroller, its expansion lease, and collapse. |
| [`theme-system.md`](architecture/theme-system.md) | The token vocabulary, the two independent appearance axes, and the client-side `themes/` directory. |
| [`chat-rewrite.md`](architecture/chat-rewrite.md) | The item-model spec of record: item ID schemas, channels, the background tray. Cited by invariants and the event types. |
| [`settle-flicker-analysis.md`](architecture/settle-flicker-analysis.md) | Root-cause record for the settle-flicker class; the standing-oracle tests cite it. |
| [`scroll-arbitration-plan.md`](architecture/scroll-arbitration-plan.md) | Quiet-work deferral and scroll arbitration design; cited by `timelineQuietWork` and the interleaving tests. |
| [`scroll-rearchitecture-plan.md`](architecture/scroll-rearchitecture-plan.md) | The scroll re-architecture design the resolver implements; companion [`scroll-rearchitecture-inventories.md`](architecture/scroll-rearchitecture-inventories.md). |
| [`virtualizer-replacement-plan.md`](architecture/virtualizer-replacement-plan.md) | Design behind `utils/virtual/`; evidence in [`virtualizer-replacement-inventories.md`](architecture/virtualizer-replacement-inventories.md). |
| [`review-pane-design.md`](architecture/review-pane-design.md) | The review-pane surface `internal/gitdiff` feeds. |

### Workflows

| File | 1-line summary |
|---|---|
| [`workflow-run-map.md`](architecture/workflow-run-map.md) | The run map: waves, nodes, fans, folding, and the follow/scroll contract. Binding on the run-map renderers. |
| [`workflow-campaigns.md`](architecture/workflow-campaigns.md) | Authoring guide for long multi-wave campaigns: the wave shape, review and verification patterns, automation wiring, operating knobs. |

### Testing and diagnostics

| File | 1-line summary |
|---|---|
| [`agent-harness.md`](architecture/agent-harness.md) | Isolated real-app harness, CLI driver, evidence, and platform shells. |
| [`soak-rig.md`](architecture/soak-rig.md) | The hours-long soak preset beside your own app, and what `make soak-check` reads afterwards. |
| [`functional-flows.md`](architecture/functional-flows.md) | JSON functional-flow format and standalone Playwright runner. |

## Specs

Designs still being decided or built. Under [`specs/`](specs/). A spec
that ships and stays load-bearing graduates to `architecture/`; one that
ships and stops being cited gets deleted (git history keeps it).

| File | 1-line summary |
|---|---|
| [`workflows-system.md`](specs/workflows-system.md) | The canonical WHAT spec for the workflows system, revision 2. |
| [`workflows-system-decisions.md`](specs/workflows-system-decisions.md) | The binding decisions log companion to that spec, D1 onward. |
| [`agent-visibility.md`](specs/agent-visibility.md) | How subagent work surfaces in the timeline. Partially implemented; unchecked criteria are the open work. |
| [`code-review.md`](specs/code-review.md) | The review workflow design, signed off 2026-08-23. Not implemented yet. |
| [`agent-thread-tools.md`](specs/agent-thread-tools.md) | Agent-callable thread search, ask, spawn, and `/side-chat`. Design awaiting sign-off; not implemented. |
| [`file-attachments.md`](specs/file-attachments.md) | Any-file composer attachments: copy to the attachments root, path line in the prompt, `--add-dir` for Claude. Signed off 2026-09-02; implementation in progress. |
| [`sidebar-thread-groups.md`](specs/sidebar-thread-groups.md) | Named, collapsible, pinnable groups of threads inside a project's sidebar list. Signed off and implemented 2026-09-02. |
| [`remote-access.md`](specs/remote-access.md) | Remote and multi-device access. Draft; only phase-0 groundwork is built. |
| [`remote-access-boundaries.md`](specs/remote-access-boundaries.md) | The boundaries and guarantees companion to the remote-access spec. |
| [`testing-harness.md`](specs/testing-harness.md) | The harness contract and design rationale. `architecture/agent-harness.md` describes the built surface. |
| [`prompt-tool-overrides.md`](specs/prompt-tool-overrides.md) | Settings-level system-prompt overrides and per-provider tool toggles. |
| [`workflows-system-ui/UI-SPEC.md`](specs/workflows-system-ui/UI-SPEC.md) | The binding workflows-overlay UI spec (rev 2). Cited as `UI-SPEC §N` across the frontend. |
| [`cursor-provider.md`](specs/cursor-provider.md) | Cursor as a third provider over ACP, signed off 2026-08-31. Living gap table + spike backlog; not implemented yet. |

## References

External repos and tools we track, and how to use them. Under
[`references/`](references/).

| File | 1-line summary |
|---|---|
| [`claude.md`](references/claude.md) | The Claude Code source checkout: what it is good for and how it lags the installed binary. |
| [`claude-wire.md`](references/claude-wire.md) | Canonical Claude Code NDJSON shapes + pinned citations into the Python SDK. Single source of truth for parser work. |
| [`codex.md`](references/codex.md) | Codex source + CodexMonitor: how to use them when touching Codex code. |
| [`codex-wire.md`](references/codex-wire.md) | Canonical Codex JSON-RPC shapes + collab-agent lifecycle. Single source of truth for Codex parser work. |
| [`codex-instructions-tools.md`](references/codex-instructions-tools.md) | Codex's own instruction blocks and tool surface, as an appendix to `codex.md`. |
| [`codex-browser-parity.md`](references/codex-browser-parity.md) | Exact map from the bundled Codex browser skill API to AO's built-in browser MCP tools and validation. |
| [`claude-api-error-upstream-report.md`](references/claude-api-error-upstream-report.md) | Draft upstream bug report for a Claude Code API-error shape, still unfiled. |
| [`spike-policy.md`](references/spike-policy.md) | When to write an isolated spike test outside the project. |
| [`ao-harness.md`](references/ao-harness.md) | Generated command and output reference for the `ao-harness` shell driver. |
| [`ao-cli.md`](references/ao-cli.md) | The `ao` scoped-token CLI: command tree and `--json` result shapes. |
| [`fixtures/`](references/fixtures/) | Recorded provider wire captures backing `claude-wire.md` and the parser replay tests. |

## Glossary

[`GLOSSARY.md`](GLOSSARY.md) holds coined vocabulary and the terms that
mean different things in different subsystems (wave, lane, spine, ghost,
envelope, lease, ...).

## Area Guides

Every Go package and the frontend area ship their own `AGENTS.md`
(with a `CLAUDE.md` symlink):

- [`/AGENTS.md`](../AGENTS.md): the root guide (stack, principles, repo map).
- [`/internal/AGENTS.md`](../internal/AGENTS.md): Go package map.
- [`/internal/store/AGENTS.md`](../internal/store/AGENTS.md)
- [`/internal/triage/AGENTS.md`](../internal/triage/AGENTS.md)
- [`/internal/provider/AGENTS.md`](../internal/provider/AGENTS.md)
- [`/internal/provider/claude/AGENTS.md`](../internal/provider/claude/AGENTS.md)
- [`/internal/provider/codex/AGENTS.md`](../internal/provider/codex/AGENTS.md)
- [`/frontend/AGENTS.md`](../frontend/AGENTS.md)

Start at the area closest to what you're touching. It will link
down if more depth is needed.
