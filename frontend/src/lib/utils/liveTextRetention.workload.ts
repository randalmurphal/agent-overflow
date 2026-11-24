// The heap workload of liveTextRetention.test.ts. It runs in a bare
// `node --expose-gc` child (src/test/helpers/nodeWorkload.ts), because the
// measurement is a whole-heap delta; the test imports this module for its
// descriptor only.
import { nodeWorkload } from '../../test/helpers/nodeWorkload';
import { appendCopied, LiveTextWindow } from './liveText';
import { PerItemSmoother } from '../markdown/smoothing/PerItemSmoother';

declare const gc: () => void;

const TEXT_CHARS = 100_000;

/** Streams TEXT_CHARS through a smoother into `keep`, then drops the smoother. */
function stream(keep: (delta: string) => void): void {
  let now = 0;
  // A holder, so the loop reads what the smoother's calls to the clock set.
  const frame: { scheduled: ((at: number) => void) | null } = { scheduled: null };
  const clock = {
    now: () => now,
    schedule(callback: (at: number) => void) { frame.scheduled = callback; return 1; },
    cancel() { frame.scheduled = null; },
  };
  const smoother = new PerItemSmoother({ clock, onReveal: keep });
  // The wire runs ahead of the reveal, as it does at speed, and a reader of
  // the whole text (an expanded row, a settle) joins the received parts. The
  // path in each line is longer than a reveal step, so it is revealed whole,
  // as a slice of a joined string.
  let sent = 0;
  let line = 0;
  for (let index = 0; sent < TEXT_CHARS || frame.scheduled; index++) {
    for (let burst = 0; burst < 10 && sent < TEXT_CHARS; burst++) {
      const delta = ['reasoning step ', String(line++), ' reads frontend/src/lib/utils/liveText.ts next\n'].join('');
      smoother.appendDelta(delta);
      sent += delta.length;
    }
    if (index % 8 === 0) smoother.getReceived();
    now += 16;
    const tick = frame.scheduled;
    frame.scheduled = null;
    tick?.(now);
  }
}

/** Heap bytes one result retains, as JSON with the shortest result's
 * length: a live window's text when AO_LIVE_TEXT is `window`, appended
 * text otherwise. */
export function measureLiveTextRetention(): string {
  const kind = process.env.AO_LIVE_TEXT;
  // The first stream compiles the code the measured ones run. Whole-heap
  // deltas vary by about one stream's text between runs, so the measurement
  // keeps RESULTS independent results and divides.
  const RESULTS = 16;
  stream(() => {});
  for (let index = 0; index < 4; index++) gc();
  const before = process.memoryUsage().heapUsed;
  const kept: string[] = [];
  for (let result = 0; result < RESULTS; result++) {
    if (kind === 'window') {
      const window = new LiveTextWindow('');
      let latest: { text: string } | null = null;
      stream((delta) => { latest = window.append(delta); });
      kept.push(latest!.text);
    } else {
      let appended = '';
      stream((delta) => { appended = appendCopied(appended, delta); });
      kept.push(appended);
    }
  }
  for (let index = 0; index < 4; index++) gc();
  // Read before touching process.stdout, whose first use builds the stream.
  const retained = process.memoryUsage().heapUsed - before;
  return JSON.stringify({
    retained: Math.round(retained / RESULTS),
    chars: Math.min(...kept.map((text) => text.length)),
  });
}

export const liveTextRetentionWorkload = nodeWorkload(import.meta.url, { measureLiveTextRetention });
