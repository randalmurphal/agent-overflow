// The live→settled handoff of the tail run's inner scroll controller,
// against a real layout engine.
//
// The incident this pins (bug-report-20260819T195810Z): the controller's
// lifetime keyed on `live`, which ends the moment closing prose exists
// behind the reveal gate — mid-stream from where the reader sits. Tearing
// the controller down there cancelled the clip's in-flight glide where it
// stood, and the settle observer then snapped the whole remaining distance
// in one frame on the next content change. Every one of those post-teardown
// writes also fired a scroll event no owner claimed, so the run's overlay
// scrollbar flashed as if the reader had scrolled — the witness that
// cracked the case.
//
// Three guarantees, each a fix:
//  1. The controller lives while the run holds the REVEALED tail (`atTail`),
//     so closing prose arriving on the wire no longer kills a glide the
//     reader is watching.
//  2. Retirement waits for the chase to finish, including selection pauses,
//     and transfers intent without writing a new position during teardown.
//  3. The overlay scrollbar treats the settle observer's bottom-hold as
//     owner-driven: only positions the READER produced show the thumb.
//
// All geometry — happy-dom cannot see any of it.
import { describe, expect, it } from 'vitest';
// Real production cascade: the clip's cap and row heights come from app.css.
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';
import type { ThreadPane } from '../../stores/thread.svelte';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
// Fractional rects (fonts, DPR). A failure here is a parked glide gap —
// tens of px — so a couple of pixels of tolerance costs nothing.
const DRIFT_PX = 2;

function tool(id: string, turnIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Bash',
    status: 'completed',
    summary: `Bash: inspect fixture ${id} with enough text to hold a realistic row height`,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function thinking(id: string, turnIndex: number, threadId: string, status: Item['status']): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    kind: 'thinking',
    role: 'assistant',
    status,
    summary:
      'Working through the fixture: the run tail is a thinking block that is still streaming while the closing prose arrives on the wire.',
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function prose(id: string, turnIndex: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    turnIndex,
    itemIndex: 0,
    status: 'completed',
    summary: `Reply ${id}: the closing prose whose arrival used to tear the inner controller down mid-glide.`,
    createdAt: turnIndex,
    updatedAt: turnIndex,
  });
}

function clipOf(host: HTMLElement): HTMLElement {
  const clips = host.querySelectorAll<HTMLElement>('[data-testid="activity-run-clip"]');
  const clip = clips[clips.length - 1];
  if (!clip) throw new Error('no activity-run clip mounted');
  return clip;
}

function clipGap(clip: HTMLElement): number {
  return clip.scrollHeight - clip.scrollTop - clip.clientHeight;
}

/** The scrollbar track inside the run that owns `clip` (the bar is the
 * clip's sibling; the visibility classes live on the track). */
function barOf(clip: HTMLElement): HTMLElement {
  const run = clip.closest('[data-testid="activity-run"]');
  const bar = run?.querySelector<HTMLElement>('[data-testid="overlay-scrollbar"]');
  if (!bar) throw new Error('no overlay-scrollbar mounted for the run');
  return bar;
}

/** The bar holds visibility for IDLE_HIDE_MS (900ms) after any activation,
 * so per-frame sampling cannot miss a flash. */
function barShowing(bar: HTMLElement): boolean {
  return bar.classList.contains('opacity-100');
}

/**
 * Sample every frame for `frames` frames; returns whether the thumb ever
 * showed and the largest gap left at the end. The sequence under test keeps
 * running while this samples — it IS the observation window.
 */
async function watchBar(clip: HTMLElement, frames: number): Promise<boolean> {
  const bar = barOf(clip);
  let saw = false;
  for (let i = 0; i < frames; i += 1) {
    await raf();
    if (barShowing(bar)) saw = true;
  }
  return saw;
}

/** Grow the run by appending activity rows in one burst, so the clip's
 * spring is provably mid-glide when the next thing happens. */
async function burstGrowth(
  pane: ThreadPane,
  threadId: string,
  fromTurn: number,
  rows: number,
): Promise<void> {
  const items: Item[] = [];
  for (let i = 0; i < rows; i += 1) {
    items.push(tool(`burst-${fromTurn + i}`, fromTurn + i, threadId));
  }
  pane.applyProviderItemUpserts(items);
  await tick();
}

describe('tail handoff', () => {
  it.each(['release', 'tail-return', 'escape', 'collapse'] as const)(
    'a paused retirement preserves motion and handles %s', async (finish) => {
    const threadId = 'thread-handoff-selection';
    const seed: Item[] = [prose('p0', 0, threadId)];
    for (let i = 0; i < 12; i++) seed.push(tool(`t${i}`, i + 1, threadId));
    seed.push(thinking('th0', 20, threadId, 'streaming'));
    const { pane, host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);
    const clip = clipOf(host);
    await burstGrowth(pane, threadId, 21, 4);
    await waitFor(() => clipGap(clip) > 40, 'an unfinished inner glide');

    const selected = clip.querySelector('[data-item-id]');
    if (!selected) throw new Error('no selectable activity row');
    const selection = window.getSelection();
    if (!selection) throw new Error('no browser selection');
    const range = document.createRange();
    range.selectNodeContents(selected);
    selection.removeAllRanges();
    selection.addRange(range);
    selected.dispatchEvent(new PointerEvent('pointerdown', {
      button: 0, buttons: 1, pointerType: 'mouse', bubbles: true,
    }));
    try {
      await raf();
      const parked = clip.scrollTop;
      const gap = clipGap(clip);
      pane.applyProviderItemUpserts([prose('p1', 100, threadId)]);
      await tick();
      expect(clip.dataset.scrollOwner).toBe('controller');
      // Exceed the former hold deadline only in the straight-release case.
      if (finish === 'release') await new Promise((resolve) => setTimeout(resolve, 1750));
      await raf();
      expect(clip.scrollTop, 'handoff must not move a selecting reader').toBe(parked);
      expect(clip.dataset.scrollOwner, 'the paused glide still owns its position').toBe('controller');

      if (finish === 'tail-return') {
        pane.removeItemById('p1', threadId);
        await tick();
        await raf();
        expect(clip.dataset.scrollOwner).toBe('controller');
        expect(clip.scrollTop).toBe(parked);
        pane.applyProviderItemUpserts([prose('p1', 100, threadId)]);
        await tick();
        await raf();
        expect(clip.dataset.scrollOwner).toBe('controller');
        expect(clip.scrollTop).toBe(parked);
      } else if (finish === 'escape') {
        clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
        clip.scrollTop = parked - 20;
        const escaped = clip.scrollTop;
        await waitFor(() => clip.dataset.scrollOwner === 'settle', 'retirement after user escape');
        await burstGrowth(pane, threadId, 30, 2);
        for (let i = 0; i < 5; i++) await raf();
        expect(clip.scrollTop, 'settled geometry must preserve escaped intent').toBe(escaped);
        return;
      } else if (finish === 'collapse') {
        const runId = clip.closest<HTMLElement>('[data-run-id]')?.dataset.runId;
        if (!runId) throw new Error('no run identity');
        pane.activityRuns.setCollapsed(runId, true);
        await tick();
        await raf();
        expect(clip.isConnected).toBe(false);
        pane.activityRuns.setCollapsed(runId, false);
        await tick();
        const reopened = clipOf(host);
        await waitFor(() => clipGap(reopened) <= DRIFT_PX, 'collapsed retirement remount');
        expect(reopened).not.toBe(clip);
        expect(reopened.dataset.scrollOwner).toBe('settle');
        return;
      }

      selected.dispatchEvent(new PointerEvent('pointerup', {
        button: 0, buttons: 0, pointerType: 'mouse', bubbles: true,
      }));
      selection.removeAllRanges();
      let previous = parked;
      let largestStep = 0;
      let movingFrames = 0;
      for (let frame = 0; frame < 360; frame++) {
        await raf();
        const step = clip.scrollTop - previous;
        if (step > 0) movingFrames++;
        largestStep = Math.max(largestStep, step);
        previous = clip.scrollTop;
        if (clip.dataset.scrollOwner === 'settle') break;
      }
      expect(movingFrames, 'release resumes motion over multiple frames').toBeGreaterThan(3);
      expect(largestStep, 'release must not land the entire remaining glide').toBeLessThan(gap / 2);
      expect(clipGap(clip)).toBeLessThanOrEqual(DRIFT_PX);
      expect(clip.dataset.scrollOwner).toBe('settle');
    } finally {
      document.dispatchEvent(new PointerEvent('pointerup', { buttons: 0, pointerType: 'mouse' }));
      selection.removeAllRanges();
    }
  }, 20_000);

  it('closing prose behind the reveal gate neither kills the controller nor strands the glide nor flashes the scrollbar', async () => {
    const threadId = 'thread-handoff-a';
    const seed: Item[] = [prose('p0', 0, threadId)];
    for (let i = 0; i < 12; i += 1) seed.push(tool(`t${i}`, i + 1, threadId));
    seed.push(thinking('th0', 20, threadId, 'streaming'));

    const { pane, scrollEl, host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);
    const clip = clipOf(host);
    // The cap must be in play or every bottom question is vacuous.
    expect(clip.scrollHeight, 'run must overflow its clip').toBeGreaterThan(
      clip.clientHeight + 40,
    );
    const bar = barOf(clip);
    const run = clip.closest<HTMLElement>('[data-testid="activity-run"]');
    if (!run) throw new Error('clip must sit inside its run');

    // The thinking tail takes a smoother backlog, so the reveal gate is
    // genuinely engaged: everything upserted from here is WITHHELD until
    // the backlog drains over real frames. Without the delta there is no
    // smoother at all (upserts never create one) and `live`/`atTail` would
    // flip in the same projection pass — the incident's window would not
    // exist.
    pane.applyItemDelta({
      threadId,
      itemId: 'th0',
      kind: 'thinking',
      delta: 'word '.repeat(40),
      updatedAt: 21,
    });
    // The closing prose arrives ON THE WIRE while the tail still streams,
    // and the item completes behind it — the incident's exact shape.
    pane.applyProviderItemUpserts([prose('p1', 40, threadId)]);
    await tick();
    pane.applyItemPatch({
      threadId,
      itemId: 'th0',
      kind: 'thinking',
      patch: { status: 'completed', updatedAt: 41 },
    });
    await tick();

    // THE lifetime pin, at geometry level: wire-liveness is over, the
    // prose is still withheld, and the controller must still own the clip.
    // Under the old `live` keying the owner reads 'settle' right here.
    expect(run.dataset.live, 'wire liveness must have ended').toBe('false');
    expect(
      scrollEl.querySelector('[data-item-id="p1"]'),
      'the closing prose must still be behind the reveal gate',
    ).toBeNull();
    expect(clip.dataset.scrollOwner).toBe('controller');

    // Sample the bar EVERY frame from before the reveal through the
    // handoff plus a tail longer than a flash can hide from (the bar holds
    // any activation visible for 900ms) — the observation window cannot
    // close before the moment under test happens inside it.
    let sawThumb = false;
    let revealedAt = -1;
    for (let frame = 0; frame < 900; frame += 1) {
      await raf();
      if (barShowing(bar)) sawThumb = true;
      if (revealedAt < 0 && scrollEl.querySelector('[data-item-id="p1"]') !== null) {
        revealedAt = frame;
      }
      if (revealedAt >= 0 && frame - revealedAt >= 80) break;
    }
    expect(revealedAt, 'the withheld prose must reveal within the frame budget').toBeGreaterThanOrEqual(0);

    expect(sawThumb, 'no reader gesture happened, so the thumb must never show').toBe(false);
    expect(
      clipGap(clip),
      'the clip must end at its bottom — a parked cancelled-glide gap is the jump',
    ).toBeLessThanOrEqual(DRIFT_PX);
    expect(clip.dataset.scrollOwner, 'the settle half owns the clip after the handoff').toBe(
      'settle',
    );
  }, 30_000);

  it('activity landing after the handoff stays owner-driven', async () => {
    // The stress shape: content keeps landing in the run after the prose
    // displaced it. The settle observer holds the bottom with instant
    // writes; those writes are the run's own follow, not the reader, so
    // the thumb stays hidden (it flashing on every post-handoff write was
    // the incident's witness).
    const threadId = 'thread-handoff-b';
    const seed: Item[] = [prose('p0', 0, threadId)];
    for (let i = 0; i < 12; i += 1) seed.push(tool(`t${i}`, i + 1, threadId));
    seed.push(thinking('th0', 20, threadId, 'streaming'));

    const { pane, host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);
    const clip = clipOf(host);
    expect(clip.scrollHeight, 'run must overflow its clip').toBeGreaterThan(
      clip.clientHeight + 40,
    );

    pane.applyProviderItemUpserts([prose('p1', 40, threadId)]);
    await tick();
    expect(clip.dataset.scrollOwner, 'the prose handoff must have happened').toBe('settle');

    // Post-displacement growth INSIDE the displaced run — turn indices
    // between the run's last member (20) and the prose (40), so the rows
    // join run 1 and drive ITS settle observer rather than starting a
    // fresh run of their own. Spread over frames so each lands as its own
    // settle-observer write.
    let saw = false;
    for (let i = 0; i < 4; i += 1) {
      await burstGrowth(pane, threadId, 21 + i, 1);
      if (await watchBar(clip, 12)) saw = true;
    }
    if (await watchBar(clip, 80)) saw = true;

    expect(saw, 'follow writes are owner-driven; the thumb is for readers').toBe(false);
    expect(clipGap(clip), 'the follow must still hold the bottom').toBeLessThanOrEqual(DRIFT_PX);
  }, 20_000);

  it('a real reader scroll still shows the thumb', async () => {
    // The suppression must not swallow the thumb's whole reason to exist:
    // a wheel gesture inside a settled run is the reader scrolling, and the
    // bar is how they see where they are.
    const threadId = 'thread-handoff-c';
    const seed: Item[] = [];
    for (let i = 0; i < 18; i += 1) seed.push(tool(`t${i}`, i, threadId));
    seed.push(prose('p0', 30, threadId));

    const { host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);
    const clip = clipOf(host);
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight + 40);
    const bar = barOf(clip);

    for (let i = 0; i < 3; i += 1) {
      clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
      clip.scrollTop -= 40;
      await raf();
    }
    await waitFor(() => barShowing(bar), 'bar to show on a reader scroll');
  }, 20_000);

  it('an escaped reader survives the handoff unpinned', async () => {
    // The controller records escapes this component never gets a gesture
    // for, so the handoff must cross the FOLLOW STATE with the position: a
    // stale "following" would let the settle observer pin the escaped
    // reader back to the bottom on the next growth — and the owner-driven
    // scrollbar classification would hide the thumb while it happened.
    const threadId = 'thread-handoff-d';
    const seed: Item[] = [prose('p0', 0, threadId)];
    for (let i = 0; i < 18; i += 1) seed.push(tool(`t${i}`, i + 1, threadId));
    seed.push(thinking('th0', 20, threadId, 'streaming'));

    const { pane, host } = await mountTimeline(threadId, seed, QUIET_BOTTOM);
    const clip = clipOf(host);
    expect(clip.scrollHeight, 'run must overflow its clip').toBeGreaterThan(
      clip.clientHeight + 40,
    );
    expect(clip.dataset.scrollOwner, 'the tail run must own a controller').toBe('controller');

    // An escape the COMPONENT never sees: middle-click autoscroll latches
    // `escapedFromLock` inside the controller (`intent.ts` handlePointerDown,
    // button 1) but `readerGestures` only arms on wheel/touch/key — so the
    // component's own `followingBottom` never learns. That divergence is
    // exactly what the handoff must reconcile.
    clip.dispatchEvent(new PointerEvent('pointerdown', { button: 1, bubbles: true }));
    await raf();
    for (let i = 0; i < 3; i += 1) {
      clip.scrollTop -= 40;
      await raf();
    }
    await raf();
    const parked = clip.scrollTop;
    expect(clipGap(clip), 'the reader must be off the bottom').toBeGreaterThan(DRIFT_PX);

    // The closing prose reveals and the controller dies. The handover must
    // not move an escaped reader.
    pane.applyProviderItemUpserts([prose('p1', 40, threadId)]);
    await tick();
    await raf();
    expect(clip.dataset.scrollOwner).toBe('settle');
    expect(
      Math.abs(clip.scrollTop - parked),
      'the handoff must leave an escaped reader where they parked',
    ).toBeLessThanOrEqual(DRIFT_PX);

    // Growth inside the settled run must not pull them down either — the
    // escape crossed the handoff into the settle half's own record.
    await burstGrowth(pane, threadId, 21, 2);
    await watchBar(clip, 12);
    expect(
      Math.abs(clip.scrollTop - parked),
      'the settle observer must not pin a reader who had escaped',
    ).toBeLessThanOrEqual(DRIFT_PX);
  }, 20_000);
});
