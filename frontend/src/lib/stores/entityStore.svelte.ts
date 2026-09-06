// Refcounted, entity-keyed reactive state.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): frontend state is keyed
// by its ENTITY — app, project, workspace, PR, thread — never by its
// consumer. Two panes looking at one worktree are looking at one worktree;
// holding a private copy per pane is how they came to disagree about
// whether there was anything to commit.
//
// A store built here owns the whole lifecycle for a key: the first attacher
// acquires the backend resource, every later attacher shares it, and the
// last release tears it down. `apply` is the single write chokepoint — event
// push, RPC result, post-action refresh all land there, so a reconciliation
// hook (`onApply`) cannot be bypassed by adding a new call site. The
// transport edge (suspend on disconnect, re-acquire on reconnect) is wired
// here too: it is the same answer for every entity, and a store that had to
// remember to wire it is a store that could forget.
//
// NOT the primitive for every keyed store. The deciding question is "is
// there something to release?": a key backed by a backend resource that has
// to be acquired and re-acquired across a reconnect belongs here, while a
// key that is merely PUSH-FED (events arrive and are written, nothing to
// acquire, nothing to tear down) belongs in `keyedSignalRegistry.svelte.ts`,
// which is a per-key signal and nothing more. Building push-fed state on
// this one buys a refcount, a source, a retry curve and a transport edge
// that all have to be no-oped.
//
// VALUE GRANULARITY (`rawValue`). By default an entry's value is deep
// `$state`, so a consumer reading one field of a big object is woken only
// when that field changes — right for a value the store MUTATES in place.
// The proxying is not free: Svelte walks and wraps every nested object on
// first read, and a value that is thousands of objects deep (a whole run
// tree) pays that walk to buy fine-grained tracking nothing uses, on the
// main thread, in the shape of the WebView2 saturation incident. A store
// whose values are REPLACED WHOLESALE — every write produces a new object,
// nothing is written through — sets `rawValue: true` and gets one signal per
// entry instead. It is opt-in because the safety condition is a property of
// the store's writers, not of the primitive: turning it on for a store that
// mutates in place silently stops waking readers.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { getTransportStatusFor, onBackendStatusChange } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

export interface EntityAttachment<T> {
  /** Reactive current value; null before first observation. */
  readonly current: T | null;
  /** Reactive error string; null when healthy. */
  readonly error: string | null;
  release(): void;
}

export interface EntityStore<T, Ctx = void> {
  /**
   * Refcounted attach. The first attacher for a key runs source(); the last
   * release tears it down. Attaching while suspended registers the
   * reference without sourcing — resetAll() sources it. An attach that
   * lands on a key sitting in retry backoff resets the curve and sources
   * NOW: a fresh consumer is fresh demand, not the tail of somebody else's
   * failure.
   */
  attach(key: string, ctx: Ctx): EntityAttachment<T>;
  /**
   * THE single write chokepoint. Every observation — event push, RPC
   * result, post-action refresh — goes through apply, which runs onApply.
   * Applying to a key nobody holds is a no-op, not a resurrection.
   *
   * An apply is evidence the key is healthy again: it clears the error, and
   * cancels a pending retry when there is an acquired source for that retry
   * to be re-acquiring. A key whose source never acquired anything keeps its
   * timer — that timer is its only way back to a live source. `preserveError`
   * marks a PARTIAL observation — one that refreshes part of the value but
   * says nothing about the failure the key is in (prReview's thread-only
   * re-list after a submit, while the poll pump is still failing). Such an
   * apply touches neither the error nor the retry curve; only an
   * observation of the failing thing itself clears them.
   */
  apply(key: string, value: T, options?: { preserveError?: boolean }): void;
  applyError(key: string, err: unknown): void;
  /** Re-run source() for a key (retry now / config change). No-op if no live entry. */
  invalidate(key: string): void;
  /**
   * Re-run source() for EVERY live key, keeping each key's last value.
   * The transport-gap entry point: a mid-connection event drop
   * (`transport:gap`) says some push on this store's channel never
   * arrived, but carries no entity key, and an edge-triggered channel
   * has no follow-up frame to heal from. Distinct from resetAll(), which
   * blanks values and lifts suspension because a RECONNECT invalidated
   * the backend resources themselves; here the socket is fine and only
   * the observations are suspect.
   */
  invalidateAll(): void;
  /** Peek without attaching (may be null). Reactive. */
  peek(key: string): T | null;
  /** Peek the error without attaching. Reactive. */
  peekError(key: string): string | null;
  /**
   * Non-reactive read, for MUTATING paths. peek() subscribes the caller to
   * the key, which is right for rendering and wrong for a handler that
   * reads a value only to write the next one — that reads its own write
   * and re-enters. Same value, no dependency.
   */
  snapshot(key: string): T | null;
  /**
   * Tear down every entry and re-source the live ones, clearing any
   * suspension. This is the transport-reconnect entry point: the far side
   * dropped every subscription with the socket, so every key must re-acquire.
   */
  resetAll(): void;
  /**
   * Tear down every entry's backend resources and drop observed values while
   * KEEPING attachments, and refuse to source until resetAll(). The
   * transport-disconnect half of the pair: nothing can be acquired over a
   * dead socket, and a retry curve grinding against one only produces error
   * states the connection banner already explains.
   */
  suspend(): void;
  /** Enumerate live keys (diagnostics/tests). */
  keys(): string[];
}

export interface EntityStoreConfig<T, Ctx> {
  /** Diagnostics prefix. */
  name: string;
  /** The computer owning a key; null for frontend-owned state, undefined
   *  while unknown. An unknown owner's source must use guarded RPC routing;
   *  it can report ambiguity or discover ownership without assuming HOME. */
  backendForKey: (key: string) => BackendKey | null | undefined;
  /**
   * Acquire backend resources for key. Called on 0→1 refcount and on
   * invalidate / retry / resetAll. Must deliver observations via the
   * provided apply/fail. Returns a cleanup that releases whatever it
   * acquired — it is called on 1→0, suspend, resetAll, and before a
   * re-source, INCLUDING when the entry was torn down while source() was
   * still in flight (in which case nothing it applied is kept).
   *
   * getCtx() returns the ctx of any currently-live attacher: a source that
   * needs e.g. a threadId to issue an RPC uses it and must tolerate the ctx
   * belonging to a different attacher than the one that triggered the
   * source. It throws when no attacher is live, which is a programming
   * error the retry curve will surface rather than swallow.
   *
   * A ctx may expose its fields as GETTERS reading live component state —
   * an attacher that holds one key across several thread ids needs that,
   * or a re-source would run against whatever id happened to be current at
   * attach time. source()'s synchronous prologue runs untracked precisely
   * so reading them cannot make the attaching $effect a dependent of the
   * state it read.
   *
   * `fail(err)` reports an error observation from a run that is still the
   * live one. It is equivalent to throwing, minus the unwind: the key reads
   * as errored and the primitive schedules the same backed-off re-source
   * (which releases this run's cleanup first). Repeated fail()s inside one
   * backoff window are one failure, not a reason to restart the curve, and
   * the next apply() cancels it. A store must not hand-roll recovery on top
   * of this — invalidate() resets the curve, which is how a backoff stops
   * backing off. For an error pushed from OUTSIDE a source run (a wire
   * event carrying an error) use store.applyError, which does not retry.
   *
   * `signal` aborts the moment this run is superseded (release, invalidate,
   * suspend, resetAll, retry). apply/fail from a superseded run are dropped
   * for free, so a single-RPC source can ignore it — but a source that does
   * anything ELSE after an await must check it. Anything it starts past that
   * point is work for a world that no longer exists, and when that work has
   * side effects (mcpServers chains a provider health-check that spawns
   * `claude mcp list`), a superseded run doubles them.
   */
  source: (args: {
    key: string;
    getCtx: () => Ctx;
    apply: (value: T) => void;
    fail: (err: unknown) => void;
    signal: AbortSignal;
  }) => Promise<() => void | Promise<void>>;
  /**
   * Store each entry's value as `$state.raw` instead of deep `$state`.
   *
   * Legal only when every write REPLACES the value — the store's writers must
   * never mutate a held value in place, because a raw signal reports the
   * assignment and nothing below it. Worth it when values are large object
   * graphs whose consumers re-derive wholesale anyway (the run map's whole run
   * tree): deep proxying then costs a full walk per read to buy per-field
   * tracking nobody subscribes to. Default false — existing stores are
   * unaffected.
   *
   * The "must replace" half is CHECKED, not merely stated: an apply whose value
   * is the reference already held is reported to the console and to
   * `reportFrontendDiagnostic`, because the alternative failure mode is a
   * frozen surface with every gate green.
   */
  rawValue?: boolean;
  /** Reconciliation hook run on every apply (e.g. persist an observed branch). */
  onApply?: (key: string, value: T, prev: T | null) => void;
  /**
   * Run when a key's entry LEAVES the store — last release, suspend, or a
   * resetAll nobody held it through. State a store derives from an entry
   * but does not source (a per-PR CI cache, a merge-conflict tree) hangs
   * its cleanup here instead of running a second refcount beside this one.
   * Not called on a re-source: the entry survives that.
   */
  onDrop?: (key: string) => void;
}

/**
 * Backoff for source() failures while attached. One curve for every store:
 * the shape is a property of "a backend acquire failed and somebody is
 * still watching", not of what is being acquired, and a per-store knob is
 * a tuning surface nothing has ever needed to turn.
 */
export const DEFAULT_RETRY = { initialMs: 3_000, maxMs: 30_000 } as const;

// One key's shared state. `value` / `error` are the reactive surface; every
// other field is bookkeeping read only from non-reactive paths.
//
// `generation` is the staleness token. It is bumped by every teardown, so a
// source() promise, a retry timer, or an apply callback that was created
// before the bump can recognise itself as belonging to a world that no
// longer exists and decline to touch the entry. `abort` is that same signal
// pushed OUT to the running source(), which is the only party that can stop
// the work still ahead of it.
interface ValueBox<T> {
  current: T | null;
}

// One signal, of whichever granularity the store asked for. A box rather than
// two fields on the entry so exactly one signal exists per entry and every
// `entry.value` site stays a plain property access.
function createValueBox<T>(raw: boolean): ValueBox<T> {
  if (raw) {
    let value = $state.raw<T | null>(null);
    return {
      get current() { return value; },
      set current(next: T | null) { value = next; },
    };
  }
  let value = $state<T | null>(null);
  return {
    get current() { return value; },
    set current(next: T | null) { value = next; },
  };
}

class EntityEntry<T, Ctx> {
  readonly #value: ValueBox<T>;
  error = $state<string | null>(null);

  readonly key: string;
  readonly ctxs = new Map<number, Ctx>();
  refs = 0;
  generation = 0;
  sourcing = false;
  abort: AbortController | null = null;
  cleanup: (() => void | Promise<void>) | null = null;
  retryTimer: ReturnType<typeof setTimeout> | null = null;
  retryDelayMs: number;

  constructor(key: string, rawValue: boolean) {
    this.key = key;
    this.#value = createValueBox<T>(rawValue);
    this.retryDelayMs = DEFAULT_RETRY.initialMs;
  }

  get value(): T | null {
    return this.#value.current;
  }

  set value(next: T | null) {
    this.#value.current = next;
  }
}

export function createEntityStore<T, Ctx = void>(
  config: EntityStoreConfig<T, Ctx>,
): EntityStore<T, Ctx> {
  // SvelteMap so peek() on an absent key re-runs when that key appears —
  // a plain Map read cannot register that dependency.
  const entries = new SvelteMap<string, EntityEntry<T, Ctx>>();
  let suspended = false;
  let nextAttachId = 0;
  let stopWatching: (() => void) | null = null;

  function isSuspended(entry: EntityEntry<T, Ctx>): boolean {
    if (suspended) return true;
    return untrack(() => {
      const backend = config.backendForKey(entry.key);
      return backend !== null && backend !== undefined && getTransportStatusFor(backend).status !== 'connected';
    });
  }

  function watchConnections(): void {
    stopWatching ??= onBackendStatusChange((backend) => {
      for (const entry of snapshotEntries()) {
        const owner = untrack(() => config.backendForKey(entry.key));
        // An unresolved key may become routable when the computer catalog
        // changes. Frontend-owned resources (null) never follow these edges.
        if (owner !== undefined && owner !== backend) continue;
        teardown(entry);
        entry.value = null;
        entry.error = null;
        if (retained(entry)) startSource(entry);
        else dropEntry(entry);
      }
    });
  }

  // Every mutating path looks entries up through here. A SvelteMap read
  // subscribes the surrounding reaction to the key set (a miss deliberately
  // tracks the version so a later insert re-runs it), so attach() called
  // from an $effect — the normal way a component holds an entity — would
  // otherwise make that effect a dependent of its own insert: attach,
  // invalidate, release, re-attach, forever. That is a flush loop, not a
  // data dependency. Only peek()/peekError() read `entries` reactively.
  function lookup(key: string): EntityEntry<T, Ctx> | undefined {
    return untrack(() => entries.get(key));
  }

  function snapshotEntries(): EntityEntry<T, Ctx>[] {
    return untrack(() => [...entries.values()]);
  }

  function retained(entry: EntityEntry<T, Ctx>): boolean {
    return entry.refs > 0;
  }

  // The one place an entry leaves the store, so onDrop cannot be forgotten
  // by a new teardown path. Guarded on identity: a re-attach under the same
  // key mints a new entry, and dropping THAT one would take a live entry
  // out of the map behind its holder's back.
  function dropEntry(entry: EntityEntry<T, Ctx>): void {
    if (lookup(entry.key) !== entry) return;
    entries.delete(entry.key);
    if (untrack(() => entries.size) === 0) {
      stopWatching?.();
      stopWatching = null;
    }
    if (!config.onDrop) return;
    try {
      config.onDrop(entry.key);
    } catch (err) {
      console.error(`${config.name}: onDrop failed for ${entry.key}:`, err);
    }
  }

  function sourced(entry: EntityEntry<T, Ctx>): boolean {
    return entry.sourcing || entry.cleanup !== null || entry.retryTimer !== null;
  }

  async function runCleanup(
    entry: EntityEntry<T, Ctx>,
    cleanup: (() => void | Promise<void>) | null,
  ): Promise<void> {
    if (!cleanup) return;
    try {
      await cleanup();
    } catch (err) {
      // A cleanup failure leaks a backend resource; it must never be
      // silent, and it must never propagate into a caller's control flow.
      console.error(`${config.name}: cleanup failed for ${entry.key}:`, err);
      // While somebody is still holding the key, a failed release is also
      // user-facing state (root CLAUDE.md principle 5) and not only a
      // console line — the resource it failed to release is one this
      // consumer is still nominally attached to. The next successful
      // observation clears it. A cleanup that failed on the way OUT (last
      // release, drop) stays console-only: there is no reader left to tell.
      if (lookup(entry.key) === entry && retained(entry)) {
        entry.error = errString(err);
      }
    }
  }

  function applyTo(entry: EntityEntry<T, Ctx>, value: T, preserveError = false): void {
    // An observation proves the ACQUIRED resource healthy, so the pending
    // teardown-and-reacquire of it is pointless: drop it and put the curve
    // back at the bottom. Here rather than at the source's apply callback,
    // so a wire event arriving through the public apply() cannot leave an
    // armed timer behind that re-acquires a resource nothing is wrong with.
    //
    // When nothing is acquired — the run threw, or failed before returning
    // a cleanup — the timer is the ONLY path back to a live source, and an
    // observation that reached this key by another route (a sibling alias
    // sharing one wire key) is no evidence the subscribe recovered. It
    // survives, curve included. A PARTIAL observation is exempt either way:
    // it is not evidence of recovery at all.
    if (!preserveError && (entry.cleanup !== null || entry.sourcing)) cancelRetry(entry);
    // untrack the read of the value we are about to overwrite: apply() is
    // reachable from a caller that is itself inside a reaction, and taking
    // a dependency on `value` there means writing it invalidates the
    // reader — a self-re-entering effect, not a data dependency.
    const prev = untrack(() => entry.value);
    // The `rawValue` contract, ENFORCED rather than documented. A raw signal
    // reports the assignment and nothing below it, so a writer that mutated the
    // held object and applied it back writes the same reference — which wakes
    // no reader and freezes the UI with every gate green and nothing in any
    // log. There is no legitimate same-reference apply: an observation that
    // changed nothing had nothing to apply, and one that changed something
    // produced a new object. Reported, not thrown: the value IS the current
    // truth, and taking the surface down over a stale render helps nobody.
    if (config.rawValue === true && prev !== null && Object.is(prev, value)) {
      const message = `${config.name}: applied the same object reference for ${entry.key}`;
      const detail = 'rawValue entries must be REPLACED, never mutated in place —'
        + ' a same-reference apply wakes no reader.';
      console.error(`${message}. ${detail}`);
      reportFrontendDiagnostic(message, detail);
    }
    entry.value = value;
    if (!preserveError) entry.error = null;
    if (!config.onApply) return;
    try {
      config.onApply(entry.key, value, prev);
    } catch (err) {
      console.error(`${config.name}: onApply failed for ${entry.key}:`, err);
    }
  }

  function failTo(entry: EntityEntry<T, Ctx>, err: unknown): void {
    entry.error = errString(err);
    console.error(`${config.name}: ${entry.key}:`, err);
  }

  // Ends the entry's current life: cancels a pending retry, invalidates any
  // in-flight source and its callbacks, and releases the acquired resource.
  // Leaves value/error alone — callers decide whether the observation
  // survives the teardown (a re-source keeps it; a release drops it).
  function teardown(entry: EntityEntry<T, Ctx>): void {
    entry.generation += 1;
    entry.sourcing = false;
    entry.abort?.abort();
    entry.abort = null;
    if (entry.retryTimer !== null) {
      clearTimeout(entry.retryTimer);
      entry.retryTimer = null;
    }
    entry.retryDelayMs = DEFAULT_RETRY.initialMs;
    const cleanup = entry.cleanup;
    entry.cleanup = null;
    void runCleanup(entry, cleanup);
  }

  // Drop a pending retry and put the curve back at the bottom.
  function cancelRetry(entry: EntityEntry<T, Ctx>): void {
    if (entry.retryTimer !== null) {
      clearTimeout(entry.retryTimer);
      entry.retryTimer = null;
    }
    entry.retryDelayMs = DEFAULT_RETRY.initialMs;
  }

  function scheduleRetry(entry: EntityEntry<T, Ctx>, generation: number): void {
    // Retry only while somebody is holding the key. A detached entry that
    // kept retrying would resurrect itself forever.
    if (!retained(entry) || isSuspended(entry)) return;
    // One curve per failure, not per report of it. A source that keeps
    // fail()ing (an event-driven poll against a broken backend) must not
    // restart the curve on every event — that is how a backoff never backs
    // off.
    if (entry.retryTimer !== null) return;
    const delayMs = entry.retryDelayMs;
    const nextDelayMs = Math.min(delayMs * 2, DEFAULT_RETRY.maxMs);
    entry.retryDelayMs = nextDelayMs;
    entry.retryTimer = setTimeout(() => {
      entry.retryTimer = null;
      if (entry.generation !== generation) return;
      // fail() leaves the acquired resource in place (unlike a throw, which
      // never produced one), so the retry has to release it before
      // re-acquiring or the source's listeners stack up. teardown resets the
      // curve because it normally starts a fresh life; this one continues
      // the same failure, so the delay is carried across it.
      teardown(entry);
      entry.retryDelayMs = nextDelayMs;
      startSource(entry);
    }, delayMs);
  }

  function startSource(entry: EntityEntry<T, Ctx>): void {
    if (isSuspended(entry) || entry.sourcing) return;
    const generation = entry.generation;
    entry.sourcing = true;
    const controller = new AbortController();
    entry.abort = controller;
    void (async () => {
      try {
        // untrack: source()'s prologue runs synchronously inside attach(),
        // which a component runs from an $effect. Anything reactive it
        // touches on the way to its first await — a ctx getter reading
        // `pane.threadId` — would otherwise become a dependency of that
        // effect, and the effect's own attach would then invalidate it.
        const cleanup = await untrack(() => config.source({
          key: entry.key,
          signal: controller.signal,
          getCtx: () => {
            const first = entry.ctxs.values().next();
            if (entry.ctxs.size === 0 || first.done) {
              throw new Error(`${config.name}: no live attacher for ${entry.key}`);
            }
            return first.value;
          },
          apply: (value) => {
            if (entry.generation !== generation) return;
            applyTo(entry, value);
          },
          fail: (err) => {
            if (entry.generation !== generation) return;
            failTo(entry, err);
            // A failure inside a source run is the primitive's to recover
            // from whether it arrives by throw or by fail(). Without this a
            // store whose observations are pushed (rather than returned)
            // had to hand-roll recovery, and the one that did reached for
            // invalidate() — which resets the curve, so it never backed off.
            scheduleRetry(entry, generation);
          },
        }));
        if (entry.generation !== generation) {
          // Torn down (or re-sourced) while source() was in flight: the
          // resource it just acquired belongs to nobody. Release it here —
          // teardown had nothing to release at the time — and apply nothing.
          await runCleanup(entry, cleanup);
          return;
        }
        entry.sourcing = false;
        entry.cleanup = cleanup;
        entry.retryDelayMs = DEFAULT_RETRY.initialMs;
      } catch (err) {
        if (entry.generation !== generation) return;
        entry.sourcing = false;
        failTo(entry, err);
        scheduleRetry(entry, generation);
      }
    })();
  }

  // Release and re-acquire one entry's backend resource. The observed
  // value survives: re-acquiring is not evidence that what we last saw is
  // wrong, and blanking it flickers every consumer.
  function resourceAgain(entry: EntityEntry<T, Ctx>): void {
    teardown(entry);
    startSource(entry);
  }

  function ensureEntry(key: string): EntityEntry<T, Ctx> {
    let entry = lookup(key);
    if (!entry) {
      entry = new EntityEntry<T, Ctx>(key, config.rawValue === true);
      entries.set(key, entry);
    }
    return entry;
  }

  const store: EntityStore<T, Ctx> = {
    attach(key, ctx) {
      const entry = ensureEntry(key);
      const attachId = nextAttachId++;
      entry.ctxs.set(attachId, ctx);
      entry.refs += 1;
      watchConnections();
      if (entry.retryTimer !== null) {
        // Somebody new is watching. Waiting out the rest of a backoff that
        // belongs to a previous holder's failure would leave a freshly
        // mounted consumer blank for up to maxMs, so the curve resets and
        // this attach re-sources immediately.
        //
        // Re-source, rather than cancel-then-source-if-unsourced: a fail()
        // that arrived AFTER its run had acquired a cleanup leaves that
        // cleanup in place (that is what makes it a fail and not a throw),
        // so `sourced()` still reads true. Cancelling the timer alone would
        // then start nothing and leave nothing armed — the key stays errored
        // for as long as anyone holds it.
        resourceAgain(entry);
      } else if (!sourced(entry)) {
        startSource(entry);
      }

      let released = false;
      return {
        get current() {
          return entry.value;
        },
        get error() {
          return entry.error;
        },
        release() {
          if (released) return;
          released = true;
          entry.ctxs.delete(attachId);
          entry.refs -= 1;
          if (retained(entry)) return;
          teardown(entry);
          entry.value = null;
          entry.error = null;
          dropEntry(entry);
        },
      };
    },

    apply(key, value, options) {
      const entry = lookup(key);
      if (!entry) return;
      applyTo(entry, value, options?.preserveError === true);
    },

    applyError(key, err) {
      const entry = lookup(key);
      if (!entry) return;
      failTo(entry, err);
    },

    invalidate(key) {
      const entry = lookup(key);
      if (!entry || !retained(entry)) return;
      resourceAgain(entry);
    },

    invalidateAll() {
      // Nothing can be acquired while suspended, and the resetAll that
      // lifts the suspension re-sources everything anyway — so this would
      // only churn generations and cancel the retry curve for free.
      if (suspended) return;
      for (const entry of snapshotEntries()) {
        if (!retained(entry)) continue;
        resourceAgain(entry);
      }
    },

    peek(key) {
      return entries.get(key)?.value ?? null;
    },

    peekError(key) {
      return entries.get(key)?.error ?? null;
    },

    snapshot(key) {
      // The untrack has to span the VALUE read too, not just the map
      // lookup: `entry.value` is the $state, and reading it through an
      // untracked lookup still registers the dependency this exists to
      // avoid.
      return untrack(() => entries.get(key)?.value ?? null);
    },

    resetAll() {
      suspended = false;
      for (const entry of snapshotEntries()) {
        teardown(entry);
        entry.value = null;
        entry.error = null;
        if (retained(entry)) startSource(entry);
        else dropEntry(entry);
      }
    },

    suspend() {
      suspended = true;
      for (const entry of snapshotEntries()) {
        teardown(entry);
        entry.value = null;
        entry.error = null;
        if (!retained(entry)) dropEntry(entry);
      }
    },

    keys() {
      return untrack(() => [...entries.keys()]);
    },
  };

  return store;
}
