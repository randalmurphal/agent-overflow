// Burst appends against the tail-following window: the mid-glide case.
//
// activityRunScroll.browser.test.ts proves a SETTLED window survives one
// append (hold + glide) and survives sequential appends that each finish
// gliding first. Streaming does not wait: the next tool row lands while the
// previous glide is still in flight. The 2026-08-29 report — "at the stable
// bottom, content inside the run jumped up and then animated back down as a
// new item came in" — is a burst signature: some writer moved the clip past
// where the follow wanted it, and a corrective motion brought it back.
//
// The invariant this stages: while the reader is following the bottom and
// never gestures, content inside the clip may only move UP (a glide over new
// rows) or hold still (a head-splice hold). Any frame where a watched row
// moves DOWN is a second writer fighting the follow — the "animate back
// down" half of the report. Any single frame that eats most of a row height
// is a snap — the "jump" half.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import { ACTIVITY_RUN_WINDOW_ROWS_DEFAULT as WINDOW_ROWS } from '../../utils/activityRunWindow';
import type { Item } from '../../types/models';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const RUN_ROWS = WINDOW_ROWS + 10;
// Fractional rects + DPR rounding jitter on authored writes.
const DRIFT_PX = 1.5;
const THREAD_ID = 'thread-run-burst';

function tool(id: string, index: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    threadId: THREAD_ID,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    summary: `Bash: inspect fixture ${id}`,
    createdAt: index,
    updatedAt: index,
    ...overrides,
  });
}

function items(): Item[] {
  const built: Item[] = [
    makeItem({
      id: 'p0',
      threadId: THREAD_ID,
      itemIndex: 0,
      summary: 'Prose ahead of the run so the rail has a boundary.',
      createdAt: 0,
      updatedAt: 0,
    }),
  ];
  for (let i = 0; i < RUN_ROWS; i += 1) built.push(tool(`a${i}`, i + 1));
  return built;
}

interface FrameSample {
  frame: number;
  appended: boolean;
  refTop: number;
  scrollTop: number;
  scrollHeight: number;
}

describe('activity run — burst appends keep motion one-directional', () => {
  it('a watched row never moves down while appends land mid-glide', async () => {
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    expect(run.dataset.live).toBe('true');
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    expect(run.querySelector('[data-testid="activity-run-earlier"]')).not.toBeNull();
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    // Watched row: near the pre-burst tail so it stays mounted while the
    // window head advances once per append.
    const refId = `a${RUN_ROWS - 1}`;
    const refRow = (): HTMLElement => {
      const el = clip.querySelector(`[data-item-id="${refId}"]`);
      if (!(el instanceof HTMLElement)) throw new Error(`watched row ${refId} unmounted`);
      return el;
    };
    const refTop = (): number =>
      refRow().getBoundingClientRect().top - clip.getBoundingClientRect().top;

    // Appends spaced a few frames apart — squarely inside the previous
    // append's glide (a single row's glide takes ~10+ frames here).
    const APPEND_FRAMES = new Set([4, 8, 12, 16, 20]);
    const TOTAL_FRAMES = 110;
    const samples: FrameSample[] = [];
    let appended = 0;

    for (let frame = 0; frame < TOTAL_FRAMES; frame += 1) {
      if (APPEND_FRAMES.has(frame)) {
        appended += 1;
        pane.upsertItem(tool(`a${RUN_ROWS + appended - 1}`, RUN_ROWS + appended));
      }
      await raf();
      samples.push({
        frame,
        appended: APPEND_FRAMES.has(frame),
        refTop: refTop(),
        scrollTop: clip.scrollTop,
        scrollHeight: clip.scrollHeight,
      });
    }

    const describeFrame = (s: FrameSample): string =>
      `f${s.frame}${s.appended ? '+append' : ''} refTop=${s.refTop.toFixed(1)} ` +
      `scrollTop=${s.scrollTop.toFixed(1)} scrollHeight=${s.scrollHeight}`;

    // Rows only ever move up or hold: an increase in refTop is content
    // moving DOWN under a bottom-follow the reader never touched.
    const downMoves = samples
      .slice(1)
      .filter((s, i) => s.refTop - samples[i].refTop > DRIFT_PX)
      .map((s) => describeFrame(s));
    expect(downMoves, `content moved down during follow:\n${downMoves.join('\n')}`).toEqual([]);

    // No snap: a single frame must not eat most of a row. The watched row is
    // ~30-40px tall in this cascade; half of one is already a visible jump.
    const rowHeightPx = refRow().getBoundingClientRect().height;
    const snaps = samples
      .slice(1)
      .filter((s, i) => samples[i].refTop - s.refTop > rowHeightPx * 0.8)
      .map((s) => describeFrame(s));
    expect(snaps, `single-frame jumps during follow:\n${snaps.join('\n')}`).toEqual([]);

    // Vacuity guards: every append really landed and the follow really moved
    // the content up across the burst (a frozen clip would pass both checks).
    expect(clip.querySelector(`[data-item-id="a${RUN_ROWS + appended - 1}"]`)).not.toBeNull();
    expect(samples[0].refTop - samples[samples.length - 1].refTop).toBeGreaterThan(
      rowHeightPx * 2,
    );
  });

  it('holds through the wire path with mixed heights and completion swaps', async () => {
    // The production shape, not the test door: `applyProviderItemUpserts`
    // rides the reveal gate and the streaming-apply machine. Each burst step
    // is what a real turn does — the running row COMPLETES (its status swap
    // can change its height) while the NEXT tool starts, and summaries vary
    // so the dropped head row and the appended row never cancel exactly.
    const seeded = items();
    const lastSeed = seeded[seeded.length - 1];
    lastSeed.status = 'running';
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, seeded, QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    expect(run.querySelector('[data-testid="activity-run-earlier"]')).not.toBeNull();

    const refId = `a${RUN_ROWS - 2}`;
    const refTop = (): number => {
      const el = clip.querySelector(`[data-item-id="${refId}"]`);
      if (!(el instanceof HTMLElement)) throw new Error(`watched row ${refId} unmounted`);
      return el.getBoundingClientRect().top - clip.getBoundingClientRect().top;
    };

    const longSummary = (i: number): string =>
      `Bash: a deliberately longer command line ${i} that wraps onto a second row so heights differ across the burst`;
    const APPEND_FRAMES = new Set([4, 9, 14, 19]);
    const TOTAL_FRAMES = 120;
    const samples: FrameSample[] = [];
    let appended = 0;

    for (let frame = 0; frame < TOTAL_FRAMES; frame += 1) {
      if (APPEND_FRAMES.has(frame)) {
        appended += 1;
        const runningId = appended === 1 ? `a${RUN_ROWS - 1}` : `b${appended - 1}`;
        const runningIndex = RUN_ROWS + appended - 1;
        pane.applyProviderItemUpserts([
          // The running row settles…
          tool(runningId, runningIndex, { status: 'completed' }),
          // …and the next one starts, taller than what the head drops.
          tool(`b${appended}`, runningIndex + 1, {
            status: 'running',
            summary: longSummary(appended),
          }),
        ]);
      }
      await raf();
      samples.push({
        frame,
        appended: APPEND_FRAMES.has(frame),
        refTop: refTop(),
        scrollTop: clip.scrollTop,
        scrollHeight: clip.scrollHeight,
      });
    }

    const describeFrame = (s: FrameSample): string =>
      `f${s.frame}${s.appended ? '+append' : ''} refTop=${s.refTop.toFixed(1)} ` +
      `scrollTop=${s.scrollTop.toFixed(1)} scrollHeight=${s.scrollHeight}`;

    const downMoves = samples
      .slice(1)
      .filter((s, i) => s.refTop - samples[i].refTop > DRIFT_PX)
      .map((s) => describeFrame(s));
    expect(downMoves, `content moved down during follow:\n${downMoves.join('\n')}`).toEqual([]);

    // Vacuity guard: the appended rows really arrived through the wire path.
    expect(clip.querySelector(`[data-item-id="b${appended}"]`)).not.toBeNull();
  });
});
