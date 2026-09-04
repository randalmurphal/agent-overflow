// Per-frame continuity of a streaming thinking tail inside the tail activity
// run: across paragraph breaks, short-line text, the block's completion, and
// the rows that reveal after it. Every frame records where a glyph that
// ALREADY EXISTED last frame sits now, plus the clip's and the pane's scroll
// offsets. Three things may never happen (bug-report-20260904T184019Z):
//
//   - a visible glyph moving inside its clip by more than the line-slide
//     tracker can move it in one frame (a re-pack teleport: the slide
//     cleared on a box-growth-plus-overflow frame, or saturated under
//     short lines — tailSlide.ts);
//   - the run clip or the pane moving in ONE isolated frame (the tail
//     controller handoff snapping a glide that was still in flight —
//     ActivityRun.svelte `holdForGlide`);
//   - a completed block resting with a blank line above or below its text
//     (the box taller than the text it holds).
//
// Scenarios span both clamp regimes (a capped run at its max-height with
// the inner controller gliding, and a growing run under the cap), long
// prose and short-line lists, and a slow, a fast, and a bursty wire.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, wait } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';
import { stepSlide } from './tailSlide';
import { TAIL_CLAMP_LINES } from './tailWindow';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const LINE_PX = 19.5;
// Between two samples the slide tracker moves a glyph by exactly one drain
// step of the offset that was pending at the first sample; anything past
// that (with slack for a sub-pixel wobble and a couple of ms of clock skew
// between the sampler and the tracker) is a teleport.
const GLYPH_SLACK_PX = 3;
const GLYPH_CLOCK_SKEW_MS = 2;
const legitGlyphMove = (pendingTy: number, dtMs: number): number =>
  pendingTy - stepSlide(pendingTy, dtMs + GLYPH_CLOCK_SKEW_MS) + GLYPH_SLACK_PX;
// A scroll offset that moves once, alone, is a snap: a glide accelerates in
// over several frames and decelerates out, so its neighbours are never this
// much smaller.
const SNAP_MIN_PX = 8;
const SNAP_ISOLATION = 3;

function tool(id: string, turnIndex: number, itemIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex,
    kind: 'tool_call',
    toolName: 'Bash',
    status: 'completed',
    summary: `Bash: inspect fixture ${id} with enough text to hold a realistic row height`,
    createdAt: turnIndex * 1000 + itemIndex,
    updatedAt: turnIndex * 1000 + itemIndex,
  });
}

function thinking(id: string, turnIndex: number, itemIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex,
    kind: 'thinking',
    role: 'assistant',
    status: 'streaming',
    summary: '',
    createdAt: turnIndex * 1000 + itemIndex,
    updatedAt: turnIndex * 1000 + itemIndex,
  });
}

function prose(id: string, turnIndex: number, itemIndex: number, threadId: string, long = false): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex,
    status: 'completed',
    summary: long
      ? `Reply ${id}: a longer paragraph of closing prose so the timeline is taller than its viewport and the outer spring has real work to do while the run streams below it.`
      : `Reply ${id}: closing prose for the fixture turn.`,
    createdAt: turnIndex * 1000 + itemIndex,
    updatedAt: turnIndex * 1000 + itemIndex,
  });
}

const LONG_THINK = [
  'The user wants me to look at the trace file first and understand what the bug report contains before I start reading any code, so the right move is to check the markers and the label census.',
  'Let me think about which passes matter here. Frame health looks clean, so the renderer is not the culprit, and there are no runtime errors in the sibling log for the same window.',
  'The row resize probe measures activity run rows rather than the thinking body itself, which means it cannot see the motion the user is describing at all.',
  'So the remaining candidates are all inside the tail clamp:\n1. the slide decision\n2. the append heuristic\n3. whatever replaces the text at the settle boundary',
  'I should build a reproduction that samples the newest glyph every frame and flags any displacement that neither a slide nor a spring could have produced.',
].join('\n\n');

const SHORT_THINKS = [
  'Let me check the trace markers first.\n\nThere is exactly one.',
  'Now the label census.',
  'Two threads share this trace, so I need to separate them before reading row resizes.\n\nThe pane geometry names pane-4.',
  'Reading the clip writes next:\n- inner\n- outer\n- both',
];

const LIST_THINK =
  'Options:\n- a\n- b\n- c\n- d\n\nok\n\nok\n\nPick one:\n1. x\n2. y\n3. z\n4. w\nDone.';

function seededRandom(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 0x100000000;
  };
}

function chunk(text: string, rnd: () => number, min: number, max: number): string[] {
  const out: string[] = [];
  let i = 0;
  while (i < text.length) {
    const n = min + Math.floor(rnd() * (max - min));
    out.push(text.slice(i, i + n));
    i += n;
  }
  return out;
}

function textNodeOf(body: HTMLElement): Text | null {
  let node: Node = body;
  while (node.lastChild) node = node.lastChild;
  return node.nodeType === Node.TEXT_NODE ? (node as Text) : null;
}

function lastGlyphIndex(node: Text): number {
  const text = node.textContent ?? '';
  let i = text.length - 1;
  while (i >= 0 && /\s/.test(text[i]!)) i -= 1;
  return i;
}

function firstGlyphIndex(node: Text): number {
  const text = node.textContent ?? '';
  let i = 0;
  while (i < text.length && /\s/.test(text[i]!)) i += 1;
  return i < text.length ? i : -1;
}

function glyphRect(node: Text, i: number): DOMRect | null {
  if (i < 0 || i >= (node.textContent?.length ?? 0)) return null;
  const range = document.createRange();
  range.setStart(node, i);
  range.setEnd(node, i + 1);
  return range.getBoundingClientRect();
}

type Frame = {
  f: number;
  t: number;
  item: string;
  /** Last glyph's bottom, pane-relative. */
  y: number | null;
  idx: number;
  /** Where LAST frame's last glyph sits this frame (same index). */
  yPrevIdx: number | null;
  /** First glyph's top, pane-relative. */
  y0: number | null;
  boxTop: number | null;
  boxBottom: number | null;
  boxH: number | null;
  len: number;
  clipTop: number;
  paneTop: number;
  ty: number;
  status: string;
  marks: string[];
};

function translateY(el: Element | null): number {
  if (!el) return 0;
  const tr = getComputedStyle(el).transform;
  if (tr === 'none') return 0;
  const parts = tr.slice(tr.indexOf('(') + 1, tr.indexOf(')')).split(',');
  return Number.parseFloat(parts[5] ?? '0');
}

interface Scenario {
  name: string;
  seedTools: number;
  seedProse: number;
  wireCps: number;
  chunkMin: number;
  chunkMax: number;
  seed: number;
  thinks: string[];
  withProse: boolean;
}

async function runScenario(s: Scenario): Promise<Frame[]> {
  const threadId = `thread-tail-continuity-${s.name}`;
  const turn = 3;
  const seed: Item[] = [];
  for (let i = 0; i < s.seedProse; i += 1) seed.push(prose(`p${i}`, 1, i, threadId, true));
  for (let i = 0; i < s.seedTools; i += 1) seed.push(tool(`t${i}`, turn, i, threadId));
  let nextIndex = s.seedTools;
  const firstThink = 'th0';
  seed.push(thinking(firstThink, turn, nextIndex++, threadId));

  const { pane, scrollEl, host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);

  const frames: Frame[] = [];
  const marks: string[] = [];
  let watched = firstThink;
  let stop = false;
  let f = 0;
  const t0 = performance.now();
  const sampler = (async () => {
    while (!stop) {
      await raf();
      const clips = host.querySelectorAll<HTMLElement>('[data-testid="activity-run-clip"]');
      const clip = clips[clips.length - 1] ?? null;
      const row = scrollEl.querySelector<HTMLElement>(`[data-item-id="${watched}"]`);
      const body = row?.querySelector<HTMLElement>('[data-testid="thinking-body"]') ?? null;
      const node = body ? textNodeOf(body) : null;
      const paneRect = scrollEl.getBoundingClientRect();
      const item = pane.items.find((i) => i.id === watched);
      const idx = node ? lastGlyphIndex(node) : -1;
      const prev = frames.length ? frames[frames.length - 1]! : null;
      const prevIdx = prev && prev.item === watched ? prev.idx : -1;
      const r = (v: number | null | undefined) =>
        v === null || v === undefined ? null : Math.round((v - paneRect.top) * 10) / 10;
      const boxRect = body?.getBoundingClientRect() ?? null;
      frames.push({
        f: f++,
        t: Math.round(performance.now() - t0),
        item: watched,
        y: r(node ? glyphRect(node, idx)?.bottom : null),
        idx,
        yPrevIdx: r(node && prevIdx >= 0 ? glyphRect(node, prevIdx)?.bottom : null),
        y0: r(node ? glyphRect(node, firstGlyphIndex(node))?.top : null),
        boxTop: boxRect ? r(boxRect.top) : null,
        boxBottom: boxRect ? r(boxRect.bottom) : null,
        boxH: boxRect ? Math.round(boxRect.height * 10) / 10 : null,
        len: body?.textContent?.length ?? -1,
        clipTop: clip ? Math.round(clip.scrollTop * 10) / 10 : 0,
        paneTop: Math.round(scrollEl.scrollTop * 10) / 10,
        ty: translateY(body?.firstElementChild ?? null),
        status: item?.status ?? '?',
        marks: marks.splice(0),
      });
    }
  })();

  const rnd = seededRandom(s.seed);
  const msPerChar = 1000 / s.wireCps;
  for (let k = 0; k < s.thinks.length; k += 1) {
    const id = k === 0 ? firstThink : `th${k}`;
    if (k > 0) {
      marks.push(`${id}-start`);
      pane.applyProviderItemUpserts([thinking(id, turn, nextIndex++, threadId)]);
      await tick();
    }
    watched = id;
    for (const c of chunk(s.thinks[k]!, rnd, s.chunkMin, s.chunkMax)) {
      pane.applyItemDelta({ threadId, itemId: id, kind: 'thinking', delta: c, updatedAt: 1 });
      await wait(Math.max(4, c.length * msPerChar * (0.5 + rnd())));
    }
    marks.push(`${id}-complete`);
    pane.applyItemPatch({ threadId, itemId: id, kind: 'thinking', patch: { status: 'completed', updatedAt: 2 } });
    await tick();
    await wait(80 + rnd() * 200);
    marks.push(`tool-after-${id}`);
    pane.applyProviderItemUpserts([tool(`tA${k}`, turn, nextIndex++, threadId)]);
    await tick();
    await wait(150 + rnd() * 400);
  }
  if (s.withProse) {
    marks.push('prose-append');
    pane.applyProviderItemUpserts([prose('pZ', turn, nextIndex++, threadId)]);
    await tick();
  }
  await wait(1500);
  stop = true;
  await sampler;
  return frames;
}

function fmt(fr: Frame): string {
  return `f${fr.f} t${fr.t} ${fr.item} y=${fr.y} y0=${fr.y0} idx=${fr.idx} yPrev=${fr.yPrevIdx} box=${fr.boxTop}..${fr.boxBottom}/${fr.boxH} len=${fr.len} clip=${fr.clipTop} pane=${fr.paneTop} ty=${fr.ty.toFixed(1)} ${fr.status}${fr.marks.length ? ' MARK ' + fr.marks.join(',') : ''}`;
}

type Finding = { at: number; what: string };

function isolatedSnap(deltas: number[], i: number): boolean {
  const d = Math.abs(deltas[i] ?? 0);
  if (d < SNAP_MIN_PX) return false;
  const before = Math.abs(deltas[i - 1] ?? 0);
  const after = Math.abs(deltas[i + 1] ?? 0);
  return d > SNAP_ISOLATION * Math.max(before, after);
}

function analyse(frames: Frame[]): Finding[] {
  const findings: Finding[] = [];
  const clipDeltas = frames.map((fr, i) => (i === 0 ? 0 : fr.clipTop - frames[i - 1]!.clipTop));
  const paneDeltas = frames.map((fr, i) => (i === 0 ? 0 : fr.paneTop - frames[i - 1]!.paneTop));
  let restFrames = 0;
  for (let i = 1; i < frames.length; i += 1) {
    const a = frames[i - 1]!;
    const b = frames[i]!;
    // 1. A glyph that existed last frame moved inside its clip by more than
    //    the slide tracker can move it. Clip and pane scrolling move the
    //    glyph on screen legitimately, so they are factored out.
    if (a.item === b.item && a.y !== null && b.yPrevIdx !== null) {
      const dyContent = b.yPrevIdx - a.y + (clipDeltas[i] ?? 0) + (paneDeltas[i] ?? 0);
      if (Math.abs(dyContent) > legitGlyphMove(a.ty, b.t - a.t)) {
        findings.push({ at: i, what: `glyph teleport dy=${dyContent.toFixed(1)} (ty ${a.ty.toFixed(1)}->${b.ty.toFixed(1)}, len ${a.len}->${b.len}, box ${a.boxH}->${b.boxH})` });
      }
    }
    // 2. The clip or the pane moved once, alone: a snap, not a glide.
    if (isolatedSnap(clipDeltas, i)) findings.push({ at: i, what: `clip snap ${clipDeltas[i]!.toFixed(1)}px` });
    if (isolatedSnap(paneDeltas, i)) findings.push({ at: i, what: `pane snap ${paneDeltas[i]!.toFixed(1)}px` });
    // 3. A completed block at rest (no slide pending, geometry unchanged for
    //    a few frames) never has a blank line above or below its text.
    const atRest =
      b.status === 'completed' && b.ty === 0 && a.ty === 0 && b.len === a.len && b.boxH === a.boxH;
    restFrames = atRest ? restFrames + 1 : 0;
    if (restFrames >= 3 && b.boxBottom !== null && b.y !== null && b.boxTop !== null && b.y0 !== null && b.boxH !== null) {
      const below = b.boxBottom - b.y;
      const above = b.y0 - b.boxTop;
      const underClamp = b.boxH < TAIL_CLAMP_LINES * LINE_PX - 1;
      if (below >= LINE_PX) findings.push({ at: i, what: `blank line below the text at rest (gap ${below.toFixed(1)}px, box ${b.boxH})` });
      if (underClamp && above >= LINE_PX) findings.push({ at: i, what: `blank line above the text at rest (gap ${above.toFixed(1)}px, box ${b.boxH})` });
      if (b.boxH > TAIL_CLAMP_LINES * LINE_PX + 1) findings.push({ at: i, what: `box taller than the clamp at rest (${b.boxH})` });
    }
  }
  return findings;
}

function report(name: string, frames: Frame[], findings: Finding[]): string[] {
  const lines: string[] = [];
  for (const fr of frames) if (fr.marks.length) lines.push(`${name} ${fmt(fr)}`);
  for (const finding of findings) {
    lines.push(`${name} FINDING ${finding.what}`);
    for (let k = Math.max(0, finding.at - 2); k <= Math.min(frames.length - 1, finding.at + 1); k += 1) {
      lines.push(`${name}    ${k === finding.at ? '>>' : '  '} ${fmt(frames[k]!)}`);
    }
  }
  lines.push(`${name} frames=${frames.length} findings=${findings.length}`);
  return lines;
}

const SCENARIOS: Scenario[] = [
  { name: 'capped-lists', seedTools: 12, seedProse: 30, wireCps: 900, chunkMin: 30, chunkMax: 90, seed: 23, thinks: [LIST_THINK, LIST_THINK], withProse: false },
  { name: 'capped-lists-slow', seedTools: 12, seedProse: 30, wireCps: 200, chunkMin: 3, chunkMax: 12, seed: 29, thinks: [LIST_THINK, LIST_THINK], withProse: false },
  { name: 'capped-longthink', seedTools: 12, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 7, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
  { name: 'capped-shorts', seedTools: 12, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 11, thinks: [...SHORT_THINKS, ...SHORT_THINKS], withProse: true },
  { name: 'growing-longthink', seedTools: 2, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 13, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
  { name: 'growing-shorts', seedTools: 0, seedProse: 60, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 17, thinks: [...SHORT_THINKS, ...SHORT_THINKS], withProse: true },
  { name: 'capped-burst', seedTools: 12, seedProse: 30, wireCps: 900, chunkMin: 40, chunkMax: 160, seed: 19, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
];

describe('streaming thinking tail continuity', () => {
  for (const s of SCENARIOS) {
    it(s.name, async () => {
      const frames = await runScenario(s);
      const findings = analyse(frames);
      for (const line of report(s.name, frames, findings)) console.log(line);
      expect(findings.map((finding) => finding.what)).toEqual([]);
    }, 120_000);
  }
});
