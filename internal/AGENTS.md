# Go packages

All non-main Go packages live under `internal/`. Root `main*.go` and
`service.go` own executable dispatch, embedded assets and platform bootstrap.

## Find the owner

| Change | Start here |
|---|---|
| Wails methods, dependency wiring, application lifecycle | [app/](app/AGENTS.md) |
| Provider processes and wire parsing | [provider/](provider/AGENTS.md) |
| Live session admission and retention | [sessionruntime/](sessionruntime/AGENTS.md) |
| Provider history import | [sessionimport/](sessionimport/AGENTS.md) |
| Event routing and timeline item lifecycle | [triage/](triage/AGENTS.md) |
| SQLite queries, migrations, restore and pruning | [store/](store/AGENTS.md) |
| HTTP/WebSocket calls, events and connection policy | [transport/](transport/AGENTS.md) |
| Sign-in, pairing and session permissions | [identity/](identity/AGENTS.md) |
| Connected computers and profiles | [deviceclient/](deviceclient/AGENTS.md), [computerroute/](computerroute/AGENTS.md) |
| Conversation move/copy | [threadtransfer/](threadtransfer/AGENTS.md) |
| Workflow definitions and execution | [workflow/def/](workflow/def/AGENTS.md), [workflow/engine/](workflow/engine/AGENTS.md) |
| Application services and their boundaries | [Application composition](../docs/architecture/root-decomposition.md) |
| Repository/worktree lookup and operations | [gitroot/](gitroot/AGENTS.md), [git/](git/AGENTS.md), [gitapp/](gitapp/AGENTS.md) |
| Browser engines and thread browser tools | [browser/](browser/AGENTS.md) |
| Settings ownership and persistence | [settings/](settings/AGENTS.md) |
| Test fixtures and provider isolation | [kerneltest/](kerneltest/AGENTS.md), [testutil/](testutil/AGENTS.md) |
| Backend harness and scenarios | [harness/](harness/AGENTS.md) |

For smaller packages, read the package comment and exported API contracts.
Use `rg --files internal/<area>` to find implementations and tests. This map
routes responsibilities; it is not a duplicate package or symbol catalog.

## Package boundaries

- Keep packages cohesive and dependencies explicit. Compose capabilities in
  the owning application service instead of reaching into another service's
  state. Pass filesystem roots, process runners and event callbacks where
  ownership belongs to the caller.
- `app` owns bound methods and frontend event projection. Root owns the
  named Wails compatibility registration. Frontend DTOs can live in the
  package that owns their meaning; do not mirror them into `main`.
- Provider parsing must not acquire a dependency on store or triage.
  Provider-neutral import events belong in `importir`; persistence belongs
  in `sessionimport`.
- Resolve provider homes through the supplied account/session owner. Do not
  infer a default home in a helper or introduce a global mutable owner.
- Extend an existing owner before creating a package. Add a package comment
  when its purpose or boundary needs explanation. Add an area guide only
  for instructions or navigation that the parent and code do not supply.

## Tests and references

Put unit and protocol integration tests beside their owner. Tests of app
composition belong in `app`; executable bootstrap tests stay at the root.
Store-backed fixtures can use `store/storetest` to clone a migrated fixture. Read [conventions](../docs/architecture/conventions.md#tests) when
adding asynchronous tests or shared fixtures.

For changes spanning providers, triage, persistence and rendering, follow
[Data flow](../docs/architecture/data-flow.md) and the relevant entries in
[Invariants](../docs/architecture/invariants.md). For common extensions,
use the task-specific recipe in [How-to](../docs/architecture/how-to.md).
