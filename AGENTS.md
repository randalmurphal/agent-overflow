# Agent Overflow

Desktop app for Claude Code and Codex, built with Go, Wails, SQLite,
Svelte 5 and TypeScript. Prioritize correctness, responsive rendering,
bounded memory and simple code that is easy to maintain.

## Working rules

- Completed chat history is immutable. A Codex spawn row records the spawn
  event plus the child's identity (nickname, role, effective model, effort)
  when Codex reports it later, and opens the agent pane; never use it as the
  agent's mutable runtime record. Later messages, signals, executions and
  completions get separate events at their own timeline positions. See the
  [agent history contract](docs/specs/agent-visibility.md#immutable-agent-history).
- Fix the cause in the code that owns it. Check sibling paths and callers
  when changing a shared contract. Validate inside the API so correctness
  does not depend on every caller remembering a precondition.
- Keep changes cohesive. Improve adjacent code when it makes the solution
  simpler; discuss larger refactors first. Do not add speculative modes,
  compatibility layers or duplicated implementations.
- Preserve visible behavior unless the requested change includes it.
  Ask before accepting a workaround or a product tradeoff the user has not
  approved. A question from the user is not approval to edit.
- Treat rendering work, allocations, I/O and resource lifetimes as design
  constraints. Test repeated calls, cancellation, partial failure and state
  transitions, including whether disabling a feature clears prior state.
- Handle every error explicitly. Return useful errors to the owning caller;
  failures that affect a user operation must reach user-facing state.
  Cleanup failures need an observable outcome. Do not silently discard them.
- Verify claims against current code and tests. Existing patterns and green
  checks are evidence, not proof that a design is correct. Do not bypass a
  failing check or document a defect to avoid fixing it.
- Test the behavior a change could break. Recheck fixes made during review
  with the same care as the original change. Report what was verified and
  any remaining limits.

## Architecture constraints

- The live provider owns turn execution. Provider-native session files own
  recoverable conversation history. Keep provider-specific behavior in its
  provider package; do not force Claude and Codex into one implementation.
- Go connects providers, persistence and clients. Keep workflow sequencing
  in `internal/workflow/` and conversation transfers in
  `internal/threadtransfer/`; do not introduce another orchestration layer
  or a second authoritative model of persisted application state.
- Provider conversation history in SQLite is an item-granular cache;
  streaming content can be persisted before completion. Account records, accepted message queues and coordination records
  have independent durability requirements. Read the store guide before
  changing restore, pruning or migration behavior.
- Load heavy content on demand and bound retained frontend state by the
  active surfaces. Preserve history the user explicitly loaded.
- A project is a repository; a workspace is its root or a linked worktree.
  Keep their identities distinct.
- macOS, Linux, Windows through WSL, embedded webviews and connected browsers
  are supported production paths. Use platform-aware path and process APIs;
  check the applicable platform implementations when changing shared code.
- Calls and events use the shared HTTP/WebSocket transport. Go emits through
  `a.emit`; frontend code uses the typed wrappers in
  `frontend/src/lib/stores/bindings.ts`. Binding generation and RPC metadata
  belong to the app and transport guides.

## Start with the relevant area

| Task | Entry point |
|---|---|
| Go packages and ownership | [internal/AGENTS.md](internal/AGENTS.md) |
| Application methods and lifecycle | [internal/app/AGENTS.md](internal/app/AGENTS.md) |
| UI and client state | [frontend/AGENTS.md](frontend/AGENTS.md) |
| Harness and browser tests | [e2e/AGENTS.md](e2e/AGENTS.md) |
| Android shell | [mobile/AGENTS.md](mobile/AGENTS.md) |
| Windows launcher | [cmd/agent-overflow-windows/AGENTS.md](cmd/agent-overflow-windows/AGENTS.md) |
| Build, bootstrap, packaging or dev watchers | [Development](docs/architecture/development.md) |
| Cross-area design or product constraints | [Documentation index](docs/README.md) |

Read linked material when its stated task applies. Follow child guides for
local constraints; parent instructions remain in effect. Check the relevant
[product decisions](docs/decisions.md) before changing intentional behavior.
For uncertain external-tool behavior, use the provider references and
[isolated spike policy](docs/references/spike-policy.md).

## Validation

Read-only investigations do not require build, type-check or test runs. Run
focused checks only when needed to answer the question or requested by the user.

Choose validation from the change's impact. Run all potentially relevant tests
and applicable build and type checks, including affected callers, shared
contracts, integration paths and platform behavior. Read the affected area
guides for additional requirements. Do not run unrelated suites solely because
code changed; run broader checks when the impact crosses areas or is uncertain.

For Go changes, use the Make targets so platform build settings are applied:
`make go-build` and `make go-test`. For frontend changes, run
`cd frontend && pnpm run check`, `cd frontend && pnpm run build`, and the
relevant Vitest tests. Changes to shared bindings, transport or build settings
may require checks on both sides. Documentation-only changes need checks of
the affected claims, links and references, not application builds or tests.

`make help` lists supported commands; [Development](docs/architecture/development.md)
routes manual and release checks. Report what ran and any relevant gaps.

Tests use temporary homes and mock providers. They must never invoke a real
provider or touch the developer's provider homes. Session-capable fixtures
use [kerneltest](internal/kerneltest/AGENTS.md). Real provider execution is
restricted to the explicitly requested manual provider smoke.

## Keep context useful

Guides contain only navigation and essential instructions for good changes
in their scope. Keep each rule in one authoritative place. Update or remove
existing guidance when code changes; a bug fix does not require new prose.
Put mechanism details in focused docs and code-local contracts beside the
code. Delete narration that a reader can infer from the implementation.

Use concise, direct engineering language in guides, docs and comments. No
em dashes, dramatic metaphors, incident stories, work logs or review history.
Preserve exact API names and technical terms. When editing documentation,
follow [Documentation maintenance](docs/architecture/documentation.md).
