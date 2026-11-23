// harnessMode carries one bit out of the bootstrap manifest: whether the
// backend on the other end of this WebSocket was booted as the agent test
// harness or the soak rig (internal/transport/server.go
// `Bootstrap.Harness`, set by main.go exactly where it registers the
// `Harness` RPC receiver).
//
// It exists so the frontend harness bridge (stores/harnessBridge.ts) can
// be ARMED rather than IMPORTED. The bridge subscribes to a wire channel,
// installs a document-wide MutationObserver and can hold a rAF loop open;
// none of that may happen in an ordinary boot. Keying it on a manifest
// field rather than a build flag is what lets a production binary serve a
// harness with no frontend rebuild, which §4 of
// docs/specs/testing-harness.md asks for explicitly.
//
// Unlike runMode's view-only bit this is NOT reactive: nothing renders
// from it. It is a one-shot arm, so the shape callers need is "tell me
// when/if this is true", not "re-read me on every paint".
//
// The flag LATCHES. A manifest is refetched on reconnect revalidation and
// a latch means a bridge, once armed, is never silently disarmed by a
// field that went missing; the alternative buys nothing, because a
// backend cannot stop being a harness without restarting.

let harness = false;
let pageMarker = '';

/**
 * The frontend timers a harness boot may shorten, by their
 * AO_HARNESS_TIMING names: the page rows of main_harness.go `harnessTimers`,
 * which TestFrontendTimersMatchThePage holds to this set. Each one's product
 * constant stays the default; its owner reads it through `harnessInterval`
 * when it arms.
 */
export type HarnessFrontendTimer = 'unreachable-settle' | 'agent-row-linger';
const FRONTEND_TIMERS: ReadonlySet<string> = new Set<HarnessFrontendTimer>([
  'unreachable-settle',
  'agent-row-linger',
]);
/** A manifest's frontend timer overrides, read by `readHarnessTimingFromBootstrap`. */
export type HarnessTiming = ReadonlyMap<HarnessFrontendTimer, number>;
let timing: HarnessTiming = new Map();
const waiting = new Set<() => void>();

/** Whether this session is attached to a --harness / --soak backend. */
export function isHarnessSession(): boolean {
  return harness;
}

export function harnessPageMarker(): string {
  return pageMarker;
}

// Called only by bootstrap.ts once a manifest has been validated, the same
// boundary setPageGrantsFromBootstrap sits on: a wire value must not
// arm the bridge before the manifest it came from has been accepted.
export function setHarnessSessionFromBootstrap(value: boolean): void {
  if (harness || !value) return;
  harness = true;
  // Each waiter arms exactly once; taking it out of the set before the
  // call means a waiter that throws cannot be re-run by a later manifest.
  for (const waiter of [...waiting]) {
    waiting.delete(waiter);
    waiter();
  }
}


/**
 * The home boot's override for `timer`, or `productMs`: the page timers
 * follow the boot that served this page.
 */
export function harnessInterval(timer: HarnessFrontendTimer, productMs: number): number {
  return timing.get(timer) ?? productMs;
}

/**
 * The longest timer setTimeout arms as given: its delay is a signed 32-bit
 * count of milliseconds, and a longer one wraps and fires at once
 * (transport.MaxFrontendTimingMs on the backend).
 */
const MAX_TIMER_MS = 2 ** 31 - 1;

/**
 * A manifest's frontend timer overrides, validated and not yet published.
 * Timing without the harness bit is no timing: only a harness boot may
 * shorten a product timer. An entry this build does not name, or a
 * duration that is not a whole number of milliseconds from 1 to
 * MAX_TIMER_MS, is a defect in the boot that served it and throws, so the
 * manifest that carried it is refused. bootstrap.ts reads it before it
 * publishes anything from the manifest.
 */
export function readHarnessTimingFromBootstrap(isHarness: boolean, value: unknown): HarnessTiming {
  const next = new Map<HarnessFrontendTimer, number>();
  if (!isHarness || value === undefined) return next;
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`bootstrap harnessTiming is not an object: ${JSON.stringify(value)}`);
  }
  for (const [name, ms] of Object.entries(value)) {
    if (!FRONTEND_TIMERS.has(name)) throw new Error(`bootstrap harnessTiming names an unknown timer ${JSON.stringify(name)}`);
    if (typeof ms !== 'number' || !Number.isInteger(ms) || ms <= 0 || ms > MAX_TIMER_MS) {
      throw new Error(`bootstrap harnessTiming ${name} needs a whole number of milliseconds from 1 to ${MAX_TIMER_MS}, got ${JSON.stringify(ms)}`);
    }
    next.set(name as HarnessFrontendTimer, ms);
  }
  return next;
}

// Called by bootstrap.ts with each accepted home manifest's timing
// (readHarnessTimingFromBootstrap). Every manifest replaces the page's
// timing: a later manifest's values win, and one without overrides restores
// the product timers. An owner reads its timer when it arms, so a timer
// already armed keeps the value it armed with.
export function setHarnessTimingFromBootstrap(next: HarnessTiming): void {
  timing = next;
}

export function setHarnessPageMarkerFromBootstrap(value: unknown): void {
  if (typeof value === 'string' && value !== '') pageMarker = value;
}

/**
 * Runs `arm` as soon as the session is known to be a harness session —
 * immediately when the manifest already resolved, otherwise on the
 * manifest that says so. Returns a canceller for the not-yet-armed case;
 * once armed, the caller owns whatever `arm` installed.
 */
export function whenHarnessSession(arm: () => void): () => void {
  if (harness) {
    arm();
    return () => {};
  }
  waiting.add(arm);
  return () => {
    waiting.delete(arm);
  };
}

/** Test-only: forget the latch and any pending waiters. */
export function __resetHarnessModeForTest(): void {
  harness = false;
  pageMarker = '';
  timing = new Map();
  waiting.clear();
}
