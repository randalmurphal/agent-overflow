// AO-TJUMP probe: per-frame continuity of a streaming thinking tail inside
// the tail activity run, across paragraph breaks, the block's completion
// and the rows that follow it. Every frame records where a glyph that
// ALREADY EXISTED last frame sits now; a single-frame displacement neither
// a slide nor a spring could produce is a jump.
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
import type { ThreadPane } from '../../stores/thread.svelte';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const JUMP_PX = 9;

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

function glyphBottom(node: Text, i: number): number | null {
  if (i < 0 || i >= (node.textContent?.length ?? 0)) return null;
  const range = document.createRange();
  range.setStart(node, i);
  range.setEnd(node, i + 1);
  return range.getBoundingClientRect().bottom;
}

type Frame = {
  f: number;
  t: number;
  item: string;
  y: number | null;
  idx: number;
  yPrevIdx: number | null;
  boxBottom: number | null;
  boxH: number | null;
  len: number;
  clipTop: number;
  clipH: number;
  paneTop: number;
  ty: string;
  status: string;
  marks: string[];
};

function translateY(el: Element | null): string {
  if (!el) return '-';
  const tr = getComputedStyle(el).transform;
  if (tr === 'none') return '0';
  const parts = tr.slice(tr.indexOf('(') + 1, tr.indexOf(')')).split(',');
  return Number.parseFloat(parts[5] ?? '0').toFixed(1);
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
  const threadId = `thread-tjump-${s.name}`;
  const turn = 3;
  const seed: Item[] = [];
  for (let i = 0; i < s.seedProse; i += 1) seed.push(prose(`p${i}`, 1, i, threadId, true));
  for (let i = 0; i < s.seedTools; i += 1) seed.push(tool(`t${i}`, turn, i, threadId));
  let nextIndex = s.seedTools;
  const firstThink = `th0`;
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
      const yAbs = node ? glyphBottom(node, idx) : null;
      const prev = frames.length ? frames[frames.length - 1]! : null;
      const prevIdx = prev && prev.item === watched ? prev.idx : -1;
      const yPrevAbs = node && prevIdx >= 0 ? glyphBottom(node, prevIdx) : null;
      const r = (v: number | null) => (v === null ? null : Math.round((v - paneRect.top) * 10) / 10);
      const boxRect = body?.getBoundingClientRect() ?? null;
      frames.push({
        f: f++,
        t: Math.round(performance.now() - t0),
        item: watched,
        y: r(yAbs),
        idx,
        yPrevIdx: r(yPrevAbs),
        boxBottom: boxRect ? r(boxRect.bottom) : null,
        boxH: boxRect ? Math.round(boxRect.height * 10) / 10 : null,
        len: body?.textContent?.length ?? -1,
        clipTop: clip ? Math.round(clip.scrollTop * 10) / 10 : -1,
        clipH: clip ? clip.scrollHeight : -1,
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
    const chunks = chunk(s.thinks[k]!, rnd, s.chunkMin, s.chunkMax);
    for (const c of chunks) {
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
  await wait(5000);
  stop = true;
  await sampler;
  return frames;
}

function fmt(fr: Frame): string {
  return `f${fr.f} t${fr.t} ${fr.item} y=${fr.y} idx=${fr.idx} yPrev=${fr.yPrevIdx} box=${fr.boxBottom}/${fr.boxH} len=${fr.len} clip=${fr.clipTop}/${fr.clipH} pane=${fr.paneTop} ty=${fr.ty} ${fr.status}${fr.marks.length ? ' MARK ' + fr.marks.join(',') : ''}`;
}

function report(name: string, frames: Frame[]): { lines: string[]; jumps: number } {
  const lines: string[] = [];
  let jumps = 0;
  for (let i = 1; i < frames.length; i += 1) {
    const a = frames[i - 1]!;
    const b = frames[i]!;
    if (b.marks.length) lines.push(`AO-TJUMP ${name} ${fmt(b)}`);
    if (a.item !== b.item || a.y === null || b.yPrevIdx === null) continue;
    const dy = b.yPrevIdx - a.y;
    if (Math.abs(dy) > JUMP_PX) {
      jumps += 1;
      const dbox = a.boxBottom !== null && b.boxBottom !== null ? b.boxBottom - a.boxBottom : 0;
      const dboxH = a.boxH !== null && b.boxH !== null ? b.boxH - a.boxH : 0;
      const dclip = b.clipTop - a.clipTop;
      const dpane = b.paneTop - a.paneTop;
      lines.push(
        `AO-TJUMP ${name} JUMP dy=${dy.toFixed(1)} boxBottom=${dbox.toFixed(1)} boxH=${dboxH.toFixed(1)} clipTop=${dclip.toFixed(1)} paneTop=${dpane.toFixed(1)} ty ${a.ty}->${b.ty} len ${a.len}->${b.len} idx ${a.idx}->${b.idx}`,
      );
      for (let k = Math.max(0, i - 2); k <= Math.min(frames.length - 1, i + 1); k += 1) {
        lines.push(`AO-TJUMP ${name}    ${k === i ? '>>' : '  '} ${fmt(frames[k]!)}`);
      }
    }
  }
  lines.push(`AO-TJUMP ${name} frames=${frames.length} jumps=${jumps}`);
  return { lines, jumps };
}

const SCENARIOS: Scenario[] = [
  { name: 'capped-longthink', seedTools: 12, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 7, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
  { name: 'capped-shorts', seedTools: 12, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 11, thinks: [...SHORT_THINKS, ...SHORT_THINKS], withProse: true },
  { name: 'growing-longthink', seedTools: 2, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 13, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
  { name: 'growing-shorts', seedTools: 0, seedProse: 30, wireCps: 250, chunkMin: 3, chunkMax: 16, seed: 17, thinks: [...SHORT_THINKS, ...SHORT_THINKS], withProse: true },
  { name: 'capped-burst', seedTools: 12, seedProse: 30, wireCps: 900, chunkMin: 40, chunkMax: 160, seed: 19, thinks: [LONG_THINK, ...SHORT_THINKS], withProse: true },
];

describe('AO-TJUMP thinking tail continuity', () => {
  for (const s of SCENARIOS) {
    it(s.name, async () => {
      const frames = await runScenario(s);
      const { lines, jumps } = report(s.name, frames);
      for (const l of lines) console.log(l);
      expect(jumps).toBe(0);
    }, 120_000);
  }
});
