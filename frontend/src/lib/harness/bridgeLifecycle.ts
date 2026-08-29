// Lifecycle state for the frontend harness bridge. The query dispatcher keeps
// this module behind its protocol facade, while the mutation clock and
// teardown receipt retain state across individual queries and page reloads.

import { stopAllMonitors } from './monitorBridge';
import {
  clearPerfSelfDisarm,
  perfRunActive,
  stopPerfRunForTeardown,
  type PerfTeardownReceipt,
} from './perf';

// ---------------------------------------------------------------------------
// settled: the mutation clock.
//
// A one-shot query cannot WAIT for quiet without holding the backend
// waiter hostage, so it reports how long the document has been quiet and
// lets the caller poll. The observer is document-wide and harness-only;
// its callback does nothing but stamp a number, so the cost is the
// engine's own record-keeping rather than anything this code does — which
// is exactly why its lifetime is scoped to the queries that need it: the
// engine's record-keeping is not free in a rig that is measuring the engine.
//
// The clock therefore starts at each fresh ARM, not at page load and not
// at chunk load. That query answers `settled:false` because the bridge has
// only just begun watching, and that is the honest answer: quiet nobody
// observed is not evidence of quiet. A caller polling for settle gets it on
// the next query, which is how every settle wait in the CLI and the e2e
// suite already works — and the linger below is what keeps a poll loop from
// re-arming (and so re-zeroing) the clock on every lap.

let lastMutationAt = 0;
let observer: MutationObserver | null = null;
let lingerTimer: ReturnType<typeof setTimeout> | null = null;

/**
 * How long the mutation clock stays armed after the last query that wanted
 * it. Long enough that a settle POLL — `expect.poll` in the e2e suite, a
 * human running `ao-harness ui` twice — keeps one continuous history rather
 * than restarting the clock every lap; short enough that a perf run or a
 * bench workload started after a stray `ui` snapshot is not measuring a
 * renderer carrying an observer production does not have.
 */
export const MUTATION_CLOCK_LINGER_MS = 5_000;

function now(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

/**
 * Arms the mutation clock for a query that reports settledness, and pushes
 * the linger out. Idempotent while armed: re-arming inside the linger keeps
 * the history the clock already has, because a poll loop that re-zeroed the
 * clock every lap could never observe a settle.
 */
export function armMutationClock(): void {
  if (observer !== null) {
    scheduleMutationClockDisarm();
    return;
  }
  // A fresh arm has no history. Stamped even when the observer cannot be
  // installed at all (no MutationObserver, no document), because the
  // alternative — a `lastMutationAt` of 0 — reads as hours of quiet and
  // would answer `settled:true` off an observation nobody made.
  lastMutationAt = now();
  if (typeof MutationObserver === 'undefined' || typeof document === 'undefined') return;
  observer = new MutationObserver(() => {
    lastMutationAt = now();
  });
  observer.observe(document.documentElement, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
  });
  scheduleMutationClockDisarm();
}

function scheduleMutationClockDisarm(): void {
  if (typeof setTimeout !== 'function') return;
  if (lingerTimer !== null && typeof clearTimeout === 'function') clearTimeout(lingerTimer);
  lingerTimer = setTimeout(() => {
    lingerTimer = null;
    disarmMutationClock();
  }, MUTATION_CLOCK_LINGER_MS);
  // The linger must never be the reason a Node-side test process (or a
  // future SSR pass) stays alive. Same contract as the perf watchdog.
  (lingerTimer as { unref?: () => void }).unref?.();
}

/** Disconnects the observer and cancels the linger. Safe when not armed. */
export function disarmMutationClock(): void {
  if (lingerTimer !== null && typeof clearTimeout === 'function') clearTimeout(lingerTimer);
  lingerTimer = null;
  observer?.disconnect();
  observer = null;
}

/** Whether the document-wide mutation observer is installed right now. */
export function mutationClockArmed(): boolean {
  return observer !== null;
}

/**
 * Per-page-load init for the bridge chunk. Installs NOTHING: the mutation
 * clock is armed by the queries that need it and disarmed again when they
 * stop asking, so a soak or a perf run that never asks for `settled` carries
 * no document-wide observer at all.
 *
 * What it does do is start from a clean slate. The chunk survives a
 * teardown — `stores/harnessBridge.ts` drops its reference but the module
 * stays loaded — so a second activation in the same page must not inherit
 * the previous bridge's clock or its perf self-disarm notice.
 */
export function activateHarnessBridge(): () => void {
  stopHarnessBridge('page-unload');
  return () => stopHarnessBridge('page-unload');
}

export interface HarnessBridgeTeardownReceipt {
  v: 1;
  kind: 'bridge-teardown';
  reason: 'page-unload' | 'bridge-close';
  partial: true;
  perf: PerfTeardownReceipt | null;
  monitors: ReturnType<typeof stopAllMonitors>;
  errors: readonly string[];
}

/** Session-scoped so a reload can inspect the last page's partial evidence. */
export const HARNESS_TEARDOWN_RECEIPT_STORAGE_KEY = 'agent-overflow:harness:teardown-receipt:v1';

function isTeardownReceipt(value: unknown): value is HarnessBridgeTeardownReceipt {
  if (!value || typeof value !== 'object') return false;
  const receipt = value as Partial<HarnessBridgeTeardownReceipt>;
  return receipt.v === 1
    && receipt.kind === 'bridge-teardown'
    && (receipt.reason === 'page-unload' || receipt.reason === 'bridge-close')
    && receipt.partial === true
    && (receipt.perf === null || (typeof receipt.perf === 'object' && receipt.perf !== null))
    && Array.isArray(receipt.monitors)
    && Array.isArray(receipt.errors)
    && receipt.errors.every((error) => typeof error === 'string');
}

function readPersistedTeardownReceipt(): HarnessBridgeTeardownReceipt | null {
  if (typeof sessionStorage === 'undefined') return null;
  try {
    const raw = sessionStorage.getItem(HARNESS_TEARDOWN_RECEIPT_STORAGE_KEY);
    if (raw === null) return null;
    const parsed: unknown = JSON.parse(raw);
    if (isTeardownReceipt(parsed)) return parsed;
    console.error('harness bridge: persisted teardown receipt has an invalid shape');
    sessionStorage.removeItem(HARNESS_TEARDOWN_RECEIPT_STORAGE_KEY);
  } catch (error) {
    console.error('harness bridge: persisted teardown receipt could not be read:', error);
  }
  return null;
}

let lastTeardownReceipt: HarnessBridgeTeardownReceipt | null = readPersistedTeardownReceipt();

function teardownError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function retainTeardownReceipt(receipt: HarnessBridgeTeardownReceipt): HarnessBridgeTeardownReceipt {
  if (typeof sessionStorage === 'undefined') return receipt;
  try {
    sessionStorage.setItem(HARNESS_TEARDOWN_RECEIPT_STORAGE_KEY, JSON.stringify(receipt));
  } catch (error) {
    const persisted = {
      ...receipt,
      errors: [...receipt.errors, `teardown receipt persistence failed: ${teardownError(error)}`],
    } satisfies HarnessBridgeTeardownReceipt;
    console.error('harness bridge: teardown receipt persistence failed:', error);
    return persisted;
  }
  return receipt;
}

export function stopHarnessBridge(reason: HarnessBridgeTeardownReceipt['reason'] = 'bridge-close'): HarnessBridgeTeardownReceipt {
  disarmMutationClock();
  const errors: string[] = [];
  let perf: PerfTeardownReceipt | null = null;
  try {
    perf = perfRunActive() ? stopPerfRunForTeardown() : null;
  } catch (error) {
    errors.push(`perf teardown failed: ${teardownError(error)}`);
  }
  let monitors: ReturnType<typeof stopAllMonitors> = [];
  try {
    monitors = stopAllMonitors();
  } catch (error) {
    errors.push(`monitor teardown failed: ${teardownError(error)}`);
  }
  clearPerfSelfDisarm();
  if (perf === null && monitors.length === 0 && errors.length === 0 && lastTeardownReceipt !== null) {
    return lastTeardownReceipt;
  }
  lastTeardownReceipt = retainTeardownReceipt({ v: 1, kind: 'bridge-teardown', reason, partial: true, perf, monitors, errors });
  return lastTeardownReceipt;
}

export function lastHarnessBridgeTeardownReceipt(): HarnessBridgeTeardownReceipt | null {
  return lastTeardownReceipt;
}

/** Milliseconds since the last observed DOM mutation. */
export function sinceLastMutationMs(): number {
  return Math.max(0, now() - lastMutationAt);
}
