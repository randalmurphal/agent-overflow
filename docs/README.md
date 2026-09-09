# Documentation

Use the task index below to find the relevant architecture, specification or
reference. Read the closest area guide for local instructions; follow its links
when the named task applies.

## Start here

- Project layout and essential engineering rules: [root guide](../AGENTS.md).
- Setup, builds, bootstrap and packaging: [Development](architecture/development.md).
- Code organization, asynchronous tests or persistence conventions:
  [Engineering conventions](architecture/conventions.md).
- A change spanning several areas: [Invariants](architecture/invariants.md)
  and the relevant recipe in [How-to](architecture/how-to.md).
- Intentional product behavior: the owning spec or [Product decisions](decisions.md).
- Editing guides, docs or comments: [Documentation maintenance](architecture/documentation.md).

## Architecture

How the app works today. Under [`architecture/`](architecture/).

### Core reference

| File | 1-line summary |
|---|---|
| [`conventions.md`](architecture/conventions.md) | Code organization, errors, resource ownership, tests, SQL and performance. |
| [`documentation.md`](architecture/documentation.md) | Placement, usefulness, style and maintenance of guides, docs and code comments. |
| [`development.md`](architecture/development.md) | Setup, generated artifacts, bootstrap, packaging and validation environments. |
| [`invariants.md`](architecture/invariants.md) | Routes cross-area changes to their current contracts. |
| [`how-to.md`](architecture/how-to.md) | Routes common cross-area changes to their implementation guides. |
| [`adrs/`](architecture/adrs/) | Architecture Decision Records. One file per load-bearing choice. |
| [`refactoring-principles.md`](architecture/refactoring-principles.md) | The five rules a behavior-preserving refactor follows. |
| [`data-flow.md`](architecture/data-flow.md) | How provider output becomes visible state. Pipeline diagram. |
| [`schema.md`](architecture/schema.md) | SQLite table families, constraints and indexes. |
| [`sqlite-store.md`](architecture/sqlite-store.md) | Connections, migrations, snapshot restore, history revisions and query contracts. |
| [`triage-routing.md`](architecture/triage-routing.md) | Routing table: every `EventKind` → handler → destination. |
| [`turn-lifecycle.md`](architecture/turn-lifecycle.md) | The three-lifecycle mental model (tool / task / turn). Read before touching provider, triage, or any turn-state UI. |
| [`user-message-ordering.md`](architecture/user-message-ordering.md) | Send identity, backend admission, frozen confirmation placement, and recovery without moving prompts below their responses. |
| [`root-decomposition.md`](architecture/root-decomposition.md) | Current application composition, ownership and Wails wire compatibility. |
| [`observability.md`](architecture/observability.md) | OpenTelemetry + per-thread NDJSON event log. |
| [`transport.md`](architecture/transport.md) | Wire mechanism deep-dives: port pinning, the gap marker, scoped-token routes, coalescing, keepalive. |
| [`release-candidates.md`](architecture/release-candidates.md) | Build and test an untagged production candidate, then publish its exact saved bytes. |
| [`remote-access-setup.md`](architecture/remote-access-setup.md) | Mac-to-Android setup: Tailscale, APK installation, pairing, release signing, and troubleshooting. |
| [`remote-commands.md`](architecture/remote-commands.md) | Explicit peer access, durable remote commands, connection recovery, and ownership. |
| [`file-previews.md`](architecture/file-previews.md) | Generated HTML on its owning computer: relative assets, independent origins, authorization, browser trust and lifetime. |
| [`session-renewal.md`](architecture/session-renewal.md) | Recoverable renewal, saved successors, legacy compatibility and lost-response validation. |
| [`computer-pairing.md`](architecture/computer-pairing.md) | Desktop LAN/tailnet discovery, address pairing, comparison trust and Windows forwarding. |
| [`computer-routes.md`](architecture/computer-routes.md) | Contract for verified LAN/tailnet routes, failure recovery and platform boundaries; implementation in progress. |
| [`serve-mode.md`](architecture/serve-mode.md) | Operating `agent-overflow serve`: the windowless boot, first-device enrollment from the console, credential storage, bind and port configuration, and installing it as a service. |

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

Product requirements and designs live under [`specs/`](specs/). Keep a spec
while it supplies current requirements that code or architecture docs do not.
Move implemented mechanism details to their architecture owner and remove
obsolete planning material. Update incoming links when moving or retiring a doc.

| File | 1-line summary |
|---|---|
| [`workflows-system.md`](specs/workflows-system.md) | The canonical WHAT spec for the workflows system, revision 2. |
| [`workflows-system-decisions.md`](specs/workflows-system-decisions.md) | The binding decisions log companion to that spec, D1 onward. |
| [`agent-visibility.md`](specs/agent-visibility.md) | How subagent work surfaces in the timeline. Partially implemented; unchecked criteria are the open work. |
| [`code-review.md`](specs/code-review.md) | The review workflow design, signed off 2026-08-23. Not implemented yet. |
| [`agent-thread-tools.md`](specs/agent-thread-tools.md) | The `ao-thread-tools` MCP server (search, ask, spawn, reply) and `/side-chat`. Signed off 2026-09-04; not implemented. |
| [`file-attachments.md`](specs/file-attachments.md) | Any-file composer attachments: copy to the attachments root, path line in the prompt, `--add-dir` for Claude. Signed off 2026-09-02; implementation in progress. |
| [`sidebar-thread-groups.md`](specs/sidebar-thread-groups.md) | Named, collapsible, pinnable groups of threads inside a project's sidebar list. Signed off and implemented 2026-09-02. |
| [`remote-access.md`](specs/remote-access.md) | Remote transport, pairing, Android, previews, and supervised updates; historical implementation notes and outstanding requirements. |
| [`connected-computers.md`](specs/connected-computers.md) | Approved multi-computer product contract, ownership, portability, and acceptance matrix; implementation in progress. |
| [`conversation-transfer.md`](specs/conversation-transfer.md) | Move/copy ownership protocol, native portability evidence, archive boundaries, and required failure tests; implementation in progress. |
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
| [`voice-dictation.md`](references/voice-dictation.md) | Researched voice-to-text options (Claude voice_stream, Codex realtime) and why none is built. |
| [`ao-harness.md`](references/ao-harness.md) | Generated command and output reference for the `ao-harness` shell driver. |
| [`ao-cli.md`](references/ao-cli.md) | The `ao` scoped-token CLI: command tree and `--json` result shapes. |
| [`fixtures/`](references/fixtures/) | Recorded provider wire captures backing `claude-wire.md` and the parser replay tests. |

## Glossary

[`GLOSSARY.md`](GLOSSARY.md) holds coined vocabulary and the terms that
mean different things in different subsystems (wave, lane, spine, ghost,
envelope, lease, ...).

## Area Guides

Area guides exist where local instructions or navigation are useful. Retained
guides have a `CLAUDE.md` symlink; small packages may rely on their package
comments and parent guide.

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
