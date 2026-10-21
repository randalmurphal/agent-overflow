// A streaming thinking row whose live window recuts (utils/liveText.ts) while
// the collapsed clamp is sliding lines up. The recut drops text far above the
// lines the clamp renders, so through the real pane, reveal smoother,
// ReasoningTailRow and TailClampedText the clamp must see an append: its
// rendered text keeps its start, the clamp keeps the rows a pending slide
// can still bring into view (TAIL_WINDOW_KEEP_LINES), and the slide in
// flight keeps draining instead of snapping to rest.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { wait, waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';
import { LIVE_WINDOW_RECUT_CHARS } from '../../utils/liveText';
import { stepSlide, transformTranslateY } from './tailSlide';
import { TAIL_WINDOW_KEEP_LINES } from './tailWindow';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const THREAD_ID = 'thread-tail-recut';
const THINKING_ID = 'think-recut';
// Sub-pixel geometry slack on a glyph's per-frame move.
const GLYPH_SLACK_PX = 3;

type Feed = {
  text: string;
  /** Characters per delta. */
  chunk: number;
  /** Milliseconds between deltas. */
  everyMs: number;
};

type Case = {
  name: string;
  stream: string;
  /** The text so far when the row arrives; by default up to a line end short of the recut. */
  seed?: string;
  /** The deltas after the seed; by default the next 800 characters, 12 at a time every 12ms. */
  feed?: Feed[];
  /** The clamp renders from one start throughout (no measured cut moves it). */
  sameStart: boolean;
};

const UNBROKEN_LINES = 'a\n'.repeat(4088);
const UNBROKEN_TOKEN = 'x'.repeat(LIVE_WINDOW_RECUT_CHARS - UNBROKEN_LINES.length - 100);
// Ten blank lines: with the word that lays them out, under the slide's cap
// (SLIDE_MAX_WINDOWS windows), so their landing slides rather than snaps.
const BLANK_RUN = '\n'.repeat(10);

const CASES: Case[] = [
  {
    // Short lines keep a slide in flight on almost every frame of the reveal.
    name: 'short lines',
    stream: Array.from({ length: 2400 }, (_, i) => `- step ${i}`).join('\n'),
    sameStart: true,
  },
  {
    // Thousand-character lines: TAIL_WINDOW_KEEP_LINES of them outrun the
    // window's keep length, so the clamp cuts at measured row starts, and
    // the window's cut must still leave it that many rows.
    name: 'long lines',
    stream: Array.from({ length: 30 }, (_, i) => `line ${i} ${'word '.repeat(200)}`.slice(0, 1000)).join('\n'),
    sameStart: false,
  },
  {
    // Short lines, then one token longer than the window's keep length
    // and a few words: an unbroken token is one rendered row however long,
    // so the window's cut has to keep lines, not just characters, or the
    // clamp (which keeps that many lines itself) adopts the later start
    // and shrinks to the token's row.
    name: 'unbroken token',
    stream: `${UNBROKEN_LINES}${UNBROKEN_TOKEN}${'x'.repeat(100)}\n${'word '.repeat(160)}`,
    seed: UNBROKEN_LINES + UNBROKEN_TOKEN,
    sameStart: false,
  },
  {
    // The token, then blank lines one delta at a time, then words: the
    // clamp trims the end, so the blank lines lay out only once a word
    // follows them. A window that counted them toward its kept lines cut
    // the lines above the token while the blank lines were still arriving,
    // and the clamp adopted that start and shrank to the rows that remained.
    name: 'unbroken token then blank lines',
    stream: `${UNBROKEN_LINES}${UNBROKEN_TOKEN}${'x'.repeat(100)}${BLANK_RUN}${'word '.repeat(160)}`,
    seed: UNBROKEN_LINES + UNBROKEN_TOKEN,
    feed: [
      { text: 'x'.repeat(100), chunk: 12, everyMs: 12 },
      { text: BLANK_RUN, chunk: 1, everyMs: 50 },
      { text: 'word '.repeat(160), chunk: 12, everyMs: 12 },
    ],
    sameStart: false,
  },
];

function feedOf(stream: string, seed: string): Feed[] {
  return [{ text: stream.slice(seed.length, seed.length + 800), chunk: 12, everyMs: 12 }];
}

// The row arrives with its text so far, up to a line end short of the recut:
// the reveal is capped at 320 chars/s, so streaming it would take over a
// minute.
function seedOf(stream: string): string {
  return stream.slice(0, stream.lastIndexOf('\n', LIVE_WINDOW_RECUT_CHARS - 200));
}

function row(overrides: Partial<Item> & Pick<Item, 'id' | 'turnIndex' | 'itemIndex'>): Item {
  const at = overrides.turnIndex * 1000 + overrides.itemIndex;
  return makeItem({ threadId: THREAD_ID, createdAt: at, updatedAt: at, ...overrides });
}

function seedItems(seed: string): Item[] {
  const items: Item[] = [];
  for (let i = 0; i < 30; i += 1) {
    items.push(row({
      id: `p${i}`,
      turnIndex: 1,
      itemIndex: i,
      summary: `Reply p${i}: a longer paragraph of closing prose so the timeline is taller than its viewport.`,
    }));
  }
  for (let i = 0; i < 4; i += 1) {
    items.push(row({
      id: `t${i}`,
      turnIndex: 2,
      itemIndex: i,
      kind: 'tool_call',
      toolName: 'Bash',
      summary: `Bash: inspect fixture t${i}`,
    }));
  }
  items.push(row({
    id: THINKING_ID,
    turnIndex: 2,
    itemIndex: 4,
    kind: 'thinking',
    status: 'streaming',
    summary: seed,
  }));
  return items;
}

function textNodeOf(body: HTMLElement): Text | null {
  let node: Node = body;
  while (node.firstChild) node = node.firstChild;
  return node.nodeType === Node.TEXT_NODE ? (node as Text) : null;
}

function glyphRect(node: Text, i: number): DOMRect | null {
  if (i < 0 || i >= node.length) return null;
  const range = document.createRange();
  range.setStart(node, i);
  range.setEnd(node, i + 1);
  return range.getBoundingClientRect();
}

// First glyph of the last rendered line. An append can pull the last word
// onto a new line, but a line's start stays put.
function lastLineStart(node: Text): number {
  const last = glyphRect(node, node.length - 1);
  if (!last) return -1;
  let lo = 0;
  let hi = node.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    const r = glyphRect(node, mid);
    if (r && r.height > 0 && r.bottom < last.bottom - 1) lo = mid + 1;
    else hi = mid;
  }
  while (lo < node.length - 1 && /\s/.test(node.data[lo]!)) lo += 1;
  return lo;
}

type Frame = {
  t: number;
  /** Start of the live window the row renders from; 0 until it recuts. */
  windowStart: number;
  /** Stream offset of the rendered text's first character. */
  renderedStart: number;
  /** Stream offset of the tracked glyph: the last rendered line's start. */
  tracked: number;
  /** Tracked glyph's bottom, relative to the clamp box. */
  y: number;
  /** Where the previous frame's tracked glyph sits in this frame. */
  yPrev: number | null;
  ty: number;
  /** Rendered rows the clamp holds, visible and above the box. */
  rows: number;
};

// A sample reads the state the previous frame painted (the sampler's rAF is
// registered before the slide's), so the move between samples i-1 and i was
// produced with the wall time between frames i-2 and i-1.
const paintedDt = (frames: Frame[], i: number): number =>
  i >= 2 ? frames[i - 1]!.t - frames[i - 2]!.t : frames[i]!.t - frames[i - 1]!.t;

describe.each(CASES)('streaming thinking tail through a live window recut ($name)', ({ stream, seed: givenSeed, feed, sameStart }) => {
  it('keeps the rendered text, its retained rows and the line slide continuous', async () => {
    const seed = givenSeed ?? seedOf(stream);
    const feeds = feed ?? feedOf(stream, seed);
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, seedItems(seed), QUIET_BOTTOM);
    const frames: Frame[] = [];
    let stop = false;
    const t0 = performance.now();
    const sampler = (async () => {
      while (!stop) {
        const frameTime = await new Promise<number>((resolve) => requestAnimationFrame(resolve));
        const body = scrollEl.querySelector<HTMLElement>(
          `[data-item-id="${THINKING_ID}"] [data-testid="thinking-body"]`,
        );
        const node = body ? textNodeOf(body) : null;
        const item = pane.items.find((candidate) => candidate.id === THINKING_ID);
        if (!body || !node || !item) continue;
        const source = pane.liveThinkingWindowForItem(THINKING_ID) ?? { text: item.summary, start: 0 };
        const renderedStart = source.start + source.text.trimEnd().length - node.length;
        const boxTop = body.getBoundingClientRect().top;
        const bottomOf = (offset: number): number | null => {
          const rect = glyphRect(node, offset - renderedStart);
          return rect ? rect.bottom - boxTop : null;
        };
        const tracked = renderedStart + lastLineStart(node);
        const inner = body.firstElementChild as HTMLElement;
        const prev = frames[frames.length - 1];
        frames.push({
          t: frameTime - t0,
          windowStart: source.start,
          renderedStart,
          tracked,
          y: bottomOf(tracked) ?? Number.NaN,
          yPrev: prev ? bottomOf(prev.tracked) : null,
          ty: transformTranslateY(getComputedStyle(inner).transform),
          rows: Math.round(inner.offsetHeight / Number.parseFloat(getComputedStyle(body).lineHeight)),
        });
      }
    })();

    let end = seed.length;
    for (const { text, chunk, everyMs } of feeds) {
      for (const delta of text.match(new RegExp(`[^]{1,${chunk}}`, 'g'))!) {
        pane.applyItemDelta({ threadId: THREAD_ID, itemId: THINKING_ID, kind: 'thinking', delta, updatedAt: 1 });
        await wait(everyMs);
      }
      end += text.length;
    }
    await waitFor(() => {
      const window = pane.liveThinkingWindowForItem(THINKING_ID);
      return window !== null && window.start + window.text.length === end;
    }, 'the reveal to catch up', 600);
    await wait(300);
    stop = true;
    await sampler;

    // The window recut while the clamp was sampled, with frames on both sides.
    const recut = frames.findIndex((frame) => frame.windowStart > 0);
    expect(recut).toBeGreaterThan(10);
    expect(frames.length - recut).toBeGreaterThan(30);
    // The recut is above the rendered text: the clamp renders the same start,
    // or its own measured cuts only ever move the start down the stream.
    if (sameStart) expect(new Set(frames.map((frame) => frame.renderedStart)).size).toBe(1);
    for (let i = 1; i < frames.length; i += 1) {
      expect(frames[i]!.renderedStart).toBeGreaterThanOrEqual(frames[i - 1]!.renderedStart);
    }
    // The window left the clamp every row a pending slide can still show.
    const starved = frames.slice(recut).filter((frame) => frame.rows < TAIL_WINDOW_KEEP_LINES);
    expect(starved.map((frame) => `t ${frame.t.toFixed(0)}: ${frame.rows} rows`)).toEqual([]);
    // A slide was in flight during the reveal.
    expect(frames.some((frame) => frame.ty > GLYPH_SLACK_PX)).toBe(true);

    // A glyph that was on screen moves no further than one slide drain step.
    const teleports: string[] = [];
    for (let i = 1; i < frames.length; i += 1) {
      const a = frames[i - 1]!;
      const b = frames[i]!;
      if (b.yPrev === null || Number.isNaN(a.y)) continue;
      const dy = b.yPrev - a.y;
      const legit = a.ty - stepSlide(a.ty, paintedDt(frames, i)) + GLYPH_SLACK_PX;
      if (Math.abs(dy) > legit) {
        teleports.push(`frame ${i}${i === recut ? ' (recut)' : ''}: dy ${dy.toFixed(1)}, ty ${a.ty.toFixed(1)} -> ${b.ty.toFixed(1)}`);
      }
    }
    expect(teleports).toEqual([]);
  });
});
