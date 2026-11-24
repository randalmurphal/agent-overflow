// Runs a browser test's streaming reveal at a 60 Hz display's pace without
// waiting for its frames. Its own module, because it reaches the thread store:
// a test that only steps frames (browserFrames.ts) does not load it.
import { flushSync } from 'svelte';
import { onTestFinished } from 'vitest';
import type { SmoothingClock } from '../../lib/markdown/smoothing/PerItemSmoother';
import { __setSmoothingClockForTest } from '../../lib/stores/thread.svelte';

// One frame of a 60 Hz display.
const DISPLAY_FRAME_MS = 1000 / 60;

/**
 * Runs this test's streaming reveal as fast as the page can take it, with
 * the pace of a 60 Hz display: each smoother tick runs on the next task
 * instead of the next animation frame, and its clock moves one 60 Hz frame
 * per tick. Each tick reveals what a 60 Hz frame earns, so the reveal
 * takes the same path of appends as on a display, without waiting out
 * the display's time. `onTick` listeners run after each tick has rendered,
 * so a test can check every state the reveal passes through. Call before
 * the pane mounts; the clock is removed when the test finishes.
 */
export function revealWithoutWaitingForFrames(): { onTick(listener: () => void): void } {
  const listeners: (() => void)[] = [];
  let now = performance.now();
  let nextHandle = 1;
  const pending = new Map<number, (frameTimestamp?: number) => void>();
  const channel = new MessageChannel();
  channel.port1.onmessage = (event: MessageEvent<number>) => {
    const callback = pending.get(event.data);
    if (callback === undefined) return;
    pending.delete(event.data);
    now += DISPLAY_FRAME_MS;
    callback(now);
    if (listeners.length === 0) return;
    flushSync();
    for (const listener of listeners) listener();
  };
  const clock: SmoothingClock = {
    now: () => now,
    schedule: (callback) => {
      const handle = nextHandle++;
      pending.set(handle, callback);
      channel.port2.postMessage(handle);
      return handle;
    },
    cancel: (handle) => { pending.delete(handle); },
  };
  __setSmoothingClockForTest(clock);
  onTestFinished(() => {
    __setSmoothingClockForTest(undefined);
    channel.port1.close();
  });
  return { onTick: (listener) => { listeners.push(listener); } };
}
