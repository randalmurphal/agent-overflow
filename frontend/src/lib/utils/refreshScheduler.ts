// One serialized, rate-bounded refresh driven by an event stream.
//
// THE HAZARD THIS REPLACES. `utils/debounce` is a pure TRAILING debounce:
// every call clears the standing timer and arms a new one, so a stream whose
// gaps stay under the delay postpones the wrapped call FOREVER. That is
// correct for a quiet-edge persist — nothing is lost by writing once the user
// stops — and it is a starvation bug for an AUTHORITATIVE refresh, where the
// wrapped call is the only thing that makes the surface true. Three modules
// had hand-rolled that same unsafe pattern over provider event streams (the
// activity rail's background tray, the workspace-change lock, the env picker's
// worktree rows). In production on 2026-08-29 the Background pill read 10
// against a truth of 3-4 for as long as any pane kept streaming — and two of
// the three gate WORKSPACE MUTATION, where a stale answer is a safety defect
// rather than a cosmetic one.
//
// WHAT THIS DOES INSTEAD. One dirty bit and one standing timer armed at an
// ABSOLUTE deadline: the refresh runs at
//   min(last request + delayMs, first request of the cycle + maxWaitMs)
// so a burst is coalesced but a flood still lands on schedule. `request()`
// under a standing timer writes two fields and nothing else — no
// clearTimeout/setTimeout churn per event, and the number of timers per cycle
// is bounded by maxWaitMs/delayMs rather than by the event rate.
//
// Runs are SERIALIZED: at most one is in flight, a `request()` during one sets
// the dirty bit and is answered by exactly one trailing run, and the minimum
// interval is measured from that run's COMPLETION — so a slow RPC is never
// re-issued at the event rate, and two answers to the same question can never
// be in flight together to land out of order.
//
// GENERATION GATING IS THE SCHEDULER'S JOB, not the consumer's. `run` receives
// a token whose `isCurrent()` goes false the moment the run is superseded, the
// key is `reset()` (thread / workspace / popover switch), or the scheduler is
// disposed. A late response therefore cannot be applied to a surface that has
// moved on, and no consumer has to remember to hand-roll a sequence number —
// the three that did each hand-rolled it slightly differently.
//
// WHEN PLAIN `debounce` IS STILL RIGHT: a quiet-edge write whose whole job is
// to happen after the user stops, with no reader in between — pane-layout
// persistence during a divider drag. Nothing reads it mid-burst and nothing
// goes stale while it waits, so starving it for the length of the drag is the
// point rather than the bug.

import { untrack } from 'svelte';

/** Handed to `run`; the one staleness check a consumer needs. */
export interface RefreshToken {
  /**
   * False once this run has been superseded by a newer one, invalidated by
   * `reset()`, or killed by `dispose()`. Check it after every await before
   * touching state — a response that arrives past it belongs to a world that
   * no longer exists.
   */
  isCurrent(): boolean;
}

export interface RefreshScheduler {
  /**
   * Mark the surface dirty. Cheap and flood-safe: with a timer already
   * standing this writes two fields and returns.
   *
   * `immediate` runs now (subject only to the in-flight guard) and is how an
   * initial load or an explicit "re-check now" enters — through the SAME
   * scheduler, so it cannot race a newer event-driven refresh.
   */
  request(options?: { immediate?: boolean }): void;
  /**
   * The key changed (thread switch, workspace re-point, popover reopen).
   * Invalidates the in-flight run's token, drops the dirty bit, cancels the
   * timer and releases the cooldown, so the caller's own `request()` starts a
   * clean cycle.
   */
  reset(): void;
  /** Permanent. Cancels the timer, invalidates the token, and refuses further work. */
  dispose(): void;
}

export interface RefreshSchedulerConfig {
  /** Diagnostics prefix, used when a run rejects. */
  name: string;
  /** Coalescing delay: quiet time after the last request before running. */
  delayMs: number;
  /** Absolute bound: a cycle runs this long after its FIRST request, however busy the stream. */
  maxWaitMs: number;
  /**
   * Quiet time after a run COMPLETES before the next one may start. Defaults
   * to `delayMs`: the cost of a refresh is the round trip, so the rate limit
   * belongs on the far side of it, not on the near side.
   */
  minIntervalMs?: number;
  run: (token: RefreshToken) => void | Promise<void>;
}

export function createRefreshScheduler(config: RefreshSchedulerConfig): RefreshScheduler {
  const { name, delayMs, maxWaitMs, run } = config;
  const minIntervalMs = config.minIntervalMs ?? delayMs;

  let disposed = false;
  // Bumped by every run start, reset and dispose. A token minted for
  // generation N recognises itself as stale by comparing against this.
  let generation = 0;
  // Generation of the run currently in flight, if any. A run whose generation
  // is no longer current is a ghost: it may still resolve, but it neither
  // blocks a new run nor owns the cooldown.
  let inFlightGen: number | null = null;
  let dirty = false;
  let firstRequestAt: number | null = null;
  let lastRequestAt = 0;
  let cooldownUntil = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const now = (): number => Date.now();

  function running(): boolean {
    return inFlightGen !== null && inFlightGen === generation;
  }

  function clearTimer(): void {
    if (timer === null) return;
    clearTimeout(timer);
    timer = null;
  }

  // When the standing cycle is allowed to run. Never moves EARLIER within a
  // cycle — `lastRequestAt` only advances and the deadline is fixed — which is
  // what lets `request()` leave a standing timer completely alone.
  function targetTime(): number {
    const byDelay = lastRequestAt + delayMs;
    const byDeadline = (firstRequestAt ?? lastRequestAt) + maxWaitMs;
    return Math.max(Math.min(byDelay, byDeadline), cooldownUntil);
  }

  function schedule(): void {
    if (disposed || !dirty || running() || timer !== null) return;
    timer = setTimeout(onTimer, Math.max(0, targetTime() - now()));
  }

  function onTimer(): void {
    timer = null;
    if (disposed || !dirty) return;
    // A run started under us (an `immediate` request). Its completion
    // re-schedules the trailing work; arming a second timer here would only
    // race it.
    if (running()) return;
    const remaining = targetTime() - now();
    // Re-arm rather than clear-and-re-arm per event: laps are bounded by
    // maxWaitMs/delayMs, not by how many events arrived inside them.
    if (remaining > 0) {
      timer = setTimeout(onTimer, remaining);
      return;
    }
    startRun();
  }

  function startRun(): void {
    if (disposed || running()) return;
    clearTimer();
    dirty = false;
    firstRequestAt = null;
    const gen = (generation += 1);
    inFlightGen = gen;
    const token: RefreshToken = { isCurrent: () => !disposed && generation === gen };
    // Synchronously, and UNTRACKED. `request({ immediate: true })` is called
    // from inside a Svelte `$effect` (thread switch, popover open) and the
    // run's prologue reads the consumer's state on the way to its RPC; tracked,
    // that would make the effect a dependent of everything the refresh happens
    // to touch and re-run the whole subscribe/teardown on unrelated churn.
    // (`stores/entityStore.svelte.ts` untracks its source prologue for exactly
    // this reason.) Deferring the call by a microtask would detach it too, but
    // it would also move WHEN the request goes out — an initial load that
    // leaves the caller's turn is a different contract than the one every call
    // site was written against.
    let settled: void | Promise<void>;
    try {
      settled = untrack(() => run(token));
    } catch (err) {
      reportFailure(err);
      settle(gen);
      return;
    }
    void Promise.resolve(settled).catch(reportFailure).then(() => settle(gen));
  }

  // The consumers own their error posture and catch inside `run`; this is the
  // net that keeps a rejection from becoming an unhandled one and silently
  // stranding the cooldown with a run that never settles.
  function reportFailure(err: unknown): void {
    console.error(`refreshScheduler(${name}): run rejected:`, err);
  }

  function settle(gen: number): void {
    if (inFlightGen === gen) inFlightGen = null;
    // A superseded run's completion says nothing about the live cycle: it must
    // not stamp a cooldown onto it, and the trailing work it left behind (if
    // any) belongs to whoever superseded it.
    if (disposed || gen !== generation) return;
    cooldownUntil = now() + minIntervalMs;
    if (dirty) schedule();
  }

  return {
    request(options) {
      if (disposed) return;
      const at = now();
      if (firstRequestAt === null) firstRequestAt = at;
      lastRequestAt = at;
      dirty = true;
      if (options?.immediate === true) {
        // Still subject to the in-flight guard: the dirty bit is already set,
        // so the run in flight is followed by exactly one trailing run.
        if (running()) return;
        // An explicit "now" is not rate-limited — it is a fresh demand (an
        // initial load, a post-action re-check), not the tail of a flood.
        cooldownUntil = 0;
        startRun();
        return;
      }
      schedule();
    },

    reset() {
      if (disposed) return;
      generation += 1;
      clearTimer();
      dirty = false;
      firstRequestAt = null;
      cooldownUntil = 0;
    },

    dispose() {
      if (disposed) return;
      disposed = true;
      generation += 1;
      clearTimer();
      dirty = false;
      firstRequestAt = null;
    },
  };
}
