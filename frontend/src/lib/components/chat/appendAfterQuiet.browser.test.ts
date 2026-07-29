// Repro attempt for the reported "append after a quiet gap jumps instead of
// gliding" (2026-07-29, post-1e92c94a): a tool round-trip leaves the pane
// silent long enough for the liveness hold (500ms), the structural one-shot
// (250ms), and the spring sentinel to all lapse — then the next wire append
// lands. Reported shapes:
//
//   A. prose -> first tool call (a NEW activity run row appears),
//   B. thinking -> next bash INSIDE the live run (the screenshot case),
//   plus streaming variants that drive the REAL reveal machinery (per-item
//   smoother, reveal gate, completion patch) at wall-clock pacing, with a
//   composer-geometry observation landing beside the append the way the
//   ActivityRail's working-row resize does in the app.
//
// Test C is the boundary that actually reproduced the report: NO silence —
// the spring sits sentinel-idle under clamped thinking, the viewport regrows
// as the rail's working row leaves, and the appended row returns the target
// to the sentinel's entry value, which the stranded-oscillation recovery used
// to misread as a content restore and snap (fix: clientHeight-rebased
// sentinel entry, scroll/spring.ts).
//
// Appends go through pane.applyProviderItemUpserts — the real wire path, the
// one that arms the structural spring and stamps liveness — never the bare
// upsertItem the other suites use as a convenience.
//
// The assertion mirrors activityRunScroll.browser.test.ts's slide-glide
// contract: the opened bottom gap closes GRADUALLY (several intermediate
// samples), never in a single frame. On failure the scroll write-chokepoint
// trace (enabled per test; MODE === 'test' opens the build gate) is attached
// so the culprit write's caller tag is in the assertion message.
import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
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
  // Clear BEFORE disabling: setUiRenderTraceEnabled(false) force-flushes,
  // and the flush binding is not mocked in this suite.
  clearUiRenderTrace();
  setUiRenderTraceEnabled(false);
});

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const DRIFT_PX = 1;
// Past LIVE_CONTENT_ACTIVE_HOLD_MS (500) and the structural one-shot (250)
// with margin, so every clock has provably lapsed and the sentinel is dead.
const SILENT_GAP_MS = 800;

const PROSE = {
  question: (i: number) => `Question ${i}: does the fixture hold **enough** prose to wrap?`,
  replyLead: (i: number) =>
    `Reply ${i} carries a couple of sentences of ordinary prose so the row has a realistic height and the markdown pipeline is genuinely exercised.`,
  replyList: `- one point\n- another point\n- a third with \`inline code\``,
};

// Long enough to exceed the collapsed thinking clamp (3 lines), so the late
// drain produces NO outer-row growth — the real reason no chase (and no
// sentinel) exists when the tool call lands.
const THINKING_TEXT =
  'Measured_at timestamp advances, and while that is running I will grab an admin token and test the health endpoints. ' +
  'First I should check the compose configuration to confirm the app is running on port 8000. ' +
  'Sweep is live with 4 rows returned in 6ms and per-target retry fields null as expected. ' +
  'Now I will check the admin API health endpoints and verify the non-happy paths as well.';

function bash(id: string, turnIndex: number, threadId: string, status: Item['status'] = 'running'): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Bash',
    status,
    summary: `Bash: PG=ai-foundations podman exec $PG psql -U postgres -d app (${id})`,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function thinkingItem(
  id: string,
  turnIndex: number,
  threadId: string,
  status: Item['status'],
  summary: string,
): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'thinking',
    role: 'assistant',
    status,
    summary,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Scroll-relevant trace records since `sinceSeq`, compact enough for an
 * assertion message. */
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

/**
 * Sample the bottom gap per frame until it closes (or the frame budget runs
 * out) and assert the closing motion was a glide: gradual, with intermediate
 * positions — not one write covering the whole distance. `sinceSeq` scopes
 * the trace dump attached to a failure.
 */
async function expectGlide(scrollEl: HTMLElement, opened: number, sinceSeq: number): Promise<void> {
  const gaps: number[] = [];
  for (let i = 0; i < 120 && (i === 0 || gaps[gaps.length - 1] > DRIFT_PX); i += 1) {
    await raf();
    gaps.push(distanceToBottom(scrollEl));
  }
  const detail = (): string =>
    `opened ${opened.toFixed(1)}px; samples: ${gaps.map((g) => g.toFixed(1)).join(', ')}\n--- scroll trace ---\n${scrollTraceSince(sinceSeq)}`;
  expect(gaps[gaps.length - 1], `gap must close\n${detail()}`).toBeLessThanOrEqual(DRIFT_PX);
  const partial = gaps.filter((gap) => gap > DRIFT_PX && gap < opened - DRIFT_PX);
  expect(
    partial.length,
    `glide must pass through intermediate positions, not snap\n${detail()}`,
  ).toBeGreaterThanOrEqual(2);
}

function lastTraceSeq(): number {
  const records = getUiRenderTraceRecords();
  return records.length ? records[records.length - 1].seq : 0;
}

/** Drive a thinking row through the real streaming machinery: streaming
 * upsert, wire deltas at `chunkMs` pacing, then the settle patch whose
 * summary re-asserts the received text (the Codex/Claude completion shape). */
async function streamThinking(
  pane: ThreadPane,
  threadId: string,
  id: string,
  turnIndex: number,
  opts: { chunks?: number; chunkMs?: number } = {},
): Promise<void> {
  const chunks = opts.chunks ?? 10;
  const chunkMs = opts.chunkMs ?? 60;
  pane.applyProviderItemUpserts([thinkingItem(id, turnIndex, threadId, 'streaming', '')]);
  await tick();
  const step = Math.ceil(THINKING_TEXT.length / chunks);
  for (let i = 0; i < chunks; i += 1) {
    const delta = THINKING_TEXT.slice(i * step, (i + 1) * step);
    if (!delta) break;
    pane.applyItemDelta({
      threadId,
      itemId: id,
      kind: 'thinking',
      delta,
      updatedAt: turnIndex + i + 1,
    });
    await sleep(chunkMs);
  }
  pane.applyItemPatch({
    threadId,
    itemId: id,
    kind: 'thinking',
    patch: { status: 'completed', summary: THINKING_TEXT, updatedAt: turnIndex + chunks + 1 },
  });
}

async function waitForDrain(pane: ThreadPane, id: string): Promise<void> {
  await waitFor(
    () => !pane.isItemSmoothing(id) && pane.revealBoundary === null,
    `smoother for ${id} to drain and the reveal gate to drop`,
    900,
  );
}

describe('append after a quiet gap — the growth glides', () => {
  it('A: prose -> first tool call of a new run', async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-quiet-append-new-run';
    const { pane, scrollEl } = await mountTimeline(
      THREAD_ID,
      seedTimelineItems(THREAD_ID, PROSE),
      QUIET_BOTTOM,
    );
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    await sleep(SILENT_GAP_MS);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    const since = lastTraceSeq();
    pane.applyProviderItemUpserts([bash('quiet-a-bash', 100, THREAD_ID)]);
    await tick();

    const opened = distanceToBottom(scrollEl);
    expect(opened, 'the appended run row must open a real gap').toBeGreaterThan(8);
    await expectGlide(scrollEl, opened, since);
  });

  it('B: thinking -> next bash inside the live run', async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-quiet-append-in-run';
    const items = seedTimelineItems(THREAD_ID, PROSE);
    items.push(bash('run-bash-0', 100, THREAD_ID, 'completed'));
    items.push(thinkingItem('run-think-0', 101, THREAD_ID, 'completed', THINKING_TEXT));
    items.push(thinkingItem('run-think-1', 102, THREAD_ID, 'completed', THINKING_TEXT));
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items, QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    expect(run).not.toBeNull();
    expect(run.dataset.live).toBe('true');

    await sleep(SILENT_GAP_MS);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    const since = lastTraceSeq();
    pane.applyProviderItemUpserts([bash('run-bash-1', 103, THREAD_ID)]);
    await tick();

    const opened = distanceToBottom(scrollEl);
    expect(opened, 'the appended row must open a real gap').toBeGreaterThan(8);
    await expectGlide(scrollEl, opened, since);
  });

  it('B-streamed: thinking streams and settles, silence, then bash lands beside a composer resize', async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-quiet-append-streamed';
    const items = seedTimelineItems(THREAD_ID, PROSE);
    items.push(bash('s-bash-0', 100, THREAD_ID, 'completed'));
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items, QUIET_BOTTOM);

    // The real boundary: thinking streams through the smoother (clamped to 3
    // lines, so the late drain moves nothing), settles via the completion
    // patch, and then the tool executes silently.
    await streamThinking(pane, THREAD_ID, 's-think-0', 101);
    await waitForDrain(pane, 's-think-0');
    await sleep(SILENT_GAP_MS);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    const since = lastTraceSeq();
    pane.applyProviderItemUpserts([bash('s-bash-1', 102, THREAD_ID)]);
    // The ActivityRail's working-row resize lands beside the append in the
    // app; its RO fires after the flush. 'composer-geometry' is that seam.
    await tick();
    pane.scrollController?.observe('composer-geometry');

    const opened = distanceToBottom(scrollEl);
    expect(opened, 'the appended row must open a real gap').toBeGreaterThan(8);
    await expectGlide(scrollEl, opened, since);
  });

  it('C: viewport regrows as thinking settles, then the bash row appends onto the idle sentinel', async () => {
    // The no-silence boundary. While thinking streams clamped (no growth) the
    // spring sits SENTINEL-IDLE at the bottom, kept alive by per-frame
    // liveness stamps, holding the target it entered at. When the thinking
    // finishes, the ActivityRail's working row goes away — the scroller's
    // clientHeight grows, the bottom target drops, and the browser natively
    // clamps scrollTop down with it. If the bash row that lands next is the
    // same height as the rail row that left (both are theme constants at a
    // tool boundary), the target returns to within the sentinel's 1px entry
    // band and the stranded-oscillation recovery fires an instant snap over
    // what is really a composition change that must glide
    // (resolveContentDelivery's contentRO.oscillationSnap).
    //
    // The probe append makes the height coincidence deterministic: the first
    // bash row measures exactly what the second one will add.
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-sentinel-shrink-append';
    const items = seedTimelineItems(THREAD_ID, PROSE);
    items.push(bash('c-bash-0', 100, THREAD_ID, 'completed'));
    const { pane, scrollEl, host } = await mountTimeline(THREAD_ID, items, QUIET_BOTTOM);

    // Probe: one appended bash row, measured once settled.
    const heightBeforeProbe = scrollEl.scrollHeight;
    pane.applyProviderItemUpserts([bash('c-bash-1', 101, THREAD_ID, 'completed')]);
    await tick();
    await waitForQuietBottom(scrollEl, 'probe bash row settle', QUIET_BOTTOM);
    const rowDelta = scrollEl.scrollHeight - heightBeforeProbe;
    expect(rowDelta, 'probe row must add real height').toBeGreaterThan(8);

    // Thinking streams clamped; the spring settles into its sentinel while
    // liveness stays fresh. Then the completion patch lands and the drain ends.
    await streamThinking(pane, THREAD_ID, 'c-think-0', 102);
    await waitForDrain(pane, 'c-think-0');
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    const since = lastTraceSeq();
    // The rail's working row disappears: viewport regrows by exactly one bash
    // row, the target drops, the browser clamps scrollTop to the new bottom.
    host.style.height = `${host.getBoundingClientRect().height + rowDelta}px`;
    await raf();
    pane.scrollController?.observe('composer-geometry');
    await raf();
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    pane.applyProviderItemUpserts([bash('c-bash-2', 103, THREAD_ID)]);
    await tick();

    const opened = distanceToBottom(scrollEl);
    expect(opened, 'the appended row must open a real gap').toBeGreaterThan(8);
    await expectGlide(scrollEl, opened, since);
  });

  it('B-queued: bash arrives while the thinking reveal is still draining', async () => {
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const THREAD_ID = 'thread-quiet-append-queued';
    const items = seedTimelineItems(THREAD_ID, PROSE);
    items.push(bash('q-bash-0', 100, THREAD_ID, 'completed'));
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items, QUIET_BOTTOM);

    // Slow chunks: the smoother is still draining when the tool call lands,
    // so the reveal gate withholds the bash row and releases it at drain end
    // — the user's own hypothesis for the boundary.
    const streaming = streamThinking(pane, THREAD_ID, 'q-think-0', 101, {
      chunks: 14,
      chunkMs: 90,
    });
    await sleep(350);
    const since = lastTraceSeq();
    pane.applyProviderItemUpserts([bash('q-bash-1', 102, THREAD_ID)]);
    await streaming;
    // The release happens when the drain catches up; from there the appended
    // row must glide in. Wait for the row to actually mount first.
    await waitFor(
      () => scrollEl.querySelector('[data-item-id="q-bash-1"]') !== null,
      'withheld bash row to be released',
      900,
    );

    const opened = distanceToBottom(scrollEl);
    await expectGlide(scrollEl, Math.max(opened, 9), since);
  });
});
