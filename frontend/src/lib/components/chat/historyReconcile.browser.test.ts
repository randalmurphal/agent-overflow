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

setupTimelineHarness();

for (const activity of ['unchanged-child', 'streaming-child', 'parent', 'cold', 'reading', 'unchanged'] as const) {
  it(`places reconciled history before paint with ${activity} activity`, async () => {
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
    const openingGaps: number[] = [];
    for (let i = 0; i < 35; i++) {
      await raf();
      const row = scrollEl.querySelector('[data-item-id="tail"]');
      if (row && getComputedStyle(row).visibility === 'visible') openingGaps.push(distanceToBottom(scrollEl));
    }
    if (activity === 'cold') expect(pane.items).toHaveLength(0);
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
    answer();
    const gaps: number[] = [];
    const visibleHeights: number[] = [];
    let firstTailText: string | null = null;
    for (let i = 0; i < 100; i++) {
      await afterRendering();
      const row = scrollEl.querySelector('[data-item-id="tail"]');
      if (row && getComputedStyle(row).visibility === 'visible') {
        firstTailText ??= row.textContent ?? '';
        gaps.push(distanceToBottom(scrollEl));
        visibleHeights.push(scrollEl.scrollHeight);
      }
    }
    await switching;
    // The staged window stays hidden until SyncThreadWindow verifies it, so
    // the first painted frame is the reconciled window at the bottom.
    expect(openingGaps).toEqual([]);
    expect(gaps.length).toBeGreaterThan(0);
    expect(Math.max(...gaps), JSON.stringify(getUiRenderTraceRecords().filter(r => r.label === 'scroll.contentRO'))).toBeLessThanOrEqual(2);
    if (activity === 'unchanged') {
      expect(firstTailText).not.toContain('Additional saved prose');
      expect(new Set(visibleHeights).size).toBe(1);
      return;
    }
    expect(firstTailText).toContain('Additional saved prose');
    if (activity !== 'reading') return;

    // A reconcile over the visible window (a backend refresh) grows the
    // history the reader is in without moving the reader.
    await userScrollTo(scrollEl, scrollEl.scrollTop - 800);
    const readingTop = scrollEl.scrollTop;
    installThreadSwitchMocks(thread, next.map((item) => item.id === tail.id
      ? { ...item, summary: `${item.summary}\n\n${'Recovered saved prose. '.repeat(100)}`, updatedAt: 3 }
      : item));
    const refresh = pane.refreshFromBackend(true);
    const readingPositions: number[] = [];
    for (let i = 0; i < 60; i++) {
      await afterRendering();
      readingPositions.push(scrollEl.scrollTop);
    }
    await refresh;
    expect(pane.getItemById(tail.id)?.summary).toContain('Recovered saved prose');
    expect(Math.max(...readingPositions.map(top => Math.abs(top - readingTop)))).toBeLessThanOrEqual(2);
  });
}

/**
 * One frame, sampled after the rendering update: rAF itself runs before
 * the row ResizeObserver can commit its correction, which is still
 * pre-paint.
 */
async function afterRendering(): Promise<void> {
  await raf();
  await new Promise(resolve => setTimeout(resolve, 0));
}
