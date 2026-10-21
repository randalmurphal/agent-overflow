import { describe, expect, it } from 'vitest';
import { TAIL_WINDOW_KEEP_LINES } from '../components/chat/tailWindow';
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

// The window contract one append broke, or null. Checked with plain
// comparisons: a feed makes tens of thousands of appends, and an `expect`
// per check per append costs seconds.
function contractBreak(w: TextWindow, stream: string, previousEnd: number, end: number): string | null {
  if (w.start + w.text.length !== end) return `window ends at ${w.start + w.text.length}, text at ${end}`;
  if (w.start !== 0 && stream[w.start - 1] !== '\n') return `window starts at ${w.start}, not at a line start`;
  if (previousEnd > 0 && w.start >= previousEnd) return `window starts at ${w.start}, at or past the previous end ${previousEnd}`;
  if (w.start > 0 && w.text.length < LIVE_WINDOW_KEEP_CHARS) return `window keeps ${w.text.length} characters`;
  return null;
}

// Newlines the clamp lays out: those before the text's trimmed end.
function newlinesIn(text: string): number {
  let n = 0;
  const laidOut = text.trimEnd().length;
  for (let i = text.indexOf('\n'); i >= 0 && i < laidOut; i = text.indexOf('\n', i + 1)) n += 1;
  return n;
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
    const broken = contractBreak(w, stream, previousEnd, end);
    if (broken) expect(broken).toBeNull();
    windows.push(w);
  }
  for (const w of windows.filter((_, i) => i % 97 === 0).concat(windows.at(-1)!)) {
    expect(w.text).toBe(stream.slice(w.start, w.start + w.text.length));
    if (w.start > 0) expect(newlinesIn(w.text)).toBeGreaterThanOrEqual(TAIL_WINDOW_KEEP_LINES);
  }
  return windows;
}

describe('LiveTextWindow', () => {
  it('holds the end of the text from a line start, keeping at least the keep length and the kept lines', () => {
    for (const [newlineEvery, maxDelta, seed] of [[12, 40, 1], [200, 400, 2], [3, 5, 3], [400, 300, 6]]) {
      feed(makeStream(120_000, newlineEvery, seed), maxDelta, seed);
    }
  });

  it('counts a newline toward the kept lines only once text follows it, as the clamp trims the end', () => {
    // After the token, blank lines arrive one at a time: nothing lays out
    // past the token until a word follows, so the cut stays above the lines
    // before it; counting the trailing newlines cut to the token alone, and
    // the clamp, whose own cut was above, adopted it and shrank to one row.
    const lines = 'a\n'.repeat(4088);
    const token = 'x'.repeat(LIVE_WINDOW_KEEP_CHARS + 16);
    const window = new LiveTextWindow(lines + token);
    let w = window.append('\n');
    for (let i = 0; i < 40; i += 1) w = window.append('\n');
    expect(w.start).toBe(lines.length - 2 * TAIL_WINDOW_KEEP_LINES);
    expect(w.text).toBe(`${'a\n'.repeat(TAIL_WINDOW_KEEP_LINES)}${token}${'\n'.repeat(41)}`);
    // A word lays the blank lines out, and they count: the next cut (a
    // second token takes the window over its recut length) may drop the
    // lines above the first token, as the character keep allows.
    const second = `w${'y'.repeat(LIVE_WINDOW_RECUT_CHARS - LIVE_WINDOW_KEEP_CHARS)}`;
    w = window.append(second);
    expect(w.text).toBe(`${token}${'\n'.repeat(41)}${second}`);
    expect(w.start).toBe(lines.length);
    // Trailing spaces are trimmed too.
    const spaced = new LiveTextWindow(lines + token);
    w = spaced.append(`\n${' \n'.repeat(40)}  `);
    expect(w.start).toBe(lines.length - 2 * TAIL_WINDOW_KEEP_LINES);
  });

  it('keeps TAIL_WINDOW_KEEP_LINES newlines after a cut, so an unbroken token is never all the clamp has', () => {
    // Short lines, then a token longer than the keep length: the character
    // keep alone would cut to the token, one rendered row however long,
    // and the clamp (which keeps that many lines itself) would adopt the
    // later start and shrink to it.
    const lines = 'a\n'.repeat(4088);
    const token = 'x'.repeat(LIVE_WINDOW_KEEP_CHARS + 16);
    const window = new LiveTextWindow(lines + token);
    let w = window.append('z');
    expect(w.start).toBe(lines.length - 2 * TAIL_WINDOW_KEEP_LINES);
    expect(w.text).toBe(`${'a\n'.repeat(TAIL_WINDOW_KEEP_LINES)}${token}z`);
    // The floor holds across later cuts: lines appended after the token
    // count, and the cut advances only as far as leaves that many.
    // (The last line's newline is trailing until text follows it, so the
    // window holds it and the 21 laid-out lines before it.)
    const line = `${'b'.repeat(LIVE_WINDOW_KEEP_CHARS / 4)}\n`;
    for (let i = 0; i < 30; i += 1) w = window.append(line);
    expect(newlinesIn(w.text)).toBe(TAIL_WINDOW_KEEP_LINES);
    expect(w.text).toBe(line.repeat(TAIL_WINDOW_KEEP_LINES + 1));
    expect(w.start).toBeGreaterThan(lines.length + token.length);
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
    // The break that ends it is a cut once the kept lines follow it, laid
    // out: the last line's newline is trailing until text follows it.
    const line = `${'q'.repeat(LIVE_WINDOW_KEEP_CHARS / TAIL_WINDOW_KEEP_LINES)}\n`;
    w = window.append(`\n${line.repeat(TAIL_WINDOW_KEEP_LINES)}`);
    expect(w.start).toBe(0);
    w = window.append(line);
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
    // A paragraph with no break past the recut length, then short lines:
    // once the lines a cut must keep are there, every append looks for a
    // cut, and finds none in the paragraph until the keep length has
    // passed it; each look covers only the text added since the last.
    const paragraph = 'p'.repeat(60_000);
    const stream = paragraph + 'l\n'.repeat(10_000);
    const window = new LiveTextWindow('');
    const budget = 2 * stream.length;
    const charCodeAt = String.prototype.charCodeAt;
    let reads = 0;
    String.prototype.charCodeAt = function (this: string, index: number): number {
      if (++reads > budget) throw new Error(`read more than ${budget} characters`);
      return charCodeAt.call(this, index);
    };
    let w: TextWindow = { text: '', start: 0 };
    try {
      for (let end = 20; end <= stream.length; end += 20) {
        w = window.append(stream.slice(end - 20, end));
      }
    } finally {
      String.prototype.charCodeAt = charCodeAt;
    }
    expect(reads).toBeGreaterThan(paragraph.length / 2);
    expect(w.start).toBeGreaterThan(paragraph.length);
  });
});

describe('appendCopied', () => {
  it('concatenates', () => {
    expect(appendCopied('', 'b')).toBe('b');
    expect(appendCopied('a', 'b')).toBe('ab');
  });
});
