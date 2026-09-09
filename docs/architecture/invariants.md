# System Invariants

This page maps cross-area changes to the document that owns their contracts.
Read the linked document before changing the named boundary. Package guides add
only the local constraints needed to work in that area.

## Conversation history

Conversation history has stable, thread-local identity and ordering:

- An item keeps the same `id` from its first streaming update through
  completion.
- `item_index` is assigned by the store and is immutable after insertion.
- New `turn_index` values are allocated under the thread action lock. Background
  child work remains attached to the turn that launched it.
- `parent_id` names a `tool_call`. A `tool_completion` names one background
  `tool_call` through `completion_of`, with at most one completion per launch.

The schema, constraints, and indexes are documented in
[`sqlite-store.md`](sqlite-store.md). Message placement and the distinction
between display position and provider consumption are documented in
[`user-message-ordering.md`](user-message-ordering.md). Provider-specific
history rules used for resume, fork, and import live under
[`../references/`](../references/).

## Provider events and lifecycle

Provider adapters translate native protocols into `provider.ProviderEvent`.
They do not write conversation history or emit frontend events directly.
Triage owns normalized event handling, persistence, and typed event emission;
provider-specific protocol types stay in their adapters.

Every declared `EventKind` has an explicit triage disposition, and
`Router.Handle` fails an unknown kind. Timeline rows pass through triage's
persistence path so a successful write and the corresponding frontend update
stay ordered. See [`triage-routing.md`](triage-routing.md) for the event map and
extension steps.

Tool, background-task, and turn completion are independent lifecycles. A signal
for one must not settle another unless the lifecycle contract explicitly says
so. Turn allocation, pending-send confirmation, stream settlement, subagent
transcript roots, and queued-send promotion must preserve their documented
ordering and idempotence. See [`turn-lifecycle.md`](turn-lifecycle.md) before
changing parser or triage completion behavior.

## Thread execution and session ownership

The provider process and its session files are the source of truth while a turn
is running. One thread action runs at a time. Session start and teardown
coordinate with that action lock and provider read-loop shutdown so stale
events cannot enter a replacement session.

Fork, resume, queued-send, stop, and recovery behavior is specified in
[`turn-lifecycle.md`](turn-lifecycle.md),
[`user-message-ordering.md`](user-message-ordering.md), and the provider wire
references. Confirm uncertain provider behavior with the isolated spike process
in [`../references/spike-policy.md`](../references/spike-policy.md).

## Storage

Provider conversation history in SQLite is a cache of provider-owned history.
Identity and access state, accepted queued messages, transfer ownership, remote
job acceptance, and remote-watch notification ownership are authoritative app
state. Schema behavior comes from
`internal/store/schema_v1.go` and the ordered migration chain in
`internal/store/migrate.go`. Existing migrations are immutable; schema changes
append a new migration. Connection pragmas are part of the DSN and their
required values are verified at startup.

Derived state owned by triggers must not also be maintained in Go. Payload
ownership, history revision stamps, imported history, background settlement,
restore boundaries, and workflow constraints are documented in
[`sqlite-store.md`](sqlite-store.md),
[`thread-replica-sync.md`](thread-replica-sync.md), and the
[`internal/store` guide](../../internal/store/AGENTS.md).

A client replica reads its revision stamps and rows in one transaction.
Whole-store restore copies identity and queued-message state from the snapshot
while preserving the live transfer, remote-job, and remote-watch coordination
records named in the store documentation.

## Transport and authorization

The embedded webview, browser client, and attached backends share the transport
described in [`transport.md`](transport.md). App methods require a declared
scope and route. Event channels require a registry policy for audience,
retention, and entity filtering. Global UI state cannot be inferred from an
entity-filtered event stream.

Scoped CLI tokens are bound to the provider session that owns them. Their
callable method set and workflow grants are closed lists enforced by transport,
with narrower row-level checks in the bound methods. See the transport area
[`guide`](../../internal/transport/AGENTS.md) and
[`workflows-system.md`](../specs/workflows-system.md).

## Workflows

The workflow engine's command loop owns all in-memory scheduler and state
machine mutation. Exit paths converge on the engine teardown path so resource
release and runner stop behavior stay consistent. A parked agent attempt keeps
its provider context when available; context loss is surfaced when the attempt
is reconstructed.

A root workflow run may report through one bound conversation thread. Called
runs report through their root, and wake delivery uses the ordinary queued user
message path. Do not cancel workflow runs while holding a thread action lock.
The complete contracts live in [`workflows-system.md`](../specs/workflows-system.md)
and the guides for [`workflow/engine`](../../internal/workflow/engine/AGENTS.md)
and [`workflowhost`](../../internal/workflowhost/AGENTS.md).

## Model-authored input

Text produced by a model, provider, repository, or third party must be quoted
and bounded before it is embedded in another model prompt. Use
`internal/untrustedtext`; do not create a local escaping convention.

## Related guidance

- [`how-to.md`](how-to.md) routes common changes to their implementation guides.
- [`conventions.md`](conventions.md) defines project-wide engineering
  conventions.
- [`documentation.md`](documentation.md) defines where contracts and guidance
  belong and how they are maintained.
- [`adrs/`](adrs/) records architectural decisions whose rationale remains
  relevant.
