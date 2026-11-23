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
export type HarnessFrontendTimer = 'unreachable-settle' | 'agent-row-linger' | 'replay-timeout';
const FRONTEND_TIMERS: ReadonlySet<string> = new Set<HarnessFrontendTimer>([
  'unreachable-settle',
  'agent-row-linger',
  'replay-timeout',
]);
let timing: ReadonlyMap<HarnessFrontendTimer, number> = new Map();
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
 * (unreachable-settle, agent-row-linger) follow the boot that served this
 * page. A timer of one connection reads its own manifest's override instead
 * (`harnessTimingOf`), so one computer's harness timing never moves another
 * computer's connection.
 */
export function harnessInterval(timer: HarnessFrontendTimer, productMs: number): number {
  return timing.get(timer) ?? productMs;
}

/**
 * A validated manifest's frontend timer overrides. Timing without the
 * harness bit is no timing: only a harness boot may shorten a product
 * timer. An entry this build does not name, or a duration that is not a
 * positive number, is a defect in the boot that served it and throws, so
 * the manifest that carried it is refused.
 */
export function harnessTimingOf(isHarness: boolean, value: unknown): ReadonlyMap<HarnessFrontendTimer, number> {
  const next = new Map<HarnessFrontendTimer, number>();
  if (!isHarness || value === undefined) return next;
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`bootstrap harnessTiming is not an object: ${JSON.stringify(value)}`);
  }
  for (const [name, ms] of Object.entries(value)) {
    if (!FRONTEND_TIMERS.has(name)) throw new Error(`bootstrap harnessTiming names an unknown timer ${JSON.stringify(name)}`);
    if (typeof ms !== 'number' || !Number.isFinite(ms) || ms <= 0) {
      throw new Error(`bootstrap harnessTiming ${name} needs a positive duration, got ${JSON.stringify(ms)}`);
    }
    next.set(name as HarnessFrontendTimer, ms);
  }
  return next;
}

// Called by bootstrap.ts with each accepted home manifest's harness bit and
// `harnessTiming`. Every manifest replaces the page's timing: a later
// manifest's values win, and one without overrides restores the product
// timers. An owner reads its timer when it arms, so a timer already armed
// keeps the value it armed with.
export function setHarnessTimingFromBootstrap(isHarness: boolean, value: unknown): void {
  timing = harnessTimingOf(isHarness, value);
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
