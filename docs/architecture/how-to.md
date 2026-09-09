# Change Guide

Use this page to find the authoritative guide for a common cross-area change.
Read the closest `AGENTS.md` before editing an area. Keep protocol and lifecycle
details in architecture or reference documents, and keep public API contracts
with the API they describe. See [`documentation.md`](documentation.md) for the
maintenance policy.

## Provider events and timeline items

Before adding or changing a provider event:

1. Read [`triage-routing.md`](triage-routing.md) for the complete `EventKind`
   path and its coverage checks.
2. Read the relevant provider guide and wire reference under
   [`../references/`](../references/).
3. If the event affects a tool, task, or turn boundary, read
   [`turn-lifecycle.md`](turn-lifecycle.md).
4. Follow the event through its typed frontend channel and update the smallest
   useful parser, routing, and consumer tests.

A new timeline item kind also changes the SQLite constraint, deterministic item
identity, triage persistence, and frontend dispatch. Read
[`sqlite-store.md`](sqlite-store.md), [`chat-rewrite.md`](chat-rewrite.md), the
[`internal/store` guide](../../internal/store/AGENTS.md),
[`triage-routing.md`](triage-routing.md), and the
[`chat component` guide](../../frontend/src/lib/components/chat/AGENTS.md) before
choosing the shape.

## Provider adapters

A provider adapter owns native process lifecycle, wire parsing, configuration,
approvals, and translation into shared provider events. Start with
[`providers.md`](providers.md), then read the existing adapter closest to the
new protocol and the spike policy in
[`../references/spike-policy.md`](../references/spike-policy.md). Capture verified
native behavior in a focused reference document rather than in a package guide.

Adding a provider also requires app session registration, capability and model
discovery, frontend selection, isolated test fixtures, and provider smoke
coverage. Treat it as a cross-area design change rather than copying one
adapter's file layout.

## Database schema

Read the [`internal/store` guide](../../internal/store/AGENTS.md) and
[`sqlite-store.md`](sqlite-store.md) before changing SQLite. Append the next
migration in `internal/store/migrate.go`; never edit a migration that may already
have run. Add a migration test that begins from the preceding schema state and
proves preserved data plus the new constraint or index. Update `sqlite-store.md`
when the durable shape or semantics change.

## Transport methods and event channels

Read the transport area guide and [`transport.md`](transport.md) before adding
an App-bound method or event channel. Bound methods need the correct scope and
route declaration. Channels need one constant, a registry policy, delivery and
replay behavior, and consumer cleanup. Entity filtering is an authorization and
delivery decision, so audit every consumer before selecting it.

## Frontend renderers and approval surfaces

For a tool renderer, begin in the chat component guide and reuse the shared tool
classification and icon paths. Add a dedicated component only when it owns a
distinct interaction or payload shape.

For an approval kind, trace both directions: provider request normalization,
frontend rendering and decision state, and provider response encoding. Read the
provider adapter guide and the guide nearest the approval component. Test the
request and response together so an accepted UI choice cannot encode an invalid
provider reply.

## Splitting code

Split around responsibility and state ownership. Keep the public entry point
easy to find, give sibling files names that describe their concern, and move
tests only when that improves navigation. A split does not require an exhaustive
file inventory in an area guide. Update documentation only when ownership or a
documented contract changed.

## Area guides

Create an `AGENTS.md` only when an area has critical local constraints or needs
task-specific navigation that its parent guide cannot provide concisely. Add the
matching `CLAUDE.md` symlink. Do not repeat parent rules, source layout visible
from filenames, implementation history, or general engineering advice.

For guide placement, links, and maintenance, follow
[`documentation.md`](documentation.md).

## Development environments

Use [`development.md`](development.md) for build and development command routing.
For Windows and WSL launcher behavior, read the guides in
[`cmd/agent-overflow-windows`](../../cmd/agent-overflow-windows/AGENTS.md) and
[`internal/wsllauncher`](../../internal/wsllauncher/AGENTS.md). Use the Make
targets so repository platform flags and generated assets match CI.

## Related guidance

- [`invariants.md`](invariants.md) maps cross-area contracts to their owners.
- [`conventions.md`](conventions.md) contains project-wide code conventions.
- [`docs/README.md`](../README.md) indexes architecture, specifications, and
  operational guides.
