# lib/stores/

Every wire subscription and every entity-owned RPC lives here, and
components read the result through `$derived`. Two of
`src/lib/architecture.test.ts`'s five rules police that boundary, with
shrink-only allowlists: a fixed exception must be deleted from the list.

## Which primitive

The deciding question is "is there something to release?".

- `entityStore.svelte.ts` for a key backed by a BACKEND RESOURCE that has
  to be acquired and re-acquired across a reconnect. It owns the whole
  lifecycle: the first `attach` sources the key, every later attacher
  shares it, the last release tears it down, and the transport edge
  (suspend on disconnect, re-acquire on reconnect) is wired once for every
  entity rather than per store. `apply` is the single write chokepoint, so
  an `onApply` reconciliation hook cannot be bypassed by a new call site.
- `keyedSignalRegistry.svelte.ts` for PUSH-FED state: events arrive and are
  written, nothing to acquire, nothing to tear down. One `$state.raw` box
  per key, so a reader re-evaluates only when ITS key changes. `set` is the
  only box creator, because Svelte does not track state created inside the
  running reaction. Building this on the entity primitive buys a refcount,
  a source, a retry curve and a transport edge that all have to be no-oped.

Entity values are deep `$state` by default. A store that REPLACES values
wholesale sets `rawValue: true` and gets one signal per entry, which
matters because Svelte's proxy walk over a whole run tree runs on the main
thread. It stays opt-in because the safety condition belongs to the
store's writers: turning it on where values are mutated in place silently
stops waking readers.

## Rules

- One thread is mounted in at most one pane. `mountThreadInPane` probes for
  an existing mount and is the only door into `replaceThreadInPane`, which
  is private for that reason. The duplicate scan in `panes.svelte.ts` is a
  deliberately non-dev-gated tripwire for a path that mounts around it.
  Two panes on one WORKSPACE stay first-class.
- `events.ts` is the single subscription root. It owns channel names,
  generics and teardown order, and fans each channel out to the
  `events*.ts` module that owns the reaction. Add a channel there, put the
  reaction in a domain module, and never subscribe from a component.
- `bindings.ts` re-exports what `wails3 generate bindings -ts` produced.
  Add the new App method by regenerating and re-exporting. Never hand-wrap
  a binding, and never reach for `window.runtime`.
- A new entity store registers its RPCs in the architecture test's
  registry, and may import the RPCs it owns and no others.
- `providerAccounts.svelte.ts` is the one account load, login, switch,
  refresh and remove path, for the picker and Settings alike.
- `thread.svelte.ts` (`ThreadPane`) is the sole owner of per-thread runtime
  UI state. Add to it rather than beside it.

State ownership taxonomy and the entity-keying doctrine:
[`frontend/AGENTS.md`](../../../AGENTS.md).
