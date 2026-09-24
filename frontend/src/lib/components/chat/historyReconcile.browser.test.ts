import { expect, it } from 'vitest';
import '../../../app.css';
import { distanceToBottom, mountTimeline, setupTimelineHarness, userScrollTo } from '../../../test/helpers/timelineBrowserHarness';
import { installThreadSwitchMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { threadItemCache } from '../../stores/threadItemCache';
import { removeReplicaWindow } from '../../replica';
import { loadSettings } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { clearUiRenderTrace, getUiRenderTraceRecords, setUiRenderTraceEnabled } from '../../utils/uiRenderTrace';
import { getThreadScrollSnapshot } from '../../utils/threadScrollSnapshots';

setupTimelineHarness();

// A switch back to a thread holds its cached or replica window hidden until
// SyncThreadWindow verifies it, then paints the verified window
// (docs/architecture/frontend-scroll.md, "Thread Switch"). Each case holds
// the answer open, checks that nothing painted meanwhile, then releases it.
//
// - reading: the reader left the thread scrolled up. The verified window
//   paints at that reading position in its first frame and holds it while
//   the grown tail lands below.
// - unchanged: the verified window equals the cache. It paints once and
//   never repaints: no hide, no height change, no row remount.
// - the rest: live activity during the sync, or no cache at all. The
//   verified window paints pinned to the bottom with its grown tail.

function rowTop(scrollEl: HTMLElement, row: Element): number {
  return row.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top;
}

function visibleRow(scrollEl: HTMLElement, selector: string): Element | null {
  const row = scrollEl.querySelector(selector);
  return row && getComputedStyle(row).visibility === 'visible' ? row : null;
}

for (const activity of ['unchanged-child', 'streaming-child', 'parent', 'cold', 'reading', 'unchanged'] as const) {
  it(`places verified history before paint with ${activity} activity`, async () => {
    setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'collapsed' }));
    await loadSettings();
    const thread = makeThread({ id: `history-${activity}` });
    const items = Array.from({ length: 40 }, (_, i) => makeItem({
      id: `prose-${i}`, threadId: thread.id, turnIndex: i, itemIndex: 0,
      kind: 'assistant_text', status: 'completed',
      summary: `Recorded response ${i}.\n\n${'This paragraph belongs to the saved conversation. '.repeat(12)}`,
    }));
    const parent = makeItem({ id: 'agent', threadId: thread.id, turnIndex: 40, itemIndex: 0,
      kind: 'tool_call', toolName: 'Agent', status: 'running', summary: 'Background work' });
    const child = makeItem({ id: 'child', threadId: thread.id, parentId: parent.id,
      turnIndex: 40, itemIndex: 1, kind: 'thinking', status: 'streaming', summary: 'Working' });
    const tail = makeItem({ id: 'tail', threadId: thread.id, turnIndex: 41, itemIndex: 0,
      kind: 'assistant_text', status: 'completed', summary: 'The last recorded response.' });
    items.push(parent, child, tail);
    const { pane, scrollEl } = await mountTimeline(thread.id, items,
      { stableFrames: 12, frameBudget: 600, epsilonPx: 2 });

    let reading: { itemId: string; top: number } | null = null;
    if (activity === 'reading') {
      await userScrollTo(scrollEl, scrollEl.scrollTop - 800);
      await waitFor(() => getThreadScrollSnapshot(thread.id)?.kind === 'anchor', 'reading position saved');
      const saved = getThreadScrollSnapshot(thread.id);
      if (saved?.kind !== 'anchor') throw new Error('reading position was not saved as an anchor');
      const row = scrollEl.querySelector(`[data-item-id="${saved.itemId}"]`);
      if (!row) throw new Error(`anchor row ${saved.itemId} is not mounted`);
      reading = { itemId: saved.itemId, top: rowTop(scrollEl, row) };
    }

    const other = makeThread({ id: 'other' });
    installThreadSwitchMocks(other, [makeItem({ threadId: other.id })]);
    await pane.switchThread(other);
    for (let i = 0; i < 20; i++) await raf();
    if (activity === 'cold') {
      threadItemCache.evictMatching(id => id === thread.id);
      await removeReplicaWindow(thread.id);
    }
    installThreadSwitchMocks(thread, items);
    let answer!: () => void;
    const ready = new Promise<void>((resolve) => { answer = resolve; });
    const next = items.map((item) => activity !== 'unchanged' && item.id === tail.id
      ? { ...item, summary: `${item.summary}\n\n${'Additional saved prose. '.repeat(100)}`, updatedAt: 2 }
      : item);
    setBindingMock('SyncThreadWindow', async () => {
      await ready;
      return { status: 'stale', epoch: 1, rev: 2, generation: 'test-generation',
        page: { items: next, hasMore: false, runs: [],
          oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: items[0].id },
          newestCursor: { turnIndex: 41, itemIndex: 0, itemId: tail.id } } };
    });
    setUiRenderTraceEnabled(true);
    clearUiRenderTrace();
    const switching = pane.switchThread(thread);
    let paintedBeforeVerification = 0;
    for (let i = 0; i < 35; i++) {
      await raf();
      if (visibleRow(scrollEl, '[data-row-index]')) paintedBeforeVerification++;
    }
    if (activity === 'cold') expect(pane.items).toHaveLength(0);
    else expect(pane.items.length, 'the cached window is held for verification').toBeGreaterThan(0);
    expect(pane.historyWindowPending).toBe(true);
    if (activity === 'unchanged-child') {
      pane.applyItemPatch({ threadId: thread.id, itemId: child.id, kind: child.kind,
        patch: { summary: child.summary, rev: 2 } });
    } else if (activity === 'parent') {
      pane.applyItemPatch({ threadId: thread.id, itemId: parent.id, kind: parent.kind,
        patch: { summary: 'Updated background task description', rev: 2 } });
      expect(pane.lastLiveContentAt).toBeGreaterThan(0);
    } else if (activity === 'streaming-child') {
      const item = child;
      pane.applyItemDelta({ threadId: thread.id, itemId: item.id, kind: item.kind,
        delta: ' More live work.', updatedAt: 3 });
      await waitFor(() => pane.getItemById(child.id)?.summary !== child.summary, 'child reveal before sync');
      expect(pane.lastLiveContentAt).toBe(0);
    }
    for (let i = 0; i < 5; i++) {
      await raf();
      if (visibleRow(scrollEl, '[data-row-index]')) paintedBeforeVerification++;
    }
    expect(paintedBeforeVerification, 'no frame paints the window before verification').toBe(0);

    const pendingHeight = scrollEl.scrollHeight;
    answer();
    interface Frame { visible: boolean; gap: number; scrollHeight: number; tail: Element | null; readingTop: number | null }
    const frames: Frame[] = [];
    let growthFrames = 0;
    for (let i = 0; i < 100; i++) {
      await raf();
      // Sample after the rendering update: rAF itself runs before the row
      // ResizeObserver can commit its correction, which is still pre-paint.
      await new Promise(resolve => setTimeout(resolve, 0));
      const visible = visibleRow(scrollEl, '[data-row-index]') !== null;
      const anchor = reading ? visibleRow(scrollEl, `[data-item-id="${reading.itemId}"]`) : null;
      frames.push({
        visible,
        gap: distanceToBottom(scrollEl),
        scrollHeight: scrollEl.scrollHeight,
        tail: scrollEl.querySelector('[data-item-id="tail"]'),
        readingTop: anchor ? rowTop(scrollEl, anchor) : null,
      });
      if (visibleRow(scrollEl, '[data-item-id="tail"]') && scrollEl.scrollHeight > pendingHeight + 100) growthFrames++;
    }
    await switching;
    expect(pane.historyWindowPending).toBe(false);
    const first = frames.findIndex(frame => frame.visible);
    expect(first, 'the verified window paints').toBeGreaterThanOrEqual(0);
    const painted = frames.slice(first);

    if (activity === 'reading') {
      const tops = painted.map(frame => frame.readingTop);
      expect(tops.every(top => top !== null), 'the reading row is visible in every painted frame').toBe(true);
      const drift = Math.max(...tops.map(top => Math.abs((top ?? Infinity) - reading!.top)));
      expect(drift, 'the verified window paints at the reading position and holds it').toBeLessThanOrEqual(2);
      expect(Math.min(...painted.map(frame => frame.gap)), 'the reader is not pulled to the bottom').toBeGreaterThan(2);
      return;
    }
    if (activity === 'unchanged') {
      expect(painted.every(frame => frame.visible), 'a painted window is never hidden again').toBe(true);
      expect(new Set(painted.map(frame => frame.scrollHeight)).size, 'no height change after the first paint').toBe(1);
      expect(painted.every(frame => frame.tail !== null && frame.tail === painted[0].tail), 'the tail row is never remounted').toBe(true);
      expect(Math.max(...painted.map(frame => frame.gap))).toBeLessThanOrEqual(2);
      return;
    }
    expect(growthFrames).toBeGreaterThan(10);
    expect(Math.max(...painted.map(frame => frame.gap)), JSON.stringify(getUiRenderTraceRecords().filter(r => r.label === 'scroll.contentRO'))).toBeLessThanOrEqual(2);
  });
}
