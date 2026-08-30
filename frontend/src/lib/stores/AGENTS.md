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
- An authoritative install that evicts absent rows
  (`installTimelineItems({disposeDropped: true})`) must first fold in the
  rows SQLite structurally cannot hold: pending sends persist only on wire
  echo, streaming rows persist on completion, so a fresh slice never
  contains either. Both authoritative install paths in
  `threadSwitchLoad` (`runBackendRefresh` and the cold-open sync leg)
  follow the pattern: merge `GetThreadLiveState.deferredItems` into the
  page, retain current `streaming`/`running` rows, and commit the
  install and the live-state apply in one synchronous step so no
  slice-only frame ever paints (incident 2026-08-29: gap-refresh cycles
  made a queued message flicker in and out of the timeline). Merged
  deferred rows also join the pane's optimistic-id ledger
  (`trackDeferredBets`): the stamped tiers strip optimistic rows because
  a bet can be dropped without a rev bump, and an untracked merged row
  would persist into the replica as a phantom.
- Row text never rewinds behind an active reveal cursor. See
  "The reveal invariant" below.
- An event-driven authoritative refresh converges, it never supersedes.
  Cancelling the in-flight refresh on each new trigger (`++generation`
  guards at every await) livelocks under an event storm, because triggers
  outpace the RPC round-trip and no install ever lands. Use
  `utils/refreshScheduler.ts` (architecture rule 5); generation guards are
  for user-input-driven flows where the newest intent wins.

## The reveal invariant

**While a smoother owns an assistant row, the row's published text IS
that smoother's reveal cursor.** Reconciliation may leave it there, hand
the smoother a longer suffix to drain, or hand ownership over with a
summary that WINS the row — snapping forward. It may never publish text
that rewinds behind the cursor.

Five separate bugs in the 2026-08-28/29 perf session were this one rule,
broken five different ways. It is one rule because there is one
chokepoint: `prepareItemReplacement` in
`threadStreamingReveal.svelte.ts` decides the text of every row a
wholesale commit publishes, and `commitTimelineItems` (fold eviction,
prune, revert, replica paint, cache install) and `upsertItemsBatch` both
go through it.

The shape that keeps recurring is a summary that TRAILS the cursor. A
row can be terminal while its smoother still drains — the completion
patch flips `status` and skips the summary write, so for seconds the
row's summary is a strict prefix of the smoother's `received` and
NOTHING later rewrites it; the drain is the only path to the full text.
SQLite and replica snapshots produce the same shape by lagging the
wire-visible delta stream. Either way the trailing summary must not take
the row:

- Mid-drain, disposing the smoother strands the row at the partial text
  forever (incident 2026-08-29: the final assistant answer froze at ~130
  of 1021 chars whenever a subagent child settled inside the drain
  window).
- Post-drain, letting the trailing summary settle the row truncates it
  outright — the same rewind, reached when the drain happened to finish
  first.

Disposing is correct only when the incoming summary genuinely DIVERGES;
then it must win the row, so the visible text snaps rather than
truncates.

**Enforcement.** `assertRevealCursorNotRewound`, called at the
chokepoint under `ASSERT_REVEAL_INVARIANT` (dev and test only; both
operands fold to literals so the guard and its `getRevealed()`
materialization leave the production bundle). Tests:
`threadStreamingRevealInvariant.test.ts` for the rule, the tripwire and
both rewind shapes; `threadStreamingReveal.incidentReplay.test.ts` for
the byte-faithful wire replay.

Nothing here may be "fixed" by skipping, rushing or popping the drain —
that is the reveal-queue doctrine, and the header comment on
`recomputeReveal` records why each attempt was rejected.

State ownership taxonomy and the entity-keying doctrine:
[`frontend/AGENTS.md`](../../../AGENTS.md).
