# Docs

Index of the architecture, reference, and archive docs. Area-specific
rules live in `AGENTS.md` next to the code; this tree is for
cross-cutting design.

## Start Here

- **New to the codebase?** Read the root
  [`AGENTS.md`](../AGENTS.md) first — it defines the stack, the core
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

Deep-dive design docs under [`architecture/`](architecture/).

| File | 1-line summary |
|---|---|
| [`conventions.md`](architecture/conventions.md) | Contributor guardrails: file sizes, naming, error handling, tests, SQL patterns, memory hygiene, Svelte rules. |
| [`invariants.md`](architecture/invariants.md) | Load-bearing rules with rationale and enforcement. Read before touching triage, store, or the item model. |
| [`how-to.md`](architecture/how-to.md) | Extension playbooks: new event kind, new item kind, new tool renderer, new migration, new provider, new approval, file splits. |
| [`adrs/`](architecture/adrs/) | Architecture Decision Records. One file per load-bearing choice. |
| [`chat-rewrite.md`](architecture/chat-rewrite.md) | The original unified-item-stream spec. Historical record; invariants and ADRs are the living summary. |
| [`turn-lifecycle.md`](architecture/turn-lifecycle.md) | The three-lifecycle mental model (tool / task / turn). Read before touching provider, triage, or any turn-state UI. |
| [`data-flow.md`](architecture/data-flow.md) | How provider output becomes visible state. Pipeline diagram. |
| [`triage-routing.md`](architecture/triage-routing.md) | Routing table: every `EventKind` → handler → destination. |
| [`schema.md`](architecture/schema.md) | SQLite schema summary. Tables, indexes, migration policy. |
| [`providers.md`](architecture/providers.md) | Provider process model, session identity, approval round-trip. |
| [`recovery.md`](architecture/recovery.md) | Session recovery on restart, thread switch, disconnect. |
| [`revert-modes.md`](architecture/revert-modes.md) | Message anchors, fork-from-message, and Stop/Esc conversation rollback. |
| [`discussion-deliberation.md`](architecture/discussion-deliberation.md) | Multi-agent discussion coordination FSM. |
| [`observability.md`](architecture/observability.md) | OpenTelemetry + per-thread NDJSON event log. |
| [`agent-harness.md`](architecture/agent-harness.md) | Isolated real-app harness, CLI driver, evidence, and platform shells. |
| [`functional-flows.md`](architecture/functional-flows.md) | JSON functional-flow format and standalone Playwright runner. |
| [`workflow-campaigns.md`](architecture/workflow-campaigns.md) | Authoring guide for long multi-wave campaigns on the workflows system: the wave shape, the review/verification patterns and why, automation wiring, and the operating knobs. |
| [`root-decomposition.md`](architecture/root-decomposition.md) | Measured field-ownership and seam map of the `*App` root receiver, the staged plan for cutting it, and the wire-compat facts that make a split byte-identical on the wire. |

## References

External repos we track and how to use them. Under
[`references/`](references/).

| File | 1-line summary |
|---|---|
| [`claude-wire.md`](references/claude-wire.md) | Canonical Claude Code NDJSON shapes + pinned citations into the Python SDK and forge. Single source of truth for parser work. |
| [`codex-wire.md`](references/codex-wire.md) | Canonical Codex JSON-RPC shapes + collab-agent lifecycle. Single source of truth for Codex parser work. |
| [`forge.md`](references/forge.md) | Pointers into the forge codebase we're rewriting. UX and provider-handling reference. |
| [`codex.md`](references/codex.md) | Codex source + CodexMonitor — how to use them when touching Codex code. |
| [`spike-policy.md`](references/spike-policy.md) | When to write an isolated spike test outside the project. |
| [`ao-harness.md`](reference/ao-harness.md) | Generated command and output reference for the `ao-harness` shell driver. |

## Archive

Frozen documents from earlier phases. Not authoritative — do not
update. See [`archive/README.md`](archive/README.md) for what's in
there and why.

## Area Guides

Every Go package and the frontend area ship their own `AGENTS.md`
(with a `CLAUDE.md` symlink):

- [`/AGENTS.md`](../AGENTS.md) — root: stack, principles, repo map.
- [`/internal/AGENTS.md`](../internal/AGENTS.md) — Go package map.
- [`/internal/store/AGENTS.md`](../internal/store/AGENTS.md)
- [`/internal/triage/AGENTS.md`](../internal/triage/AGENTS.md)
- [`/internal/provider/AGENTS.md`](../internal/provider/AGENTS.md)
- [`/internal/provider/claude/AGENTS.md`](../internal/provider/claude/AGENTS.md)
- [`/internal/provider/codex/AGENTS.md`](../internal/provider/codex/AGENTS.md)
- [`/frontend/AGENTS.md`](../frontend/AGENTS.md)

Start at the area closest to what you're touching — it will link
down if more depth is needed.
