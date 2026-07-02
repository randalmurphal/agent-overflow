// Above-viewport compensation outcome regression — Stage 3 of
// docs/architecture/scroll-rearchitecture-plan.md.
//
// virtua compensates for above-viewport row remeasurements via
// $fixScrollJump. Those writes were arbitrated by the controller's scrollTop
// descriptor gate until Stage 3 routed them through the patched applier seam
// (resolveVirtuaCompensation) and deleted the gate. The gate's decision tiers
// encoded five shipped regression histories (bug-reports 20260524T200233Z,
// 20260524T183128Z, 20260622T041049Z, revert-to-top, seq-509 — see
// scroll-contracts.md C10). This suite pins those histories at the OUTCOME
// level — what the user sees on screen — with no controller internals and no
// mechanism spies, so the same assertions held before the applier patch,
// after it, and after the gate deletion:
//
//  1. pinned + above-viewport growth → the visible tail never moves
//     (20260524T200233Z dormant compensation, 20260524T183128Z instant-mode
//     race, and the anchor-redirect cold-switch flicker);
//  2. escaped reading + above-viewport growth → the reading anchor holds
//     (the revert-to-top / right→wrong→right desync family: suppressed
//     compensation diverges virtua's model from the DOM);
//  3. huge above-viewport correction mid-stream → snaps in one paint instead
//     of a visible ~1s spring chase (20260622T041049Z's +2276px write).
//
// (seq-509 — restore-consent — is pane/controller choreography with no
// virtua writer involved; it stays covered by the forceStick consent-gate
// unit tests in useStickToBottom.svelte.test.ts. Every mount here also
// exercises the mount-cascade compensation window: mountTimeline fails
// unless the post-cascade settle lands at the bottom.)
//
// Thresholds are calibrated from healthy runs with ~2x headroom (same method
// as streamingOutcome.browser.test.ts); healthy-run numbers are recorded
// next to each constant.
import { describe, expect, it } from 'vitest';
// Real production cascade: row margins, markdown typography, and the
// [data-row-geometry-content] flow-root rule all participate in the geometry
// under test.
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
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';

// Quiet-point distance to bottom (suite standard): the controller pins to
// the exact bottom; 2px absorbs fractional-DPR rounding without masking a
// real mis-pin, which is tens of px.
const QUIET_BOTTOM_EPSILON_PX = 2;
// Visible-anchor drift ceiling. While pinned, an above-viewport growth must
// not move the tail row on screen; while escaped, it must not move the row
// being read. Healthy runs measure 0-1px (one frame of RO-to-compensation
// gap); a suppressed/desynced compensation shifts the anchor by the whole
// growth delta (hundreds of px).
const MAX_ANCHOR_SHIFT_PX = 6;
// Unmount-burst detector: a compensation must not cost the above-viewport
// buffer. Healthy growth sheds at most a couple of rows as the window's
// absolute offsets shift; the buffer-drop failure band is ~13 rows/batch.
const MAX_REMOVED_ROWS_PER_BATCH = 4;
// Mid-stream correction recovery budget (test 3): consecutive frames the
// viewport may sit meaningfully away from the bottom after the correction
// lands. Healthy runs (3×) measure 0 frames — the correction snaps in the
// same paint; the break-test build (historical unconditional suppression)
// measures ≥24 frames of spring chase across the delta, and the shipped
// 20260622T041049Z failure was ~1s (~60 frames). 8 is absolute headroom over
// healthy jitter while sitting 3× under the broken floor.
const MAX_CONSECUTIVE_OFF_BOTTOM_FRAMES = 8;
const OFF_BOTTOM_THRESHOLD_PX = 60;

const BEAT_MS = 35;

const QUIET_BOTTOM: QuietBottomOptions = {
  epsilonPx: QUIET_BOTTOM_EPSILON_PX,
  stableFrames: 24,
  frameBudget: 480,
};

// Compensation-flavored prose over the harness's shared seed shape.
const SEED_PROSE: SeedProse = {
  question: (i) => `Question ${i}: what happens when row ${i} remeasures above the viewport while the timeline is pinned or being read?`,
  replyLead: (i) => `Reply ${i}: an above-viewport remeasure is compensated so the visible content does not move — wrapping across several visual lines at timeline width.`,
  replyList: `- above-viewport growth never moves the visible tail\n- reading anchors hold through remeasures\n- bulk corrections snap in one paint`,
};

// The growth payload appended to an above-viewport row: several paragraphs,
// tall enough (hundreds of px rendered) that a lost compensation is
// unmissable next to the anchor-shift ceiling.
const GROWTH_PARAGRAPHS = Array.from({ length: 6 }, (_, p) =>
  `Growth paragraph ${p}: this paragraph exists to make the row substantially taller when it re-renders, the way payload expansion or late typesetting grows an already-scrolled-past row in production, wrapping across several visual lines at timeline width.`,
).join('\n\n');

setupTimelineHarness();

function seedItems(threadId: string): Item[] {
  return seedTimelineItems(threadId, SEED_PROSE);
}

// Find a mounted seed row lying entirely above the viewport top (virtua's
// above-viewport buffer band) — the rows whose remeasurement triggers
// $fixScrollJump compensation. Throws if none is mounted: the scenario would
// be vacuous without real windowing.
function findAboveViewportSeedId(scrollEl: HTMLElement): string {
  const viewportTop = scrollEl.getBoundingClientRect().top;
  let best: { id: string; bottom: number } | undefined;
  for (const el of scrollEl.querySelectorAll<HTMLElement>('[data-item-id]')) {
    const rect = el.getBoundingClientRect();
    if (rect.bottom >= viewportTop - 40) continue; // not clearly above
    const id = el.dataset.itemId!;
    if (!id.startsWith('seed-')) continue;
    // Deepest above-viewport row (closest to the viewport) grows the most
    // adversarially: its growth pushes everything the user sees.
    if (!best || rect.bottom > best.bottom) best = { id, bottom: rect.bottom };
  }
  if (!best) throw new Error('no mounted above-viewport seed row — windowing/buffer regressed, scenario is vacuous');
  return best.id;
}

// Re-upsert a seed item with the growth payload appended, preserving the
// identity fields seedTimelineItems used. This is the production shape of an
// above-viewport row growing (payload expansion, late typesetting).
function growSeedRow(pane: ThreadPane, threadId: string, seedId: string, updatedAt: number): void {
  const i = Number(seedId.slice('seed-'.length));
  const isUser = i % 2 === 0;
  const base = isUser ? SEED_PROSE.question(i) : SEED_PROSE.replyLead(i);
  pane.upsertItem(makeItem({
    id: seedId,
    threadId,
    turnIndex: i,
    itemIndex: 0,
    kind: isUser ? 'user_text' : 'assistant_text',
    role: isUser ? 'user' : 'assistant',
    status: 'completed',
    summary: `${base}\n\n${GROWTH_PARAGRAPHS}`,
    createdAt: i,
    updatedAt,
  }));
}

function rectTop(scrollEl: HTMLElement, itemId: string): number {
  const el = scrollEl.querySelector<HTMLElement>(`[data-item-id="${itemId}"]`);
  if (!el) throw new Error(`row ${itemId} not mounted`);
  return el.getBoundingClientRect().top;
}

// Emulated user scroll (same shape as remountReturn.browser.test.ts): the
// wheel event carries the escape intent, the unmarked stepped writes are the
// user motion.
async function userScrollTo(scrollEl: HTMLElement, targetTop: number, steps = 8): Promise<void> {
  const start = scrollEl.scrollTop;
  const deltaY = targetTop < start ? -120 : 120;
  for (let i = 1; i <= steps; i++) {
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
    scrollEl.scrollTop = start + ((targetTop - start) * i) / steps;
    await raf();
    await raf();
  }
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

describe('above-viewport compensation outcomes (real MessageTimeline × real virtua × Chromium)', () => {
  it('pinned: an above-viewport row growing never moves the visible tail', async () => {
    const threadId = 'thread-comp-pinned';
    const { pane, scrollEl, entry } = await mountTimeline(threadId, seedItems(threadId), QUIET_BOTTOM);

    const growId = findAboveViewportSeedId(scrollEl);
    const tailId = `seed-${SEED_COUNT - 1}`;
    const tailTopBefore = rectTop(scrollEl, tailId);
    const heightBefore = scrollEl.scrollHeight;

    let maxTailShift = 0;
    let maxRemovedBatch = 0;
    const removedRows = observeRemovedRows(scrollEl, (batch) => {
      if (batch > maxRemovedBatch) maxRemovedBatch = batch;
    });
    let running = true;
    const sampler = (async () => {
      while (running) {
        await raf();
        const el = scrollEl.querySelector<HTMLElement>(`[data-item-id="${tailId}"]`);
        if (el) {
          const shift = Math.abs(el.getBoundingClientRect().top - tailTopBefore);
          if (shift > maxTailShift) maxTailShift = shift;
        }
        removedRows.flush();
      }
    })();
    entry.stop = async () => {
      running = false;
      await sampler;
      removedRows.end();
    };

    growSeedRow(pane, threadId, growId, SEED_COUNT + 1);
    await waitForQuietBottom(scrollEl, 'post-growth settle', QUIET_BOTTOM);
    await entry.stop();
    entry.stop = undefined;

    // The growth really happened — otherwise the anchor assertions pass
    // vacuously on a dead harness.
    expect(scrollEl.scrollHeight, 'growth must extend the timeline').toBeGreaterThan(heightBefore + 100);
    expect(
      maxTailShift,
      'pinned tail moved on screen during an above-viewport growth (compensation lost or mis-arbitrated)',
    ).toBeLessThanOrEqual(MAX_ANCHOR_SHIFT_PX);
    expect(
      maxRemovedBatch,
      'row unmount burst during compensation (buffer dropped)',
    ).toBeLessThanOrEqual(MAX_REMOVED_ROWS_PER_BATCH);
    expect(distanceToBottom(scrollEl), 'must rest at the bottom').toBeLessThanOrEqual(QUIET_BOTTOM_EPSILON_PX);
  });

  it('escaped: an above-viewport row growing holds the reading anchor', async () => {
    const threadId = 'thread-comp-escaped';
    const { pane, scrollEl, entry } = await mountTimeline(threadId, seedItems(threadId), QUIET_BOTTOM);

    // Read mid-thread: far enough from the bottom that a re-pin would be
    // unmistakable, far enough from the top that an above-viewport buffer
    // band exists.
    await userScrollTo(scrollEl, Math.floor(scrollEl.scrollHeight / 2));
    // Let virtua's scrollend debounce clear so the growth window starts from
    // a neutral store.
    await wait(400);

    const growId = findAboveViewportSeedId(scrollEl);
    // Anchor on the first row whose top edge is inside the viewport — what
    // the user is actually reading.
    const viewportTop = scrollEl.getBoundingClientRect().top;
    let anchorId: string | undefined;
    for (const el of scrollEl.querySelectorAll<HTMLElement>('[data-item-id]')) {
      const rect = el.getBoundingClientRect();
      if (rect.top >= viewportTop && el.dataset.itemId!.startsWith('seed-')) {
        anchorId = el.dataset.itemId!;
        break;
      }
    }
    expect(anchorId, 'a seed row must be visible mid-thread').toBeDefined();
    const anchorTopBefore = rectTop(scrollEl, anchorId!);
    const distanceBefore = distanceToBottom(scrollEl);

    growSeedRow(pane, threadId, growId, SEED_COUNT + 1);
    // Give the RO → jump → compensation → settle pipeline a real window.
    for (let i = 0; i < 30; i++) await raf();

    const anchorShift = Math.abs(rectTop(scrollEl, anchorId!) - anchorTopBefore);
    expect(
      anchorShift,
      'reading anchor moved on screen during an above-viewport growth (compensation suppressed / model desynced)',
    ).toBeLessThanOrEqual(MAX_ANCHOR_SHIFT_PX);
    // Still reading — the growth must not have re-pinned the viewport to the
    // bottom (the seq-509 symptom shape: a stale restore slamming the reader
    // to the bottom).
    expect(
      distanceToBottom(scrollEl),
      'escaped viewport must stay away from the bottom',
    ).toBeGreaterThan(distanceBefore / 2);
  });

  it('streaming: a huge above-viewport correction snaps in one paint instead of a visible spring chase', async () => {
    const threadId = 'thread-comp-correction';
    const { pane, scrollEl, entry } = await mountTimeline(threadId, seedItems(threadId), QUIET_BOTTOM);

    // Live stream to put the controller in spring-follow.
    startTurn(pane, 'turn-corr', SEED_COUNT);
    pane.upsertItem(makeItem({
      id: 'corr-tail',
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

    // Sampler: longest run of consecutive frames meaningfully off the
    // bottom. The 20260622T041049Z failure held the viewport hundreds of px
    // short for ~60 frames while the spring chased the suppressed delta.
    let running = true;
    let offRun = 0;
    let maxOffRun = 0;
    const sampler = (async () => {
      while (running) {
        await raf();
        if (distanceToBottom(scrollEl) > OFF_BOTTOM_THRESHOLD_PX) {
          offRun += 1;
          if (offRun > maxOffRun) maxOffRun = offRun;
        } else {
          offRun = 0;
        }
      }
    })();
    entry.stop = async () => {
      running = false;
      await sampler;
    };

    // Stream a few beats, then land the bulk correction mid-stream: THREE
    // above-viewport rows growing in the same delivery, the shape of a
    // fresh-mount estimate→measure pass or a late async-typesetting reflow.
    for (let b = 0; b < 6; b++) {
      pane.applyItemDelta({
        threadId,
        itemId: 'corr-tail',
        kind: 'assistant_text',
        delta: `Beat ${b} appends another clause so the tail keeps growing. `,
        updatedAt: SEED_COUNT + 1 + b,
      });
      await wait(BEAT_MS);
    }
    const viewportTop = scrollEl.getBoundingClientRect().top;
    const aboveIds: string[] = [];
    for (const el of scrollEl.querySelectorAll<HTMLElement>('[data-item-id]')) {
      const rect = el.getBoundingClientRect();
      const id = el.dataset.itemId!;
      if (rect.bottom < viewportTop - 40 && id.startsWith('seed-')) aboveIds.push(id);
    }
    expect(aboveIds.length, 'need above-viewport rows for the correction').toBeGreaterThanOrEqual(3);
    for (const id of aboveIds.slice(-5)) {
      growSeedRow(pane, threadId, id, SEED_COUNT + 40);
    }
    // Keep streaming through the correction — the failure mode was exactly
    // this overlap (fresh corrections + an active chase).
    for (let b = 6; b < 14; b++) {
      pane.applyItemDelta({
        threadId,
        itemId: 'corr-tail',
        kind: 'assistant_text',
        delta: `Beat ${b} appends another clause so the tail keeps growing. `,
        updatedAt: SEED_COUNT + 41 + b,
      });
      await wait(BEAT_MS);
    }
    finishTurn(pane, 'turn-corr', SEED_COUNT);
    await waitForQuietBottom(scrollEl, 'post-correction settle', QUIET_BOTTOM);
    await entry.stop();
    entry.stop = undefined;

    expect(
      maxOffRun,
      `viewport hung ${maxOffRun} consecutive frames >${OFF_BOTTOM_THRESHOLD_PX}px off the bottom — the suppressed-correction spring chase (20260622T041049Z)`,
    ).toBeLessThanOrEqual(MAX_CONSECUTIVE_OFF_BOTTOM_FRAMES);
    expect(distanceToBottom(scrollEl), 'must rest at the bottom').toBeLessThanOrEqual(QUIET_BOTTOM_EPSILON_PX);
  });
});
