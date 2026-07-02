// Streaming outcome harness — Stage 0 of
// docs/architecture/scroll-rearchitecture-plan.md. Mounts the REAL
// MessageTimeline with a REAL pane in Chromium (real engine windowing, real
// ResizeObserver timing, real fonts/layout) via the shared
// timelineBrowserHarness and drives synthetic streaming beats through the
// same seams production uses (pane.upsertItem / pane.applyItemDelta + turn
// lifecycle). Asserts user-visible OUTCOMES only — no controller internals,
// no mechanism spies — so the suite survives every stage of the scroll
// rewrite. Guards contracts C1 (at-bottom auto-follow), C9 (no buffer-drop
// remount churn from pin writes), and C16 (follow reads as continuous
// motion, never moving away from the bottom); see
// docs/architecture/scroll-contracts.md.
//
// Every threshold is relative (drops, deltas-to-bottom, unmount counts) —
// never an absolute pixel position — because glyph metrics differ across
// machines and the seed content is markdown whose rendered height is
// font-dependent.
import { describe, expect, it } from 'vitest';
// Real production cascade: row margins, markdown typography, and the
// [data-row-geometry-content] flow-root rule all participate in the
// geometry under test (same coupling as rowMarginContainment.browser.test.ts).
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf, wait } from '../../../test/helpers/browserFrames';
import {
  SEED_COUNT,
  distanceToBottom,
  mountTimeline,
  observeRemovedRows,
  seedTimelineItems,
  setupTimelineHarness,
  waitForQuietBottom,
  type MountedTimeline,
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';
import type { ThreadPane } from '../../stores/thread.svelte';

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
// Unmount-burst detector (C9). Thresholds calibrated in the virtua era:
// the healthy build (marking patch) shed at most a few rows one-at-a-time
// (rows leaving the back buffer as the window's absolute offset grows with
// the content), while the deliberately-broken build (unmarked pin writes →
// virtua dropping its above-viewport buffer) removed a ~13-row band at
// once, repeatedly. The bespoke engine's symmetric buffer
// (utils/virtual/window.ts) removes that failure class by construction and
// sheds rows the same one-at-a-time way — kept as a regression oracle.
// 2/batch and 6/total sit at ~2× the measured healthy ceiling and ~5×
// under the historical failure mode.
const MAX_REMOVED_ROWS_PER_BATCH = 2;
const MAX_REMOVED_ROWS_TOTAL = 6;

// Settle policy for this suite's quiet-point waits.
const QUIET_BOTTOM: QuietBottomOptions = {
  epsilonPx: QUIET_BOTTOM_EPSILON_PX,
  stableFrames: 24,
  frameBudget: 480,
};

// Streaming-flavored prose over the harness's shared seed shape.
const SEED_PROSE: SeedProse = {
  question: (i) => `Question ${i}: how should the scroll controller behave when row ${i} grows while the user is pinned at the bottom of the timeline?`,
  replyLead: (i) => `Reply ${i}: the timeline keeps following because growth below the viewport extends the scrollable range, and the controller pins the new bottom in the same frame the content paints — wrapping across several visual lines at timeline width.`,
  replyList: `- growth pins same-frame with **no lag**\n- pin writes are announced via \`markProgrammaticScroll\`\n- quiet points land within a couple of px of the bottom`,
};

setupTimelineHarness();

function mountStreamingTimeline(threadId: string): Promise<MountedTimeline> {
  return mountTimeline(threadId, seedTimelineItems(threadId, SEED_PROSE), QUIET_BOTTOM);
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
// sightings) and the harness's removed-row counter — the user-visible
// signature of the virtua buffer-drop remount churn.
function startOutcomeMonitors(scrollEl: HTMLElement, host: HTMLElement): OutcomeMonitors {
  const stats: OutcomeStats = {
    frames: 0,
    maxFrameDropPx: 0,
    chipSamples: 0,
    removedRowsTotal: 0,
    maxRemovedRowsBatch: 0,
  };
  const removedRows = observeRemovedRows(scrollEl, (batch) => {
    stats.removedRowsTotal += batch;
    if (batch > stats.maxRemovedRowsBatch) stats.maxRemovedRowsBatch = batch;
  });

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
      removedRows.discardPending();
      prevTop = scrollEl.scrollTop;
      stats.frames = 0;
      stats.maxFrameDropPx = 0;
      stats.chipSamples = 0;
      stats.removedRowsTotal = 0;
      stats.maxRemovedRowsBatch = 0;
    },
    async stop() {
      running = false;
      removedRows.end();
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
    `${label}: row unmount burst — the window dropped buffered rows (C9) — ${detail}`,
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

describe('streaming outcome harness (real MessageTimeline × real windowing × Chromium)', () => {
  it('follows a pinned streaming tail without reversals, unmount bursts, or the chip', async () => {
    const threadId = 'thread-stream-follow';
    const { pane, scrollEl, host, entry } = await mountStreamingTimeline(threadId);
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
    await waitForQuietBottom(scrollEl, 'post-stream settle', QUIET_BOTTOM);
    await monitors.stop();

    // The stream really produced motion — otherwise the invariants above
    // pass vacuously on a dead harness.
    expect(scrollEl.scrollHeight, 'stream must grow the timeline').toBeGreaterThan(heightBefore + 100);
    expect(scrollEl.scrollTop, 'pinned follow must advance scrollTop').toBeGreaterThan(topBefore + 100);
    assertStreamOutcomes(monitors.stats, scrollEl, 'delta stream');
  });

  it('stays pinned through structural appends (new rows) during an active turn', async () => {
    const threadId = 'thread-stream-structural';
    const { pane, scrollEl, host, entry } = await mountStreamingTimeline(threadId);
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
    await waitForQuietBottom(scrollEl, 'post-append settle', QUIET_BOTTOM);
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
    const { pane, scrollEl, host, entry } = await mountStreamingTimeline(threadId);
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
    await waitForQuietBottom(scrollEl, 'pre-gap settle', QUIET_BOTTOM);
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
    await waitForQuietBottom(scrollEl, 'post-resume settle', QUIET_BOTTOM);
    await monitors.stop();

    expect(
      scrollEl.scrollTop,
      'follow must resume after the gap (C16 bounded catch-up)',
    ).toBeGreaterThan(topAtGapEnd);
    assertStreamOutcomes(monitors.stats, scrollEl, 'gap + resume');
  });
});
