// Streaming outcome harness — Stage 0 of
// docs/architecture/scroll-rearchitecture-plan.md. Mounts the REAL
// MessageTimeline with a REAL pane in Chromium (real virtua windowing, real
// ResizeObserver timing, real fonts/layout) and drives synthetic streaming
// beats through the same seams production uses (pane.upsertItem /
// pane.applyItemDelta + turn lifecycle). Asserts user-visible OUTCOMES only —
// no controller internals, no mechanism spies — so the suite survives every
// stage of the scroll rewrite. Guards contracts C1 (at-bottom auto-follow),
// C9 (no buffer-drop remount churn from pin writes), and C16 (follow reads
// as continuous motion, never moving away from the bottom); see
// docs/architecture/scroll-contracts.md.
//
// Every threshold is relative (drops, deltas-to-bottom, unmount counts) —
// never an absolute pixel position — because glyph metrics differ across
// machines and the seed content is markdown whose rendered height is
// font-dependent.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
// Real production cascade: row margins, markdown typography, and the
// [data-row-geometry-content] flow-root rule all participate in the
// geometry under test (same coupling as rowMarginContainment.browser.test.ts).
import '../../../app.css';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { raf, wait, waitFor } from '../../../test/helpers/browserFrames';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import MessageTimeline from './MessageTimeline.svelte';

// ~44 seeded rows at 100-200px rendered height each ≈ 6-8k px of scrollback —
// comfortably past viewport (600px) + virtua's 2×1800px buffers, so real
// windowing is active and an above-viewport buffer drop has rows to drop.
const SEED_COUNT = 44;
const VIEWPORT_W_PX = 800;
const VIEWPORT_H_PX = 600;
// Real-timer beat cadence in the 30-60ms band the task targets; ~60 chars per
// beat lands near the pane smoother's ~840cps reveal ceiling, so the harness
// exercises both live reveal and the post-turn backlog drain.
const BEAT_MS = 35;

// Outcome thresholds, calibrated against healthy and deliberately-broken
// builds (marking wiring severed). Healthy runs measure: maxFrameDropPx 0,
// chipSamples 0, removedRows ≤3 total in batches of 1 (slow window shift as
// the stream grows). Broken runs measure removedRows batches of 13 (the
// whole above-viewport buffer band), 14-29 total per scenario.
//
// Quiet-point distance to bottom. The controller pins to the exact bottom;
// 2px absorbs fractional-DPR rounding (C22 territory) without masking a real
// mis-pin, which is tens of px.
const QUIET_BOTTOM_EPSILON_PX = 2;
// While pinned and growing, scrollTop must never move away from the bottom
// (C16). 1px absorbs DPR rounding on readback; the settle-flicker family
// measured 2-6px+ single-frame reversals.
const MAX_FRAME_DROP_PX = 1;
// Unmount-burst detector (C9). Healthy streaming with the virtua marking
// patch sheds at most a few rows one-at-a-time (rows leaving the back
// buffer as the window's absolute offset grows with the content). The
// broken build (unmarked pin writes → virtua drops its above-viewport
// buffer) removes a ~13-row band at once, repeatedly. 2/batch and 6/total
// sit at ~2× the measured healthy ceiling and ~5× under the failure mode.
const MAX_REMOVED_ROWS_PER_BATCH = 2;
const MAX_REMOVED_ROWS_TOTAL = 6;

interface Mounted {
  app: object;
  host: HTMLElement;
  stop?: () => Promise<void>;
}
const mounted: Mounted[] = [];

beforeEach(async () => {
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
});

afterEach(async () => {
  for (const { app, host, stop } of mounted.splice(0)) {
    await stop?.();
    unmount(app);
    host.remove();
  }
});

function distanceToBottom(el: HTMLElement): number {
  return el.scrollHeight - el.clientHeight - el.scrollTop;
}

// Markdown-ish transcript: alternating user asks and multi-paragraph
// assistant replies with lists/bold/inline code, so rendered row heights vary
// realistically and the markdown pipeline (streamdown) is genuinely in play.
function seedItems(threadId: string): Item[] {
  const items: Item[] = [];
  for (let i = 0; i < SEED_COUNT; i++) {
    const isUser = i % 2 === 0;
    let summary: string;
    if (isUser) {
      summary = `Question ${i}: how should the scroll controller behave when row ${i} grows while the user is pinned at the bottom of the timeline?`;
    } else {
      const parts = [
        `Reply ${i}: the timeline keeps following because growth below the viewport extends the scrollable range, and the controller pins the new bottom in the same frame the content paints — wrapping across several visual lines at timeline width.`,
      ];
      if (i % 3 === 0) {
        parts.push(`- growth pins same-frame with **no lag**\n- pin writes are announced via \`markProgrammaticScroll\`\n- quiet points land within a couple of px of the bottom`);
      }
      if (i % 4 === 1) {
        parts.push(`A second paragraph for reply ${i} so consecutive assistant rows do not all share one height bucket and virtua's estimate-to-measure corrections have real work to do.`);
      }
      summary = parts.join('\n\n');
    }
    items.push(makeItem({
      id: `seed-${i}`,
      threadId,
      turnIndex: i,
      itemIndex: 0,
      kind: isUser ? 'user_text' : 'assistant_text',
      role: isUser ? 'user' : 'assistant',
      status: 'completed',
      summary,
      createdAt: i,
      updatedAt: i,
    }));
  }
  return items;
}

interface Harness {
  pane: ThreadPane;
  scrollEl: HTMLElement;
  host: HTMLElement;
  // afterEach entry — tests hang their monitor teardown on `entry.stop` so a
  // failed assertion can't leak a running rAF sampler into the next test.
  entry: Mounted;
}

// Geometry-quiet AND at-bottom for `stableFrames` consecutive frames. Covers
// virtua's 150ms scrollend debounce, the smoother's backlog drain, and late
// measure corrections without a fixed sleep.
async function waitForQuietBottom(
  scrollEl: HTMLElement,
  label: string,
  stableFrames = 24,
  frameBudget = 480,
): Promise<void> {
  let stable = 0;
  let lastHeight = -1;
  for (let i = 0; i < frameBudget; i++) {
    const height = scrollEl.scrollHeight;
    if (height === lastHeight && distanceToBottom(scrollEl) <= QUIET_BOTTOM_EPSILON_PX) {
      stable += 1;
      if (stable >= stableFrames) return;
    } else {
      stable = 0;
      lastHeight = height;
    }
    await raf();
  }
  throw new Error(`timed out waiting for quiet bottom: ${label}`);
}

async function mountTimeline(threadId: string): Promise<Harness> {
  const pane = await buildPane(makeThread({ id: threadId }), seedItems(threadId));
  const host = document.createElement('div');
  // Fixed, definite size: MessageTimeline's root is `h-full`, so the host is
  // the viewport. position:fixed keeps document scrollbars out of the test.
  host.style.cssText = `position: fixed; top: 0; left: 0; width: ${VIEWPORT_W_PX}px; height: ${VIEWPORT_H_PX}px; background: #111;`;
  document.body.appendChild(host);
  const app = mount(MessageTimeline, { target: host, props: { pane } });
  const entry: Mounted = { app, host };
  mounted.push(entry);

  const scrollEl = await (async () => {
    await waitFor(
      () => host.querySelector('[data-testid="message-timeline-scroll"]') !== null,
      'scroll container to mount',
    );
    return host.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
  })();
  // Real windowing sanity: the whole point of the ssrCount seam is that this
  // project does NOT flat-render every row. If this trips, the seam regressed
  // and every outcome counter below is meaningless.
  await waitFor(
    () => scrollEl.querySelectorAll('[data-row-index]').length > 0,
    'virtua to render rows',
  );
  expect(
    scrollEl.querySelectorAll('[data-row-index]').length,
    'browser project must run real virtua windowing (ssrCount seam)',
  ).toBeLessThan(SEED_COUNT);
  // Warm-up gate: content stays visibility:hidden until the measurement
  // cascade settles; monitors must only start on the visible steady state.
  await waitFor(() => {
    const row = scrollEl.querySelector('[data-row-index]');
    return row !== null && getComputedStyle(row).visibility === 'visible';
  }, 'warm-up reveal');
  await waitForQuietBottom(scrollEl, 'initial mount settle');
  return { pane, scrollEl, host, entry };
}

interface OutcomeStats {
  frames: number;
  maxFrameDropPx: number;
  chipSamples: number;
  removedRowsTotal: number;
  maxRemovedRowsBatch: number;
}

interface OutcomeMonitors {
  stats: OutcomeStats;
  resetCounters(): void;
  stop(): Promise<void>;
}

// The outcome instruments: an rAF sampler (scrollTop reversals + chip
// sightings) and a MutationObserver counting removed [data-row-index] rows —
// the user-visible signature of the virtua buffer-drop remount churn.
function startOutcomeMonitors(scrollEl: HTMLElement, host: HTMLElement): OutcomeMonitors {
  const stats: OutcomeStats = {
    frames: 0,
    maxFrameDropPx: 0,
    chipSamples: 0,
    removedRowsTotal: 0,
    maxRemovedRowsBatch: 0,
  };
  const observer = new MutationObserver((records) => {
    let batch = 0;
    for (const record of records) {
      for (const node of record.removedNodes) {
        if (!(node instanceof Element)) continue;
        batch += node.matches('[data-row-index]')
          ? 1
          : node.querySelectorAll('[data-row-index]').length;
      }
    }
    if (batch === 0) return;
    stats.removedRowsTotal += batch;
    if (batch > stats.maxRemovedRowsBatch) stats.maxRemovedRowsBatch = batch;
  });
  observer.observe(scrollEl, { childList: true, subtree: true });

  let running = true;
  let prevTop = scrollEl.scrollTop;
  const samplerDone = (async () => {
    while (running) {
      await raf();
      if (!running) break;
      const top = scrollEl.scrollTop;
      const drop = prevTop - top;
      if (drop > stats.maxFrameDropPx) stats.maxFrameDropPx = drop;
      if (host.querySelector('[data-testid="scroll-to-bottom"]') !== null) {
        stats.chipSamples += 1;
      }
      stats.frames += 1;
      prevTop = top;
    }
  })();

  return {
    stats,
    resetCounters() {
      observer.takeRecords();
      prevTop = scrollEl.scrollTop;
      stats.frames = 0;
      stats.maxFrameDropPx = 0;
      stats.chipSamples = 0;
      stats.removedRowsTotal = 0;
      stats.maxRemovedRowsBatch = 0;
    },
    async stop() {
      running = false;
      observer.takeRecords();
      observer.disconnect();
      await samplerDone;
    },
  };
}

function startTurn(pane: ThreadPane, turnId: string, turnIndex: number): void {
  pane.setActiveTurn({ turnId, turnIndex, startedAt: Date.now() });
}

function finishTurn(pane: ThreadPane, turnId: string, turnIndex: number): void {
  pane.settleTurn({
    turnId,
    turnIndex,
    startedAt: Date.now() - 1_000,
    completedAt: Date.now(),
    stopReason: 'end_turn',
    assistantMessageId: null,
    tokenUsage: null,
    aborted: false,
    errorMessage: '',
  });
}

async function streamDeltaBeats(
  pane: ThreadPane,
  threadId: string,
  itemId: string,
  beats: number,
  firstUpdatedAt: number,
): Promise<void> {
  for (let b = 0; b < beats; b++) {
    // ~60 chars, occasionally a paragraph break, so the tail row keeps
    // wrapping onto new visual lines and periodically adds block spacing.
    const chunk = b % 6 === 5
      ? `\n\nBeat ${b} opens a fresh paragraph in the streaming tail. `
      : `Beat ${b} appends another clause so the tail keeps growing. `;
    pane.applyItemDelta({
      threadId,
      itemId,
      kind: 'assistant_text',
      delta: chunk,
      updatedAt: firstUpdatedAt + b,
    });
    await wait(BEAT_MS);
  }
}

function assertStreamOutcomes(stats: OutcomeStats, scrollEl: HTMLElement, label: string): void {
  const detail = JSON.stringify(stats);
  expect(
    stats.maxFrameDropPx,
    `${label}: pinned scrollTop moved away from the bottom (C16) — ${detail}`,
  ).toBeLessThanOrEqual(MAX_FRAME_DROP_PX);
  expect(
    stats.chipSamples,
    `${label}: scroll-to-bottom chip appeared during pinned follow (C1) — ${detail}`,
  ).toBe(0);
  expect(
    stats.maxRemovedRowsBatch,
    `${label}: row unmount burst — virtua dropped buffered rows (C9) — ${detail}`,
  ).toBeLessThanOrEqual(MAX_REMOVED_ROWS_PER_BATCH);
  expect(
    stats.removedRowsTotal,
    `${label}: rows unmounted during pinned follow (C9) — ${detail}`,
  ).toBeLessThanOrEqual(MAX_REMOVED_ROWS_TOTAL);
  expect(
    distanceToBottom(scrollEl),
    `${label}: quiet point must rest at the bottom (C1)`,
  ).toBeLessThanOrEqual(QUIET_BOTTOM_EPSILON_PX);
}

describe('streaming outcome harness (real MessageTimeline × real virtua × Chromium)', () => {
  it('follows a pinned streaming tail without reversals, unmount bursts, or the chip', async () => {
    const threadId = 'thread-stream-follow';
    const { pane, scrollEl, host, entry } = await mountTimeline(threadId);
    const monitors = startOutcomeMonitors(scrollEl, host);
    entry.stop = monitors.stop;

    const topBefore = scrollEl.scrollTop;
    const heightBefore = scrollEl.scrollHeight;
    startTurn(pane, 'turn-live', SEED_COUNT);
    pane.upsertItem(makeItem({
      id: 'live-tail',
      threadId,
      turnIndex: SEED_COUNT,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'Streaming reply begins.',
      createdAt: SEED_COUNT,
      updatedAt: SEED_COUNT,
    }));
    await raf();
    await streamDeltaBeats(pane, threadId, 'live-tail', 24, SEED_COUNT + 1);
    finishTurn(pane, 'turn-live', SEED_COUNT);
    await waitForQuietBottom(scrollEl, 'post-stream settle');
    await monitors.stop();

    // The stream really produced motion — otherwise the invariants above
    // pass vacuously on a dead harness.
    expect(scrollEl.scrollHeight, 'stream must grow the timeline').toBeGreaterThan(heightBefore + 100);
    expect(scrollEl.scrollTop, 'pinned follow must advance scrollTop').toBeGreaterThan(topBefore + 100);
    assertStreamOutcomes(monitors.stats, scrollEl, 'delta stream');
  });

  it('stays pinned through structural appends (new rows) during an active turn', async () => {
    const threadId = 'thread-stream-structural';
    const { pane, scrollEl, host, entry } = await mountTimeline(threadId);
    const monitors = startOutcomeMonitors(scrollEl, host);
    entry.stop = monitors.stop;

    const heightBefore = scrollEl.scrollHeight;
    startTurn(pane, 'turn-struct', SEED_COUNT);
    const appended = 6;
    for (let k = 0; k < appended; k++) {
      pane.upsertItem(makeItem({
        id: `struct-${k}`,
        threadId,
        turnIndex: SEED_COUNT,
        itemIndex: k,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: `Structural row ${k}: a whole new timeline row lands mid-turn, tall enough to wrap across a couple of visual lines and shift the bottom down in one step.`,
        createdAt: SEED_COUNT + k,
        updatedAt: SEED_COUNT + k,
      }));
      await wait(2 * BEAT_MS);
    }
    finishTurn(pane, 'turn-struct', SEED_COUNT);
    await waitForQuietBottom(scrollEl, 'post-append settle');
    await monitors.stop();

    expect(scrollEl.scrollHeight, 'appends must grow the timeline').toBeGreaterThan(heightBefore + 100);
    // The newest appended row is the visible tail (leaf roots carry
    // data-item-id) — the user actually sees the new content, not just a
    // longer scrollbar.
    expect(
      host.querySelector(`[data-item-id="struct-${appended - 1}"]`),
      'newest appended row must be mounted at the tail',
    ).not.toBeNull();
    assertStreamOutcomes(monitors.stats, scrollEl, 'structural append');
  });

  it('holds the bottom through a >400ms inter-beat gap and resumes following', async () => {
    const threadId = 'thread-stream-gap';
    const { pane, scrollEl, host, entry } = await mountTimeline(threadId);
    const monitors = startOutcomeMonitors(scrollEl, host);
    entry.stop = monitors.stop;

    startTurn(pane, 'turn-gap', SEED_COUNT);
    pane.upsertItem(makeItem({
      id: 'gap-tail',
      threadId,
      turnIndex: SEED_COUNT,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'Reply with a mid-stream stall.',
      createdAt: SEED_COUNT,
      updatedAt: SEED_COUNT,
    }));
    await raf();
    await streamDeltaBeats(pane, threadId, 'gap-tail', 8, SEED_COUNT + 1);
    // Let the reveal backlog drain so the gap segment measures a genuinely
    // quiet stall, then isolate the gap in its own counter window.
    await waitForQuietBottom(scrollEl, 'pre-gap settle');
    monitors.resetCounters();
    await wait(500);
    expect(
      monitors.stats.maxFrameDropPx,
      'stall gap: scrollTop must not sag away from the bottom (C16)',
    ).toBeLessThanOrEqual(MAX_FRAME_DROP_PX);
    expect(
      distanceToBottom(scrollEl),
      'stall gap: still resting at the bottom',
    ).toBeLessThanOrEqual(QUIET_BOTTOM_EPSILON_PX);

    const topAtGapEnd = scrollEl.scrollTop;
    await streamDeltaBeats(pane, threadId, 'gap-tail', 8, SEED_COUNT + 20);
    finishTurn(pane, 'turn-gap', SEED_COUNT);
    await waitForQuietBottom(scrollEl, 'post-resume settle');
    await monitors.stop();

    expect(
      scrollEl.scrollTop,
      'follow must resume after the gap (C16 bounded catch-up)',
    ).toBeGreaterThan(topAtGapEnd);
    assertStreamOutcomes(monitors.stats, scrollEl, 'gap + resume');
  });
});
