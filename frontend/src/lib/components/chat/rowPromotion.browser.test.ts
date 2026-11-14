// A Claude narration block streams as a thinking row inside the tail
// activity run and settles as prose under the same id
// (stores/threadRowPromotions.svelte.ts). These drive that through the real
// timeline, reveal and virtualizer, sampling every painted frame:
//
//   - the thinking row keeps revealing to its last word before anything moves,
//   - the change is a frame-by-frame motion (the stage opens while the old
//     row closes), never a jump and never a CSS transition,
//   - the install lands the prose row where the stage showed it,
//   - rows queued behind the block mount only after the install.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, wait, waitFor } from '../../../test/helpers/browserFrames';
import {
  distanceToBottom,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  userScrollTo,
  waitForQuietBottom,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import { getSettings } from '../../stores/settings.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import { SPRING_MAX_VELOCITY_PX_PER_FRAME } from '../../utils/scroll/motion';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
const TURN = 100;
const NARRATION_ID = 'think-narration';
const QUEUED_ID = 'queued-bash';

const PROSE = {
  question: (i: number) => `Question ${i}: does the fixture hold **enough** prose to wrap?`,
  replyLead: (i: number) =>
    `Reply ${i} carries a couple of sentences of ordinary prose so the row has a realistic height and the markdown pipeline is genuinely exercised.`,
  replyList: `- one point\n- another point\n- a third with \`inline code\``,
};

const NARRATION =
  'The migration applied cleanly and the first two checks pass. The third fails because the fixture still ' +
  'names the old column, so I will update the fixture and rerun only that check before touching anything else. ' +
  'Last words land here.';

function bash(id: string, itemIndex: number, threadId: string, status: Item['status'] = 'completed'): Item {
  return makeItem({
    id,
    threadId,
    turnIndex: TURN,
    itemIndex,
    kind: 'tool_call',
    toolName: 'Bash',
    status,
    summary: `Bash: go test ./internal/store -run TestMigration (${id})`,
    createdAt: TURN,
    updatedAt: TURN,
  });
}

function thinkingRow(threadId: string, itemIndex: number): Item {
  return makeItem({
    id: NARRATION_ID,
    threadId,
    turnIndex: TURN,
    itemIndex,
    kind: 'thinking',
    role: 'assistant',
    status: 'streaming',
    summary: '',
    rev: -1,
    createdAt: TURN,
    updatedAt: TURN,
  });
}

function proseRow(threadId: string, itemIndex: number): Item {
  return makeItem({
    id: NARRATION_ID,
    threadId,
    turnIndex: TURN,
    itemIndex,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: NARRATION,
    meta: JSON.stringify({ narration: true }),
    rev: 5,
    createdAt: TURN,
    updatedAt: TURN + 50,
  });
}

/** Stream the narration through the real reveal, then settle it as prose. */
async function streamNarration(
  pane: ThreadPane,
  threadId: string,
  itemIndex: number,
  queued: Item[] = [],
): Promise<void> {
  pane.applyProviderItemUpserts([thinkingRow(threadId, itemIndex)]);
  await tick();
  const chunks = 8;
  const step = Math.ceil(NARRATION.length / chunks);
  for (let i = 0; i < chunks; i += 1) {
    const delta = NARRATION.slice(i * step, (i + 1) * step);
    if (!delta) break;
    pane.applyItemDelta({
      threadId,
      itemId: NARRATION_ID,
      kind: 'thinking',
      delta,
      updatedAt: TURN + i + 1,
    });
    await wait(40);
  }
  // The block closes: the store's row is prose now, and the next rows of
  // the turn are already on the wire.
  pane.applyProviderItemUpserts([proseRow(threadId, itemIndex), ...queued]);
}

interface Frame {
  phase: 'thinking' | 'stage' | 'prose' | 'absent';
  /** Stage box height, while it is mounted. */
  stageHeight: number | null;
  /** Height of the box that closes, while the stage is mounted. */
  departingHeight: number | null;
  /** Top of the prose body in content coordinates (stage or installed row). */
  proseTop: number | null;
  /** Bottom of the prose's message block in content coordinates. */
  proseBottom: number | null;
  scrollHeight: number;
  gap: number;
  /** The thinking row's rendered text, while it is in the run. */
  thinkingText: string;
  queuedMounted: boolean;
  transitions: number;
  /** The run row wrapper's top margin, while the run is mounted. */
  runRowMargin: number | null;
  /** The run clip's distance from its own bottom, when it scrolls. */
  clipGap: number | null;
}

function contentY(scrollEl: HTMLElement, clientY: number): number {
  return clientY - scrollEl.getBoundingClientRect().top + scrollEl.scrollTop;
}

function sampleFrame(scrollEl: HTMLElement, runSelector: string): Frame {
  const stage = scrollEl.querySelector<HTMLElement>('[data-testid="row-promotion-stage"]');
  const leaf = scrollEl.querySelector<HTMLElement>(`[data-item-id="${NARRATION_ID}"]`);
  const inRun = leaf?.closest('[data-testid="activity-run"]') ?? null;
  const run = scrollEl.querySelector<HTMLElement>(runSelector);
  const message = stage?.querySelector<HTMLElement>('[data-item-kind="assistant_text"]')
    ?? (leaf && !inRun ? leaf.querySelector<HTMLElement>('[data-item-kind="assistant_text"]') : null);
  const body = message?.querySelector<HTMLElement>('[data-testid="assistant-message-body"]') ?? null;
  const clip = run?.querySelector<HTMLElement>('[data-testid="activity-run-clip"]') ?? null;
  const departing = stage
    ? leaf?.closest<HTMLElement>('[data-run-key]') ?? null
    : null;
  const runOnly = run !== null && run.querySelectorAll('[data-run-key]').length === 1;
  return {
    phase: stage ? 'stage' : leaf ? (inRun ? 'thinking' : 'prose') : 'absent',
    stageHeight: stage ? stage.getBoundingClientRect().height : null,
    departingHeight: stage
      ? (runOnly ? run!.getBoundingClientRect().height : departing?.getBoundingClientRect().height ?? null)
      : null,
    proseTop: body ? contentY(scrollEl, body.getBoundingClientRect().top) : null,
    proseBottom: message ? contentY(scrollEl, message.getBoundingClientRect().bottom) : null,
    scrollHeight: scrollEl.scrollHeight,
    gap: distanceToBottom(scrollEl),
    thinkingText: inRun ? leaf!.textContent ?? '' : '',
    queuedMounted: scrollEl.querySelector(`[data-item-id="${QUEUED_ID}"]`) !== null,
    transitions: scrollEl.getAnimations({ subtree: true })
      .filter((animation) => animation instanceof CSSTransition).length,
    runRowMargin: run
      ? Number.parseFloat(getComputedStyle(run.closest<HTMLElement>('[data-row-index]')!).marginTop)
      : null,
    clipGap: clip && clip.scrollHeight > clip.clientHeight + 1
      ? clip.scrollHeight - clip.clientHeight - clip.scrollTop
      : null,
  };
}

/** Sample every frame until the prose row is installed (and `settled` holds). */
async function recordUntilInstalled(
  scrollEl: HTMLElement,
  runSelector: string,
  settled: (frame: Frame) => boolean = () => true,
): Promise<Frame[]> {
  const frames: Frame[] = [];
  for (let i = 0; i < 900; i += 1) {
    await raf();
    const frame = sampleFrame(scrollEl, runSelector);
    frames.push(frame);
    if (frame.phase === 'prose' && settled(frame)) return frames;
  }
  throw new Error(`promotion did not install; phases: ${frames.map((f) => f.phase[0]).join('')}`);
}

function describeFrames(frames: Frame[]): string {
  return frames
    .filter((frame) => frame.phase !== 'thinking')
    .map((f) => `${f.phase[0]} stage=${f.stageHeight?.toFixed(1)} dep=${f.departingHeight?.toFixed(1)} top=${f.proseTop?.toFixed(1)} sh=${f.scrollHeight} gap=${f.gap.toFixed(1)} m=${f.runRowMargin}`)
    .join('\n');
}

/** The shared contract of a presented promotion. */
function expectPresentedPromotion(frames: Frame[]): { stage: Frame[]; installed: Frame } {
  const detail = () => describeFrames(frames);
  const firstStage = frames.findIndex((frame) => frame.phase === 'stage');
  const installedAt = frames.findIndex((frame) => frame.phase === 'prose');
  expect(firstStage, `a stage must present the promotion\n${detail()}`).toBeGreaterThan(0);
  const stage = frames.slice(firstStage, installedAt);
  expect(stage.every((frame) => frame.phase === 'stage'), `stage frames are contiguous\n${detail()}`).toBe(true);

  // The reveal finished first. Its last word and the stage mount share a
  // frame, and in that frame the old row is whole: the last word is painted
  // in the run before anything has moved.
  expect(frames[firstStage - 1].phase).toBe('thinking');
  expect(stage[0].thinkingText).toContain('Last words land here.');

  // A motion, not a jump: many frames, each step within the spring's cap.
  expect(stage.length, `the promotion spans many frames\n${detail()}`).toBeGreaterThanOrEqual(12);
  const heights = stage.map((frame) => frame.stageHeight!);
  expect(heights[0], `the stage opens from nothing\n${detail()}`).toBeLessThanOrEqual(4);
  for (let i = 1; i < heights.length; i += 1) {
    const stepPx = heights[i] - heights[i - 1];
    expect(stepPx, `stage never closes\n${detail()}`).toBeGreaterThanOrEqual(-0.01);
    expect(stepPx, `stage step within the spring cap\n${detail()}`)
      .toBeLessThanOrEqual(SPRING_MAX_VELOCITY_PX_PER_FRAME + 1);
  }
  const departing = stage.map((frame) => frame.departingHeight!);
  for (let i = 1; i < departing.length; i += 1) {
    const stepPx = departing[i - 1] - departing[i];
    expect(stepPx, `departing box never reopens\n${detail()}`).toBeGreaterThanOrEqual(-0.01);
    expect(stepPx, `departing step within the spring cap\n${detail()}`)
      .toBeLessThanOrEqual(SPRING_MAX_VELOCITY_PX_PER_FRAME + 1);
  }
  expect(departing[0], `the old row starts whole\n${detail()}`).toBeGreaterThan(16);
  expect(departing.at(-1)!, `the old row is closed when the row installs\n${detail()}`).toBeLessThanOrEqual(2);

  // Frame-driven: the timeline's transition rule holds through it.
  expect(Math.max(...stage.map((frame) => frame.transitions))).toBe(0);

  // The install lands the prose where the stage showed it.
  const before = stage.at(-1)!;
  const installed = frames[installedAt];
  expect(Math.abs(installed.proseTop! - before.proseTop!), `prose top holds across the install\n${detail()}`)
    .toBeLessThanOrEqual(1.5);
  expect(Math.abs(installed.proseBottom! - before.proseBottom!), `prose bottom holds across the install\n${detail()}`)
    .toBeLessThanOrEqual(1.5);
  return { stage, installed };
}

describe('narration promotion in the timeline', () => {
  it('moves the block out of a run that keeps its rows, then releases the queue', async () => {
    const threadId = 'thread-promotion-under-cap';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId), bash('run-bash-1', 1, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);

    const recording = recordUntilInstalled(
      scrollEl, '[data-testid="activity-run"]', (frame) => frame.queuedMounted,
    );
    await streamNarration(pane, threadId, 2, [bash(QUEUED_ID, 3, threadId, 'running')]);
    const frames = await recording;
    const { stage } = expectPresentedPromotion(frames);

    // The queued tool call waits for the install: never beside the thinking
    // row or the stage.
    for (const frame of frames) {
      if (frame.phase !== 'prose') expect(frame.queuedMounted).toBe(false);
    }
    // The run keeps its two tool rows, so its row keeps its margin.
    expect(new Set(stage.map((frame) => frame.runRowMargin)).size).toBe(1);

    const leaf = scrollEl.querySelector<HTMLElement>(`[data-item-id="${NARRATION_ID}"]`)!;
    expect(leaf.closest('[data-testid="activity-run"]')).toBeNull();
    expect(leaf.querySelector('[data-testid="assistant-message-summarized"]')?.textContent?.trim())
      .toBe('Summarized');
    expect(leaf.textContent).toContain('Last words land here.');
    const row = leaf.closest<HTMLElement>('[data-row-index]')!;
    expect(row.querySelector('[data-testid="response-divider"]')).not.toBeNull();
    const rowMargin = Number.parseFloat(getComputedStyle(row).marginTop);
    expect(rowMargin).toBeGreaterThan(0);
    // The stage opened exactly the box the row now occupies: gap, divider
    // with its margins, and message.
    expect(Math.abs(stage.at(-1)!.stageHeight! - (rowMargin + row.getBoundingClientRect().height)))
      .toBeLessThanOrEqual(2);
    const firstRun = scrollEl.querySelector<HTMLElement>('[data-testid="activity-run"]')!;
    expect(firstRun.querySelectorAll('[data-run-key]').length).toBe(2);
    expect(pane.getItemById(NARRATION_ID)).toMatchObject({ kind: 'assistant_text', status: 'completed' });
    expect(pane.revealBoundary).toBeNull();
    await waitForQuietBottom(scrollEl, 'bottom follow after the promotion', QUIET_BOTTOM);
  });

  it('replaces a run that held only the block, carrying the row margin', async () => {
    const threadId = 'thread-promotion-single';
    const { pane, scrollEl } = await mountTimeline(threadId, seedTimelineItems(threadId, PROSE), QUIET_BOTTOM);

    const recording = recordUntilInstalled(scrollEl, '[data-testid="activity-run"]');
    await streamNarration(pane, threadId, 0);
    const frames = await recording;
    const { stage, installed } = expectPresentedPromotion(frames);

    // Nothing else changed in that commit, so the page height holds too.
    expect(Math.abs(installed.scrollHeight - stage.at(-1)!.scrollHeight)).toBeLessThanOrEqual(1.5);
    // Prose after prose has no boundary gap: the run row's gap closes with it.
    const margins = stage.map((frame) => frame.runRowMargin!);
    expect(margins[0]).toBeGreaterThan(8);
    expect(margins.at(-1)!).toBeLessThanOrEqual(1);
    for (let i = 1; i < margins.length; i += 1) expect(margins[i]).toBeLessThanOrEqual(margins[i - 1] + 0.01);

    expect(scrollEl.querySelector('[data-testid="activity-run"]')).toBeNull();
    const leaf = scrollEl.querySelector<HTMLElement>(`[data-item-id="${NARRATION_ID}"]`)!;
    const row = leaf.closest<HTMLElement>('[data-row-index]')!;
    expect(row.querySelector('[data-testid="response-divider"]')).toBeNull();
    expect(Number.parseFloat(getComputedStyle(row).marginTop)).toBe(0);
    expect(leaf.querySelector('[data-testid="assistant-message-summarized"]')).not.toBeNull();
    await waitForQuietBottom(scrollEl, 'bottom follow after the promotion', QUIET_BOTTOM);
  });

  it('opens no divider when a run of one follows an error row after earlier activity', async () => {
    const threadId = 'thread-promotion-single-divider';
    const items = seedTimelineItems(threadId, PROSE);
    // An error row ends the first run without being prose, so the block
    // opens a run of its own. Only a run directly above opens a response.
    items.push(bash('run-bash-0', 0, threadId));
    items.push(makeItem({
      id: 'turn-error', threadId, turnIndex: TURN, itemIndex: 1, kind: 'error', role: 'system',
      status: 'completed', summary: 'The command exited with status 1.', createdAt: TURN, updatedAt: TURN,
    }));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    expect(scrollEl.querySelectorAll('[data-testid="activity-run"]').length).toBe(1);

    const runOfOne = `[data-testid="activity-run"]:has([data-item-id="${NARRATION_ID}"])`;
    const recording = recordUntilInstalled(scrollEl, runOfOne);
    await streamNarration(pane, threadId, 2);
    const frames = await recording;
    const { stage, installed } = expectPresentedPromotion(frames);

    const leaf = scrollEl.querySelector<HTMLElement>(`[data-item-id="${NARRATION_ID}"]`)!;
    const row = leaf.closest<HTMLElement>('[data-row-index]')!;
    expect(row.querySelector('[data-testid="response-divider"]')).toBeNull();
    expect(scrollEl.querySelectorAll('[data-testid="activity-run"]').length).toBe(1);
    // The stage opened exactly the box the installed row occupies.
    const rowMargin = Number.parseFloat(getComputedStyle(row).marginTop);
    expect(Math.abs(stage.at(-1)!.stageHeight! - (rowMargin + row.getBoundingClientRect().height)))
      .toBeLessThanOrEqual(2);
    expect(Math.abs(installed.scrollHeight - stage.at(-1)!.scrollHeight)).toBeLessThanOrEqual(1.5);
  });

  it('scrolls a capped run down as the block leaves it', async () => {
    const threadId = 'thread-promotion-capped';
    const items = seedTimelineItems(threadId, PROSE);
    for (let i = 0; i < 16; i += 1) items.push(bash(`run-bash-${i}`, i, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    const clip = scrollEl.querySelector<HTMLElement>('[data-testid="activity-run-clip"]')!;
    expect(clip.scrollHeight, 'the fixture run must be over its cap').toBeGreaterThan(clip.clientHeight + 20);

    const recording = recordUntilInstalled(
      scrollEl, '[data-testid="activity-run"]', (frame) => frame.queuedMounted,
    );
    await streamNarration(pane, threadId, 16, [bash(QUEUED_ID, 17, threadId, 'running')]);
    const frames = await recording;
    const { stage } = expectPresentedPromotion(frames);

    // The clip finishes chasing the last revealed line and then stays on
    // its own bottom: the closing row never opens a gap under it.
    const gaps = stage.map((frame) => frame.clipGap).filter((gap): gap is number => gap !== null);
    expect(gaps.length).toBeGreaterThan(0);
    for (let i = 1; i < gaps.length; i += 1) expect(gaps[i]).toBeLessThanOrEqual(gaps[i - 1] + 0.01);
    expect(gaps.at(-1)!).toBeLessThanOrEqual(1);
    await waitForQuietBottom(scrollEl, 'bottom follow after the promotion', QUIET_BOTTOM);
  });

  it('installs without a stage when motion is reduced', async () => {
    const threadId = 'thread-promotion-reduced';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    getSettings().lowPowerMode = true;

    const recording = recordUntilInstalled(
      scrollEl, '[data-testid="activity-run"]', (frame) => frame.queuedMounted,
    );
    await streamNarration(pane, threadId, 1, [bash(QUEUED_ID, 2, threadId, 'running')]);
    const frames = await recording;

    expect(frames.some((frame) => frame.phase === 'stage')).toBe(false);
    expect(pane.getItemById(NARRATION_ID)?.kind).toBe('assistant_text');
    expect(scrollEl.querySelector('[data-testid="row-promotion-stage"]')).toBeNull();
  });

  it('installs without a stage while the page is hidden', async () => {
    const threadId = 'thread-promotion-hidden';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    const visibility = Object.getOwnPropertyDescriptor(Document.prototype, 'visibilityState')!;
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    try {
      const recording = recordUntilInstalled(scrollEl, '[data-testid="activity-run"]');
      await streamNarration(pane, threadId, 1);
      const frames = await recording;
      expect(frames.some((frame) => frame.phase === 'stage')).toBe(false);
    } finally {
      delete (document as unknown as { visibilityState?: unknown }).visibilityState;
      expect(Object.getOwnPropertyDescriptor(Document.prototype, 'visibilityState')).toEqual(visibility);
    }
  });

  it('installs at once when the block is not mounted', async () => {
    const threadId = 'thread-promotion-offscreen';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);
    await userScrollTo(scrollEl, 0);
    await waitFor(
      () => scrollEl.querySelector('[data-item-id="run-bash-0"]') === null,
      'the tail to unmount',
    );

    await streamNarration(pane, threadId, 1, [bash(QUEUED_ID, 2, threadId, 'running')]);
    await waitFor(() => !pane.isItemSmoothing(NARRATION_ID), 'the reveal to drain', 600);
    // Nothing on screen to present: no wait for the presentation ceiling.
    await waitFor(() => pane.getItemById(NARRATION_ID)?.kind === 'assistant_text', 'the install', 10);
    expect(pane.revealBoundary).toBeNull();
  });

  it('installs when the row leaves the mounted window mid-promotion', async () => {
    const threadId = 'thread-promotion-scrolled-away';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId));
    const { pane, scrollEl } = await mountTimeline(threadId, items, QUIET_BOTTOM);

    await streamNarration(pane, threadId, 1, [bash(QUEUED_ID, 2, threadId, 'running')]);
    await waitFor(
      () => scrollEl.querySelector('[data-testid="row-promotion-stage"]') !== null,
      'the stage to mount',
      600,
    );
    // The reader leaves for the top: the virtualizer unmounts the tail row
    // and its stage with the promotion a few frames in.
    void userScrollTo(scrollEl, 0, 3);
    await waitFor(
      () => scrollEl.querySelector(`[data-item-id="${NARRATION_ID}"]`) === null,
      'the tail row to unmount',
      60,
    );
    expect(scrollEl.querySelector('[data-testid="row-promotion-stage"]')).toBeNull();
    // Well inside the presentation ceiling: the unmount ended the hold.
    await waitFor(() => pane.getItemById(NARRATION_ID)?.kind === 'assistant_text', 'the install', 20);
    expect(pane.presentingRowPromotions).toEqual([]);
    expect(pane.revealBoundary).toBeNull();
  });

  it('installs when the timeline unmounts mid-promotion', async () => {
    const threadId = 'thread-promotion-unmount';
    const items = seedTimelineItems(threadId, PROSE);
    items.push(bash('run-bash-0', 0, threadId));
    const { pane, scrollEl, entry } = await mountTimeline(threadId, items, QUIET_BOTTOM);

    await streamNarration(pane, threadId, 1, [bash(QUEUED_ID, 2, threadId, 'running')]);
    await waitFor(
      () => scrollEl.querySelector('[data-testid="row-promotion-stage"]') !== null,
      'the stage to mount',
      600,
    );
    expect(pane.getItemById(NARRATION_ID)?.kind).toBe('thinking');

    const { unmount } = await import('svelte');
    unmount(entry.app);
    await tick();
    await Promise.resolve();
    expect(pane.getItemById(NARRATION_ID)).toMatchObject({ kind: 'assistant_text', summary: NARRATION });
    expect(pane.presentingRowPromotions).toEqual([]);
    expect(pane.revealBoundary).toBeNull();
  });
});
