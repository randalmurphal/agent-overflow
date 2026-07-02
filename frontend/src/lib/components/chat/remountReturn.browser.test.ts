// Scroll-away/return remount outcome regression — the permanent successor to
// the Stage-1 floor capture experiment (docs/architecture/
// scroll-rearchitecture-plan.md). The experiment measured that on the stock
// build (virtua marking patch + fractional heights, NO per-row min-height
// floors) an emulated user scrolling away from the bottom and back gets:
// zero scrollHeight dips, zero scrollTop reversals on the return/parked legs,
// bounded unmount batches (plain windowing), and a clean bottom
// landing. The floor system was deleted on that evidence; this test pins
// those outcomes through the remaining rewrite stages.
//
// Built on the shared timelineBrowserHarness (src/test/helpers/): real
// MessageTimeline, real pane, real engine windowing, real Chromium layout.
// Markdown-only seed — deliberately NO mermaid, so vite's dep optimizer never
// discovers a lazy `import('mermaid')` mid-run (the cold-cache re-optimize
// reload documented by the experiment).
//
// "User scroll" = a direction-matched wheel event followed by stepped, direct
// scrollEl.scrollTop writes. The wheel event is load-bearing: it is the
// controller's ONLY escape-intent signal (bare scroll events never set
// escapedFromLock — without it the controller re-pins the viewport to the
// bottom mid-away). The writes are deliberately UNTAGGED so the controller
// classifies them as user scrolls, which is the scenario under test; a
// controller-routed jump would keep "programmatic" intent.
//
// Thresholds are calibrated from healthy runs with ~2x headroom (same method
// as streamingOutcome.browser.test.ts); the healthy-run numbers are recorded
// next to each constant.
import { describe, expect, it } from 'vitest';
// Real production cascade: row margins and the [data-row-geometry-content]
// flow-root rule participate in the geometry under test.
import '../../../app.css';
import { raf } from '../../../test/helpers/browserFrames';
import {
  SEED_COUNT,
  distanceToBottom,
  mountTimeline,
  observeRemovedRows,
  seedTimelineItems,
  setupTimelineHarness,
  waitForQuietBottom,
  waitForQuietTop,
  type FrameSettleOptions,
  type QuietBottomOptions,
  type SeedProse,
} from '../../../test/helpers/timelineBrowserHarness';

const AWAY_RETURN_CYCLES = 2;
// Post-landing observation window while just-remounted above-viewport rows
// finish settling — the shrink-perturbs-the-pin window.
const PARKED_FRAMES = 50;

// Clean bottom landing (B1 outcome): parked distance-to-bottom after the
// return leg settles. Healthy runs land at exactly 0; 2px absorbs
// fractional-DPR rounding without masking a real mis-pin (tens of px).
const QUIET_BOTTOM_EPSILON_PX = 2;
// scrollHeight dip ceiling on every phase. Healthy markdown-only runs
// measure ZERO dips (the 439px dip class needs an async-short remount such
// as a cache-defeated mermaid host); 8px also stays below one text line, so
// a regression that reintroduces even single-row remount collapse trips it.
const MAX_DIP_PX = 8;
// Single-frame scrollTop reversal ceiling while returning to / parked at the
// bottom. Healthy runs measure 0 on both legs; 2px absorbs DPR readback
// rounding. (The away leg is the user scrolling up — not asserted.)
const MAX_FRAME_DROP_PX = 2;
// Unmount-batch bounds: plain windowing sheds the off-window band as
// the viewport moves. Healthy runs (3×, identical) measure max batch 5 /
// total 39 (away) and max batch 6 / total 39 (return) per leg; 2x headroom on
// the batch bound also keeps it below the ~13-row buffer-drop failure band
// streamingOutcome calibrated against.
const MAX_REMOVED_ROWS_PER_BATCH = 12;
const MAX_REMOVED_ROWS_PER_PHASE = 80;
// Parked at the bottom nothing moves: healthy runs measure 0 removed rows.
// 2 tolerates a late single-row window adjust without admitting a burst.
const MAX_PARKED_REMOVED_ROWS = 2;

// Settle policy for this suite's quiet-point waits (bottom and top share it).
const SETTLE: FrameSettleOptions = { stableFrames: 18, frameBudget: 600 };
const QUIET_BOTTOM: QuietBottomOptions = { ...SETTLE, epsilonPx: QUIET_BOTTOM_EPSILON_PX };

// Remount-flavored prose over the harness's shared seed shape.
const SEED_PROSE: SeedProse = {
  question: (i) => `Question ${i}: how should the scroll controller behave when row ${i} remounts while the user is scrolling back to the bottom of the timeline?`,
  replyLead: (i) => `Reply ${i}: a remounted row must come back at its measured height, so the return leg reads as continuous motion — wrapping across several visual lines at timeline width.`,
  replyList: `- remounts land at measured size\n- no totalSize dip while returning\n- the landing rests within a couple of px of the bottom`,
};

setupTimelineHarness();

// ---------------------------------------------------------------------------
// Phase monitor: one rAF sampler + one removed-row counter for the whole
// test, sliced into labeled phases (away / return / parked per cycle).
// ---------------------------------------------------------------------------
interface DipEvent {
  magnitudePx: number;
  frames: number;
}

interface PhaseStats {
  label: string;
  frames: number;
  maxFrameDropPx: number;
  dips: DipEvent[];
  removedRowsTotal: number;
  maxRemovedBatch: number;
  finalBottomDistancePx: number;
}

interface PhaseMonitor {
  begin(label: string): void;
  end(): PhaseStats;
  stop(): Promise<void>;
}

function createPhaseMonitor(scrollEl: HTMLElement): PhaseMonitor {
  interface ActivePhase {
    label: string;
    frames: number;
    maxFrameDropPx: number;
    dips: DipEvent[];
    openDip: { peak: number; min: number; frames: number } | null;
    heightPeak: number;
    removedRowsTotal: number;
    maxRemovedBatch: number;
    prevTop: number;
  }
  let phase: ActivePhase | null = null;

  const removedRows = observeRemovedRows(scrollEl, (batch) => {
    if (!phase) return;
    phase.removedRowsTotal += batch;
    if (batch > phase.maxRemovedBatch) phase.maxRemovedBatch = batch;
  });

  let running = true;
  // Named function, not an IIFE: TS control-flow analysis flows INTO an IIFE
  // body, where `phase` is still narrowed to its initial `null`.
  async function runSampler(): Promise<void> {
    while (running) {
      await raf();
      if (!running || !phase) continue;
      const p = phase;
      const top = scrollEl.scrollTop;
      const height = scrollEl.scrollHeight;
      const drop = p.prevTop - top;
      if (drop > p.maxFrameDropPx) p.maxFrameDropPx = drop;
      p.prevTop = top;

      // scrollHeight dip tracking against the running peak: a dip is the
      // engine's totalSize shrinking (short remount measure) and closes when the peak
      // is regained (regrow) or the phase ends.
      if (height > p.heightPeak) {
        if (p.openDip) {
          p.dips.push({
            magnitudePx: Math.round(p.openDip.peak - p.openDip.min),
            frames: p.openDip.frames,
          });
          p.openDip = null;
        }
        p.heightPeak = height;
      } else if (height < p.heightPeak - 0.5) {
        if (!p.openDip) p.openDip = { peak: p.heightPeak, min: height, frames: 0 };
        if (height < p.openDip.min) p.openDip.min = height;
        p.openDip.frames += 1;
      } else if (p.openDip) {
        p.dips.push({
          magnitudePx: Math.round(p.openDip.peak - p.openDip.min),
          frames: p.openDip.frames,
        });
        p.openDip = null;
      }

      p.frames += 1;
    }
  }
  const samplerDone = runSampler();

  return {
    begin(label) {
      if (phase) throw new Error(`phase ${phase.label} still open when beginning ${label}`);
      removedRows.discardPending();
      phase = {
        label,
        frames: 0,
        maxFrameDropPx: 0,
        dips: [],
        openDip: null,
        heightPeak: scrollEl.scrollHeight,
        removedRowsTotal: 0,
        maxRemovedBatch: 0,
        prevTop: scrollEl.scrollTop,
      };
    },
    end(): PhaseStats {
      if (!phase) throw new Error('no phase open');
      // Drain queued-but-undelivered removal records into THIS phase before
      // closing it — after `phase = null` the counter callback would drop
      // them on its null-phase guard.
      removedRows.flush();
      const p = phase;
      phase = null;
      if (p.openDip) {
        p.dips.push({
          magnitudePx: Math.round(p.openDip.peak - p.openDip.min),
          frames: p.openDip.frames,
        });
      }
      return {
        label: p.label,
        frames: p.frames,
        maxFrameDropPx: Math.round(p.maxFrameDropPx * 100) / 100,
        dips: p.dips,
        removedRowsTotal: p.removedRowsTotal,
        maxRemovedBatch: p.maxRemovedBatch,
        finalBottomDistancePx: Math.round(distanceToBottom(scrollEl) * 100) / 100,
      };
    },
    async stop() {
      running = false;
      removedRows.end();
      await samplerDone;
    },
  };
}

// ---------------------------------------------------------------------------
// Scroll choreography
// ---------------------------------------------------------------------------
// Emulated user scroll: a direction-matched wheel event (the controller's
// escape/return-intent signal) followed by stepped, unmarked scrollTop writes
// (Chromium fires real scroll events for each). See the header for why both
// halves are load-bearing.
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

describe('scroll-away/return remount outcomes (real MessageTimeline × Chromium)', () => {
  it('returns to the bottom with no dips, no reversals, and bounded unmount batches', { timeout: 60_000 }, async () => {
    const threadId = 'thread-remount-return';
    const { scrollEl, entry } = await mountTimeline(
      threadId,
      seedTimelineItems(threadId, SEED_PROSE),
      QUIET_BOTTOM,
    );

    const monitor = createPhaseMonitor(scrollEl);
    entry.stop = monitor.stop;
    const phases: PhaseStats[] = [];
    const tailRowIndex = SEED_COUNT - 1;

    for (let cycle = 1; cycle <= AWAY_RETURN_CYCLES; cycle++) {
      monitor.begin(`c${cycle}-away`);
      await userScrollTo(scrollEl, 0);
      await waitForQuietTop(scrollEl, tailRowIndex, `cycle ${cycle} away`, SETTLE);
      phases.push(monitor.end());

      monitor.begin(`c${cycle}-return`);
      await userScrollTo(scrollEl, scrollEl.scrollHeight);
      // Landing clamp: if late remount settling extends scrollHeight under
      // us, nudge to the true bottom like a user holding the scrollbar at
      // the end, then wait for quiet.
      for (let i = 0; i < 3; i++) {
        await raf();
        if (distanceToBottom(scrollEl) > QUIET_BOTTOM_EPSILON_PX) {
          scrollEl.scrollTop = scrollEl.scrollHeight;
        }
      }
      await waitForQuietBottom(scrollEl, `cycle ${cycle} return`, QUIET_BOTTOM);
      phases.push(monitor.end());

      monitor.begin(`c${cycle}-parked`);
      for (let i = 0; i < PARKED_FRAMES; i++) await raf();
      phases.push(monitor.end());
    }

    await monitor.stop();
    entry.stop = undefined;

    for (const phase of phases) {
      const detail = `${phase.label}: ${JSON.stringify(phase)}`;
      const isAway = phase.label.endsWith('away');
      const isParked = phase.label.endsWith('parked');

      const maxDip = phase.dips.reduce((max, dip) => Math.max(max, dip.magnitudePx), 0);
      expect(
        maxDip,
        `scrollHeight dipped — a row remounted short (the collapse class the floors used to mask) — ${detail}`,
      ).toBeLessThanOrEqual(MAX_DIP_PX);

      if (!isAway) {
        expect(
          phase.maxFrameDropPx,
          `scrollTop reversed while returning to / parked at the bottom — ${detail}`,
        ).toBeLessThanOrEqual(MAX_FRAME_DROP_PX);
      }

      if (isParked) {
        expect(
          phase.removedRowsTotal,
          `rows unmounted while parked at the bottom — ${detail}`,
        ).toBeLessThanOrEqual(MAX_PARKED_REMOVED_ROWS);
        expect(
          phase.finalBottomDistancePx,
          `parked away from the bottom — ${detail}`,
        ).toBeLessThanOrEqual(QUIET_BOTTOM_EPSILON_PX);
      } else {
        expect(
          phase.maxRemovedBatch,
          `row unmount burst beyond plain windowing — ${detail}`,
        ).toBeLessThanOrEqual(MAX_REMOVED_ROWS_PER_BATCH);
        expect(
          phase.removedRowsTotal,
          `rows unmounted beyond plain windowing — ${detail}`,
        ).toBeLessThanOrEqual(MAX_REMOVED_ROWS_PER_PHASE);
      }
    }
  });
});
