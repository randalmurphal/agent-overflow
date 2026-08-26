// A shared coarse clock for relative-time labels ("2m ago"). One
// interval for the whole app; readers re-derive once a minute instead
// of each running a timer. Before the live-activity boxes landed, the
// sidebar's time labels refreshed as a SIDE EFFECT of the per-beat tree
// rebuilds; with those rebuilds cut off, this clock is what keeps an
// idle row's label creeping forward.
let minuteNow = $state(Date.now());
let armed = false;

const MINUTE_MS = 60_000;

/**
 * Current time, updated once a minute. Reading it arms the shared
 * interval on first use (app-lifetime — the sidebar is always mounted,
 * so there is nothing to tear down).
 */
export function getMinuteNow(): number {
  if (!armed && typeof window !== 'undefined') {
    armed = true;
    window.setInterval(() => {
      minuteNow = Date.now();
    }, MINUTE_MS);
  }
  return minuteNow;
}
