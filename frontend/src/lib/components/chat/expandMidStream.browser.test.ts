// bug-report-20260802T011749Z: expanding a collapsed activity run while a
// turn streamed made the pane crawl "1 px at a time" — and the glide
// prior fractional content transform resampled every glyph on screen for the
// duration (the reported "text flicker on expand"). Mechanism: the
// transaction's claim placed the bottom on pre-measurement geometry, its
// pause released at tick(), and the release repin's yield handed the
// still-measuring expand delta to the engaged live-content spring, whose
// first tick killed the pending index-scroll convergence — so the CLICKED
// delta glided at tail speed for 500-2000ms. The fix holds the
// transaction's pause across the measurement flush
// (timelineWindowAnchor.svelte.ts `waitForMeasurementFlush`), so the
// convergence pass places the clicked delta instantly and the spring only
// ever owns genuinely streamed growth.
//
// This drives the incident shape end to end — real timeline, real engine,
// real wire appends/deltas, the production toggle transaction — and holds
// it to the documented contract: "the clicked delta never animates."
// Streamed chunks landing around the toggle may still glide (that is the
// spring's job), so the discriminator is scale: post-toggle bottom gaps
// must stay at streamed-chunk size, never at expand-delta size.
import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf } from '../../../test/helpers/browserFrames';
import {
  distanceToBottom,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  waitForQuietBottom,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../../utils/uiRenderTrace';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';

setupTimelineHarness();

afterEach(() => {
  clearUiRenderTrace();
  setUiRenderTraceEnabled(false);
});

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

const PROSE = {
  question: (i: number) => `Question ${i}: is the run tall enough to matter when it expands?`,
  replyLead: (i: number) =>
    `Reply ${i} carries a couple of sentences of ordinary prose so the row has a realistic height and the markdown pipeline is genuinely exercised.`,
  replyList: `- one point\n- another point\n- a third with \`inline code\``,
};

const THINKING_TEXT =
  'Measured_at timestamp advances, and while that is running I will grab an admin token and test the health endpoints. ' +
  'First I should check the compose configuration to confirm the app is running on port 8000. ' +
  'Sweep is live with 4 rows returned in 6ms and per-target retry fields null as expected. ' +
  'Now I will check the admin API health endpoints and verify the non-happy paths as well.';

function bash(id: string, turnIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Bash',
    status: 'completed',
    summary: `Bash: PG=ai-foundations podman exec $PG psql -U postgres -d app (${id})`,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function thinkingItem(id: string, turnIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'thinking',
    role: 'assistant',
    status: 'completed',
    summary: THINKING_TEXT,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function lastTraceSeq(): number {
  const records = getUiRenderTraceRecords();
  return records.length ? records[records.length - 1].seq : 0;
}

function scrollTraceSince(sinceSeq: number): string {
  return getUiRenderTraceRecords()
    .filter((r) => r.seq > sinceSeq && r.label.startsWith('scroll.'))
    .map((r) => {
      const d = r.data as Record<string, unknown>;
      const bits = [r.label];
      for (const key of ['caller', 'writeCaller', 'kind', 'delta', 'requested', 'beforeTop', 'afterTop', 'willSpring', 'willPin', 'startSpring', 'springActive']) {
        if (d && d[key] !== undefined) bits.push(`${key}=${String(d[key])}`);
      }
      return bits.join(' ');
    })
    .join('\n');
}

/** Stream prose into the tail item at wire pacing until told to stop. */
function streamTail(
  pane: ThreadPane,
  threadId: string,
  id: string,
): { done: Promise<void>; stop: () => void } {
  let stopped = false;
  const done = (async () => {
    for (let i = 0; i < 60 && !stopped; i += 1) {
      pane.applyItemDelta({
        threadId,
        itemId: id,
        kind: 'assistant_text',
        delta: ` Streamed sentence number ${i} keeps the turn alive while the reader toggles.`,
        updatedAt: 200 + i,
      });
      await sleep(60);
    }
  })();
  return { done, stop: () => { stopped = true; } };
}

describe('reader-asked expand while a turn streams', () => {
  it('the clicked expand delta places instantly — it is never handed to the spring', { timeout: 30000 }, async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-expand-mid-stream';
    const items = seedTimelineItems(THREAD_ID, PROSE);
    // A settled run with real expanded height, sitting just above the
    // streaming tail — the reader's screen in the incident.
    items.push(
      bash('x-bash-0', 100, THREAD_ID),
      thinkingItem('x-think-0', 101, THREAD_ID),
      bash('x-bash-1', 102, THREAD_ID),
      thinkingItem('x-think-1', 103, THREAD_ID),
      bash('x-bash-2', 104, THREAD_ID),
    );
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items, QUIET_BOTTOM);
    const runRow = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    expect(runRow, 'the settled activity run must exist').not.toBeNull();
    const runId = runRow.dataset.runId!;

    // The reader collapses the run (idle — this direction shrinks and was
    // never the problem), and the pane settles.
    pane.activityRuns.setCollapsed(runId, true);
    await tick();
    await waitForQuietBottom(scrollEl, 'collapse settle', QUIET_BOTTOM);
    const collapsedHeight = scrollEl.scrollHeight;

    // A live turn streams prose at the tail through the real wire path,
    // keeping the live-content program engaged across the toggle.
    pane.applyProviderItemUpserts([
      makeItem({
        id: 'x-stream',
        threadId: THREAD_ID,
        turnIndex: 110,
        itemIndex: 0,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: '',
        createdAt: 110,
        updatedAt: 110,
      }),
    ]);
    await tick();
    const stream = streamTail(pane, THREAD_ID, 'x-stream');
    // Let a few chunks land so the spring program is genuinely running.
    await sleep(200);

    const since = lastTraceSeq();
    pane.activityRuns.setCollapsed(runId, false);
    await tick();

    // Sample the bottom gap per frame across the expand and beyond. The
    // streamed chunks in flight may open chunk-scale gaps that glide
    // closed — that is correct — but the expand delta itself must never
    // appear as a gap the spring works through: with the transaction's
    // pause held across the measurement flush, the convergence pass
    // places it before the spring can see it.
    const gaps: number[] = [];
    for (let i = 0; i < 90; i += 1) {
      await raf();
      gaps.push(distanceToBottom(scrollEl));
    }
    stream.stop();
    await stream.done;

    const expanded = scrollEl.scrollHeight - collapsedHeight;
    const detail = (): string =>
      `expand delta ~${expanded.toFixed(0)}px; gaps: ${gaps.map((g) => g.toFixed(1)).join(', ')}\n--- scroll trace ---\n${scrollTraceSince(since)}`;
    expect(expanded, `the expand must add real height\n${detail()}`).toBeGreaterThan(120);

    // Chunk-scale threshold: far above any single streamed sentence's
    // height, far below the expand delta. A gap past it means the spring
    // was handed the clicked delta.
    const CHUNK_SCALE_PX = 80;
    // The first frames may catch the flush mid-measurement; from there on
    // every sample must be chunk-scale.
    const late = gaps.slice(4);
    expect(
      Math.max(...late),
      `post-toggle gaps must stay at streamed-chunk scale — an expand-delta-sized gap means the clicked delta was handed to the spring\n${detail()}`,
    ).toBeLessThanOrEqual(CHUNK_SCALE_PX);
  });
});
