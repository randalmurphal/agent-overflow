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
// "Quiet" is three facts, not two: no input here, no provider turn on
// the backend, AND no pane still draining its reveal queue. The drain
// outlives `turn_completed` by ten seconds or more, and a reader
// watching text glide in has hands off the input by definition — so
// input silence plus turn-complete is exactly the window where a GC
// stall lands mid-glide and reads as a stutter (live 1h capture
// 2026-08-26: the trim's 30-58ms stalls on a 5s wall grid, one per
// 5-6min quiet window, several mid-drain). The drain read is the same
// cheap pane-registry fold the harness drain probe uses; a draining
// pane skips the attempt WITHOUT stamping the floor, so the trim lands
// on the first check after the reader has actually seen the stream.
//
// Cost while active: four passive listeners doing one timestamp
// assignment, and one comparison every few seconds. No per-event
// allocation, no reactive state.
import { RequestWebviewMemoryTrim } from '../stores/bindings';
import { isMethodUnavailableError } from '../stores/transportStatus.svelte';
import { isViewOnlySession, runMode } from '../transport/runMode';
import { revealDrainStats } from './revealDrainProbe';

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
  // When the backend last ACCEPTED a trim from this page. The input fact
  // sent with each request — "did input land after that" — is this side's
  // half of the backend's activity gate: with no input here and no provider
  // turn there, the renderer is already at floor and the backend answers
  // "skipped-no-activity" instead of forcing a ~50ms GC stall that
  // reclaims nothing (717 of those in one overnight window, 2026-08-26).
  let lastTrimAcceptedAt = 0;
  let disarmed = false;

  const onInput = () => {
    lastInputAt = Date.now();
  };
  for (const name of INPUT_EVENTS) {
    window.addEventListener(name, onInput, { passive: true, capture: true });
  }

  const check = async () => {
    if (disarmed || document.hidden) return;
    let now = Date.now();
    if (now - lastInputAt < IDLE_TRIM_THRESHOLD_MS) return;
    if (now - lastAttemptAt < IDLE_TRIM_REATTEMPT_MS) return;
    // A pane mid-reveal means the reader is watching motion; a GC stall
    // there is a visible stutter, not an invisible pause. Skip WITHOUT
    // stamping the floor so the next 5s check retries — the trim lands
    // on the first quiet check after the drain, not a floor later.
    const drain = await revealDrainStats().catch(() => null);
    if (drain !== null && drain.draining > 0) return;
    if (disarmed) return;
    // Re-read the clocks: input may have landed during the await, and
    // the stamp must be the send time, not the pre-await time.
    now = Date.now();
    if (now - lastInputAt < IDLE_TRIM_THRESHOLD_MS) return;
    lastAttemptAt = now;
    // Captured before the round trip so input landing DURING it stays
    // "since the last trim" for the next request. `>=` because the idle
    // threshold guarantees the accepted request's own input was seconds
    // older than the marker — a same-millisecond timestamp can only be
    // input that landed after the accept.
    const requestedAt = now;
    RequestWebviewMemoryTrim(lastInputAt >= lastTrimAcceptedAt).then(
      (outcome) => {
        if (outcome === 'requested') lastTrimAcceptedAt = requestedAt;
      },
      (err: unknown) => {
        if (isMethodUnavailableError(err)) {
          // Not the desktop webview after all (or an old backend). Final.
          disarmed = true;
          return;
        }
        // Transient (reconnect window, timeout): stay armed, the next
        // check past the reattempt floor retries.
      },
    );
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
