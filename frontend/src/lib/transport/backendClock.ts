// What time it is ON a backend, as this page can best tell.
//
// Every timestamp the app renders was minted by a backend's clock, and
// "5m ago" is a subtraction between that reading and this device's. The
// two clocks are not the same clock. A phone left asleep, a laptop that
// resumed before NTP caught up, a serve host in a VM with a drifting
// timer: any of them puts minutes between the two, and the subtraction
// then reads a fresh row as an hour old or renders a real one "just now"
// for as long as the drift lasts. Attaching a second machine makes it
// structural rather than occasional — two backends drift independently,
// and one list can hold rows from both.
//
// The transport already measures it. Every hello frame carries the
// server's own reading of the moment it was sent, and `wsClient` stores
// the difference as `clockSkewMs`. This module is what makes that
// measurement readable by the code that formats a timestamp, keyed by
// the backend the timestamp came from.
//
// A SOURCE per backend rather than a stored number, because the number
// moves under us: `wsClient` deliberately does not publish a hello whose
// only change is the clock reading (a reconnect would otherwise wake
// every hello consumer for a few milliseconds of drift), so a cached
// copy would be the skew from whichever hello last happened to differ in
// some other field. Reading through a closure costs one call and one
// property read, which is what lets `relativeTime` stay allocation-free
// on its hot path.
//
// An unregistered backend reads as ZERO skew, which is exactly the
// behaviour every caller had before this existed. That is the point: the
// seam corrects a reading when it can and changes nothing when it
// cannot.

import { HOME_BACKEND, type BackendKey } from './backendKey';

/** How far ahead of this device a backend's clock is, in milliseconds. */
type SkewSource = () => number;

const sources = new Map<BackendKey, SkewSource>();

/**
 * Register a backend's skew reading. ./backends.ts calls this at attach
 * and drops it at detach; nothing else should.
 */
export function registerBackendClock(backendId: BackendKey, source: SkewSource): void {
  sources.set(backendId, source);
}

/** Drop a backend's reading. A later read answers as if it never had one. */
export function forgetBackendClock(backendId: BackendKey): void {
  sources.delete(backendId);
}

/**
 * How far ahead of this device `backendId`'s clock is, in milliseconds.
 * Zero for a backend that has not said, and for one that never will.
 */
export function backendClockSkew(backendId: BackendKey = HOME_BACKEND): number {
  const source = sources.get(backendId);
  if (source === undefined) return 0;
  const skew = source();
  return Number.isFinite(skew) ? skew : 0;
}

/**
 * This device's best reading of what time it is on `backendId` right now.
 *
 * The one function timestamp formatting should call. Comparing a
 * backend's timestamp against `Date.now()` directly is the bug this
 * exists for.
 */
export function backendNow(backendId: BackendKey = HOME_BACKEND): number {
  return Date.now() + backendClockSkew(backendId);
}

/** Test seam: drop every registered reading. */
export function resetBackendClocksForTest(): void {
  sources.clear();
}
