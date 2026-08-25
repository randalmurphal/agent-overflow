// Renderer memory trim — the input-quiet half of the pipeline in
// app_webview_trim.go.
//
// Blink only decommits its pooled Oilpan pages on a memory-reducing GC,
// which it triggers on page-hide or OS memory pressure. An always-visible
// desktop window gets neither, so the renderer parks at its high-water
// mark for hours (measured 2026-08-25: 5 idle hours flat at ~293MB with
// ~20MB live). This module reports "the user is not interacting" to the
// backend, which is the only side that knows whether a provider turn is
// streaming; when both are true it directs the process owning the webview
// to force one memory-reducing GC (CDP HeapProfiler.collectGarbage).
//
// The threshold is short on purpose. The GC's whole cost is one ~58ms
// main-thread stall (soak A/B 2026-08-25: -25.5MB private on a 127MB
// renderer, one 58ms gap, zero LoAF; pressure signals returned nothing),
// so the trim fires in the first quiet window AFTER EVERY TURN rather
// than waiting out a coffee break — active sessions return to floor
// between turns instead of ratcheting until the user walks away. Sending
// a message is input (resets the clock) and the turn itself makes the
// backend answer "skipped", so the earliest fire is ~10s after the last
// turn completed with hands off the input. The backend's 4-minute floor
// is the pacer; the stall budget is one 58ms hit per floor interval,
// always with no turn running and no input for 10s.
//
// Cost while active: four passive listeners doing one timestamp
// assignment, and one comparison every few seconds. No per-event
// allocation, no reactive state.
import { RequestWebviewMemoryTrim } from '../stores/bindings';
import { isMethodUnavailableError } from '../stores/transportStatus.svelte';
import { isViewOnlySession, runMode } from '../transport/runMode';

/** Input silence before a trim request. A pause this long means reading,
 * not typing; a 58ms GC stall under a still hand is invisible. */
export const IDLE_TRIM_THRESHOLD_MS = 10_000;

/** Floor between two requests while idleness persists. The backend keeps
 * its own floor (webviewTrimMinInterval) below this, so pacing here is a
 * courtesy, not the guard. Kept moderate because a request during an
 * active turn is answered "skipped" and must retry once the turn ends. */
export const IDLE_TRIM_REATTEMPT_MS = 5 * 60_000;

/** How often idleness is re-evaluated. Must stay below the threshold or
 * short quiet windows are missed entirely. */
export const IDLE_TRIM_CHECK_MS = 5_000;

/** The complete "user is interacting" event set. pointermove is included
 * deliberately: a reader keeping a hand on the mouse is present, and a
 * trim under a moving cursor is exactly the hitch this gate exists to
 * avoid. All passive, all capture (nothing downstream can swallow them). */
const INPUT_EVENTS = ['pointerdown', 'pointermove', 'keydown', 'wheel'] as const;

/**
 * Install the idle detector. Local desktop sessions only: a `--connect`
 * client's or remote browser's idleness says nothing about the desktop
 * renderer, and the RPC is LocalOnly anyway — a session that still gets
 * the method refused disarms itself permanently. Returns an idempotent
 * stop that removes every listener and timer.
 */
export function startIdleMemoryTrim(): () => void {
  // A remote LAN browser boots with mode 'local' too; the Remote flag in
  // its bootstrap is what marks it. Both gates are fixed for the page's
  // lifetime, so this is an install-time decision.
  if (runMode() !== 'local' || isViewOnlySession()) return () => {};

  let lastInputAt = Date.now();
  let lastAttemptAt = 0;
  let disarmed = false;

  const onInput = () => {
    lastInputAt = Date.now();
  };
  for (const name of INPUT_EVENTS) {
    window.addEventListener(name, onInput, { passive: true, capture: true });
  }

  const check = () => {
    if (disarmed || document.hidden) return;
    const now = Date.now();
    if (now - lastInputAt < IDLE_TRIM_THRESHOLD_MS) return;
    if (now - lastAttemptAt < IDLE_TRIM_REATTEMPT_MS) return;
    lastAttemptAt = now;
    RequestWebviewMemoryTrim().catch((err: unknown) => {
      if (isMethodUnavailableError(err)) {
        // Not the desktop webview after all (or an old backend). Final.
        disarmed = true;
        return;
      }
      // Transient (reconnect window, timeout): stay armed, the next
      // check past the reattempt floor retries.
    });
  };
  const timer = setInterval(check, IDLE_TRIM_CHECK_MS);

  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    clearInterval(timer);
    for (const name of INPUT_EVENTS) {
      window.removeEventListener(name, onInput, { capture: true });
    }
  };
}
