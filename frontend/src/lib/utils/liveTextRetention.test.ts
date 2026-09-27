import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';
import { LIVE_WINDOW_KEEP_CHARS, LIVE_WINDOW_RECUT_CHARS } from './liveText';

const liveTextUrl = pathToFileURL(resolve(import.meta.dirname, './liveText.ts')).href;
const smootherUrl = pathToFileURL(
  resolve(import.meta.dirname, '../markdown/smoothing/PerItemSmoother.ts'),
).href;

// A reveal delta of 13 or more characters (a long word, or a late frame's
// worth of text) is a slice of the smoother's received text, which the
// smoother joins into one string every few hundred wire deltas and whenever a
// reader asks for the whole text. A kept slice keeps that whole joined string
// alive, so an accumulation that kept the deltas themselves would hold a copy
// of the text per join; shorter deltas are copies, but kept one by one they
// cost several times their length. This measures what a live window and an
// appended text retain after a real smoother streams into them and is dropped.
//
// The workload runs in a bare `node --expose-gc` subprocess because the
// measurement is a whole-heap delta (see markdown/parseBlockRetention.test.ts
// for the resolve hook).
const workload = `
import nodeModule from 'node:module';
nodeModule.registerHooks({
  resolve(specifier, context, next) {
    if (specifier.startsWith('.')) {
      try { return next(specifier + '.ts', context); } catch {}
      try { return next(specifier, context); } catch {}
      return next(specifier + '/index.ts', context);
    }
    return next(specifier, context);
  },
});
const { LiveTextWindow, appendCopied } = await import(${JSON.stringify(liveTextUrl)});
const { PerItemSmoother } = await import(${JSON.stringify(smootherUrl)});
const kind = process.env.AO_LIVE_TEXT;

const TEXT_CHARS = 100_000;

// Stream TEXT_CHARS through a smoother into \`keep\`, then drop the smoother.
function stream(keep) {
  let now = 0;
  let scheduled = null;
  const clock = {
    now: () => now,
    schedule(callback) { scheduled = callback; return 1; },
    cancel() { scheduled = null; },
  };
  const smoother = new PerItemSmoother({ clock, onReveal: keep });
  // The wire runs ahead of the reveal, as it does at speed, and a reader of
  // the whole text (an expanded row, a settle) joins the received parts. The
  // path in each line is longer than a reveal step, so it is revealed whole,
  // as a slice of a joined string.
  let sent = 0;
  let line = 0;
  for (let frame = 0; sent < TEXT_CHARS || scheduled; frame++) {
    for (let burst = 0; burst < 10 && sent < TEXT_CHARS; burst++) {
      const delta = ['reasoning step ', String(line++), ' reads frontend/src/lib/utils/liveText.ts next\\n'].join('');
      smoother.appendDelta(delta);
      sent += delta.length;
    }
    if (frame % 8 === 0) smoother.getReceived();
    now += 16;
    const tick = scheduled;
    scheduled = null;
    tick?.(now);
  }
}

// The first stream compiles the code the measured ones run. Whole-heap
// deltas vary by about one stream's text between runs, so the measurement
// keeps RESULTS independent results and divides.
const RESULTS = 16;
stream(() => {});
for (let index = 0; index < 4; index++) globalThis.gc();
const before = process.memoryUsage().heapUsed;
const kept = [];
for (let result = 0; result < RESULTS; result++) {
  if (kind === 'window') {
    const window = new LiveTextWindow('');
    let latest = null;
    stream((delta) => { latest = window.append(delta); });
    kept.push(latest.text);
  } else {
    let appended = '';
    stream((delta) => { appended = appendCopied(appended, delta); });
    kept.push(appended);
  }
}
for (let index = 0; index < 4; index++) globalThis.gc();
// Read before touching process.stdout, whose first use builds the stream.
const retained = process.memoryUsage().heapUsed - before;
process.stdout.write(JSON.stringify({
  retained: Math.round(retained / RESULTS),
  chars: Math.min(...kept.map((text) => text.length)),
}));
`;

function retained(kind: 'window' | 'appended'): { retained: number; chars: number } {
  const output = execFileSync(
    process.execPath,
    ['--expose-gc', '--input-type=module', '--eval', workload],
    { encoding: 'utf8', env: { ...process.env, AO_LIVE_TEXT: kind }, timeout: 120_000 },
  );
  return JSON.parse(output) as { retained: number; chars: number };
}

// Bytes each result retains, for one-byte text (a character is a byte). The
// window may still share the string it was cut from until its next append.
describe('live text retention', () => {
  it('a live window holds only its own characters', () => {
    const { retained: bytes, chars } = retained('window');
    expect(chars).toBeGreaterThanOrEqual(LIVE_WINDOW_KEEP_CHARS);
    expect(bytes).toBeLessThan(2 * LIVE_WINDOW_RECUT_CHARS + 32 * 1024);
  }, 180_000);

  it('appended live text holds only its own characters', () => {
    const { retained: bytes, chars } = retained('appended');
    expect(chars).toBeGreaterThanOrEqual(100_000);
    expect(bytes).toBeLessThan(2 * chars + 64 * 1024);
  }, 180_000);
});
