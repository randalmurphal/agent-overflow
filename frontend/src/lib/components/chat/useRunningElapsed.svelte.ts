import { formatElapsedSeconds } from '../../utils/format';

/**
 * Shared running-elapsed ticker for tool-row headers (AdvisorRow,
 * AgentRow, GenericToolCallRow). Three rows previously held
 * structurally-identical `now = $state`, `setInterval` $effect, and
 * threshold-gated label derivations. Centralising the contract here
 * means a future change to the 2s warm-up threshold, the 1s tick
 * interval, or the SSR/test exception lands in one place.
 *
 * Usage (must be called from a `.svelte` component, since it uses
 * runes):
 *
 *   const ticker = createRunningElapsed(
 *     () => item.status === 'running' || item.status === 'streaming',
 *     () => item.createdAt,
 *   );
 *   const label = $derived(ticker.label);
 *
 * The threshold guard hides the label entirely during the first 2s
 * so a fast tool call doesn't show "0s" → "1s" jitter before
 * resolving. Callers can still gate downstream logic on
 * `ticker.label === ''` if they want to hide the duration cell.
 */
export const RUNNING_ELAPSED_THRESHOLD_MS = 2_000;

interface RunningElapsed {
  readonly label: string;
}

let sharedNow = $state(Date.now());
let sharedTicker: ReturnType<typeof setInterval> | null = null;
let activeTickerSubscriberCount = 0;
let reactiveTickerSubscriberCount = $state(0);
let stopQueued = false;

function stopSharedTickerIfIdle(): void {
  stopQueued = false;
  if (activeTickerSubscriberCount > 0 || sharedTicker === null) return;
  clearInterval(sharedTicker);
  sharedTicker = null;
}

function queueSharedTickerStop(): void {
  if (stopQueued) return;
  stopQueued = true;
  queueMicrotask(stopSharedTickerIfIdle);
}

function acquireRunningElapsedTicker(): () => void {
  activeTickerSubscriberCount += 1;
  reactiveTickerSubscriberCount = activeTickerSubscriberCount;
  sharedNow = Date.now();
  if (sharedTicker === null) {
    sharedTicker = setInterval(() => {
      sharedNow = Date.now();
    }, 1_000);
  }

  let released = false;
  return () => {
    if (released) return;
    released = true;
    activeTickerSubscriberCount -= 1;
    reactiveTickerSubscriberCount = activeTickerSubscriberCount;
    if (activeTickerSubscriberCount === 0) queueSharedTickerStop();
  };
}

export function createRunningElapsed(
  isTicking: () => boolean,
  createdAt: () => number,
  thresholdMs: number = RUNNING_ELAPSED_THRESHOLD_MS,
): RunningElapsed {
  $effect(() => {
    if (!isTicking()) return;
    return acquireRunningElapsedTicker();
  });

  const label = $derived.by<string>(() => {
    if (!isTicking()) return '';
    const created = createdAt();
    if (!Number.isFinite(created) || created <= 0) return '';
    const elapsedMs = sharedNow - created;
    if (elapsedMs < thresholdMs) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
  });

  return {
    get label() {
      return label;
    },
  };
}

/**
 * Direct read access to the shared 1Hz clock, for rows whose label math
 * doesn't fit the threshold-gated running-elapsed contract (a completed
 * branch anchored on `updatedAt`, retention pruning). `isTicking` gates
 * interval ownership exactly like `createRunningElapsed`; while it is
 * false the shared interval can wind down (if no other subscriber holds
 * it) and `now` goes stale exactly like a private frozen clock — reads
 * that matter must therefore be gated on the same predicate.
 *
 * Must be called from component/effect-root context (uses runes).
 */
export function createSharedNowClock(isTicking: () => boolean): { readonly now: number } {
  // Refresh the clock at creation so the first render doesn't compute
  // against a sharedNow left over from whenever the last subscriber
  // released (acquire refreshes too, but effects run after render).
  sharedNow = Date.now();
  $effect(() => {
    if (!isTicking()) return;
    return acquireRunningElapsedTicker();
  });
  return {
    get now() {
      return sharedNow;
    },
  };
}

export function __runningElapsedTickerSubscribersForTest(): number {
  return reactiveTickerSubscriberCount;
}

export function __runningElapsedTickerActiveForTest(): boolean {
  return sharedTicker !== null;
}

export function __resetRunningElapsedTickerForTest(): void {
  if (sharedTicker !== null) {
    clearInterval(sharedTicker);
    sharedTicker = null;
  }
  activeTickerSubscriberCount = 0;
  reactiveTickerSubscriberCount = 0;
  stopQueued = false;
  sharedNow = Date.now();
}
