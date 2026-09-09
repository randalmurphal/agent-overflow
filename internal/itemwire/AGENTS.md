# `internal/itemwire`

Projects complete persisted timeline items into bounded client copies. Raw
SQLite content remains canonical. Read
[data flow](../../docs/architecture/data-flow.md#wire-projection) for the
storage/projection boundary and
[remote access](../../docs/specs/remote-access.md#wire-budget-enforcement) for
the wire budget.

- Never mutate or persist a projected item. Every elided value gets a typed
  marker and a recovery route through `GetThreadItemProjectionSource`.
- Apply projection to every list, cursor/window, live event, deferred item, and
  nested-item path. The page byte backstop is a final safety bound, not a
  replacement for field-aware elision.
- `inlinePreviews` is a request parameter because clients connected to one
  backend may choose differently. Do not read the backend setting here.
- Preserve `retainedIdentityKeys` regardless of size. Any new uncapped
  frontend metadata reader must update that inventory and
  `metaInputLeafRenderCaps.test.ts`.
- Under-budget rows remain byte-identical. Keep the fast path free of JSON
  decode/re-encode work.
- Leaf elision skips ineligible candidates and continues scanning; one retained
  large value must not prevent other large leaves from being removed.

A new heavy field needs an explicit projection, marker, hydration route, and
coverage across every outbound item path.
