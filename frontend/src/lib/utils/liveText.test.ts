import { describe, expect, it } from 'vitest';
import {
  LIVE_WINDOW_KEEP_CHARS,
  LIVE_WINDOW_RECUT_CHARS,
  LiveTextWindow,
  appendCopied,
  type TextWindow,
} from './liveText';

function random(seed: number): (n: number) => number {
  let state = seed;
  return (n) => {
    state = (state * 1103515245 + 12345) & 0x7fffffff;
    return state % n;
  };
}

// Words with a '\n' after roughly one in `newlineEvery` of them.
function makeStream(length: number, newlineEvery: number, seed: number): string {
  const next = random(seed);
  const parts: string[] = [];
  let size = 0;
  while (size < length) {
    const word = 'w'.repeat(1 + next(10)) + (next(newlineEvery) === 0 ? '\n' : ' ');
    parts.push(word);
    size += word.length;
  }
  return parts.join('').slice(0, length);
}

// Feed `stream` to a window in deltas of 1..maxDelta characters, checking the
// window's contract after every append.
function feed(stream: string, maxDelta: number, seed: number): TextWindow[] {
  const next = random(seed);
  const window = new LiveTextWindow('');
  const windows: TextWindow[] = [];
  let end = 0;
  while (end < stream.length) {
    const previousEnd = end;
    end = Math.min(stream.length, end + 1 + next(maxDelta));
    const w = window.append(stream.slice(previousEnd, end));
    expect(w.start + w.text.length).toBe(end);
    expect(w.start === 0 || stream[w.start - 1] === '\n').toBe(true);
    if (previousEnd > 0) expect(w.start).toBeLessThan(previousEnd);
    if (w.start > 0) expect(w.text.length).toBeGreaterThanOrEqual(LIVE_WINDOW_KEEP_CHARS);
    windows.push(w);
  }
  for (const w of windows.filter((_, i) => i % 97 === 0).concat(windows.at(-1)!)) {
    expect(w.text).toBe(stream.slice(w.start, w.start + w.text.length));
  }
  return windows;
}

describe('LiveTextWindow', () => {
  it('holds the end of the text from a line start, keeping at least the keep length', () => {
    for (const [newlineEvery, maxDelta, seed] of [[12, 40, 1], [200, 400, 2], [3, 5, 3]]) {
      feed(makeStream(120_000, newlineEvery, seed), maxDelta, seed);
    }
  });

  it('stays bounded while the text has line breaks', () => {
    const windows = feed(makeStream(200_000, 12, 4), 30, 4);
    const longest = Math.max(...windows.map((w) => w.text.length));
    expect(longest).toBeLessThanOrEqual(LIVE_WINDOW_RECUT_CHARS + 30);
    expect(windows.at(-1)!.start).toBeGreaterThan(200_000 - LIVE_WINDOW_RECUT_CHARS - 30);
  });

  it('keeps a paragraph with no line break whole, and cuts at the break that ends it', () => {
    const paragraph = 'p'.repeat(LIVE_WINDOW_RECUT_CHARS * 2);
    const window = new LiveTextWindow('');
    let w = window.append(paragraph);
    w = window.append(' more');
    expect(w.start).toBe(0);
    w = window.append(`\n${'q'.repeat(LIVE_WINDOW_KEEP_CHARS)}`);
    w = window.append('q');
    expect(w.start).toBe(paragraph.length + ' more'.length + 1);
  });

  it('never starts at or past the previous end, even for one long delta', () => {
    const window = new LiveTextWindow('');
    window.append('first line');
    // The only breaks are past the previous end: the cut waits for the next
    // append rather than starting where a reader of the previous window
    // cannot tell it was appended to.
    let w = window.append(`\n${'x\n'.repeat(LIVE_WINDOW_RECUT_CHARS)}`);
    expect(w.start).toBe(0);
    w = window.append('y');
    expect(w.start).toBeGreaterThan(0);
    expect(w.text.length).toBeGreaterThanOrEqual(LIVE_WINDOW_KEEP_CHARS);
    // A break that ends the previous text would start the cut at its end.
    expect(new LiveTextWindow('first line\n').append('x'.repeat(LIVE_WINDOW_RECUT_CHARS)).start).toBe(0);
  });

  it('cuts a long seed at its first append', () => {
    const seed = makeStream(100_000, 12, 5);
    const w = new LiveTextWindow(seed).append('next');
    expect(w.start).toBeGreaterThan(0);
    expect(w.text.length).toBeLessThanOrEqual(LIVE_WINDOW_RECUT_CHARS);
    expect(w.text).toBe((seed + 'next').slice(w.start));
  });

  it('does not rescan text it found no line break in', () => {
    // A paragraph with no break past the recut length: each append looks for
    // a cut only in the text added since the last look.
    const paragraph = 'p'.repeat(60_000);
    const window = new LiveTextWindow('');
    const budget = 2 * paragraph.length;
    const charCodeAt = String.prototype.charCodeAt;
    let reads = 0;
    String.prototype.charCodeAt = function (this: string, index: number): number {
      if (++reads > budget) throw new Error(`read more than ${budget} characters`);
      return charCodeAt.call(this, index);
    };
    try {
      for (let end = 20; end <= paragraph.length; end += 20) {
        window.append(paragraph.slice(end - 20, end));
      }
    } finally {
      String.prototype.charCodeAt = charCodeAt;
    }
    expect(reads).toBeGreaterThan(0);
  });
});

describe('appendCopied', () => {
  it('concatenates', () => {
    expect(appendCopied('', 'b')).toBe('b');
    expect(appendCopied('a', 'b')).toBe('ab');
  });
});
