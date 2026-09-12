import { expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { mountTimeline, seedTimelineItems, setupTimelineHarness } from '../../../test/helpers/timelineBrowserHarness';
import { getFlushedForThread, markItemsFlushed } from '../../stores/sendQueue.svelte';
import { isThreadWorking, projectTurnStarted } from '../../stores/threadStatuses.svelte';

setupTimelineHarness();

it.each(['claude', 'codex'] as const)('recovers the final answer in a narrow %s timeline', async (provider) => {
  const threadId = `narrow-recovery-${provider}`;
  const items = seedTimelineItems(threadId, {
    question: i => `Question ${i}`,
    replyLead: i => `Reply ${i}: settled conversation history.`,
    replyList: '- First detail\n- Second detail',
  });
  const { pane, host, scrollEl } = await mountTimeline(threadId, items,
    { epsilonPx: 1, stableFrames: 8, frameBudget: 480 }, { provider });
  host.style.width = '380px';
  const old = makeItem({ id: 'stale-thinking', threadId, kind: 'thinking', status: 'streaming',
    turnIndex: 75, itemIndex: 22, summary: 'Old reasoning', updatedAt: 100 });
  pane.applyProviderItemUpserts([old]);
  pane.applyItemDelta({ threadId, itemId: old.id, kind: 'thinking', delta: ' still thinking', updatedAt: 110 });
  projectTurnStarted(threadId, 'finished-turn', 75, 100);
  markItemsFlushed(threadId, [{ queueItemId: 'delivered', userItemId: 'message', message: 'Already received' }]);
  await tick();
  expect(pane.revealBoundary).not.toBeNull();
  const answer = makeItem({ id: 'final-answer', threadId, kind: 'assistant_text', status: 'completed',
    turnIndex: 75, itemIndex: 63, summary: 'The final answer is visible after recovery.', updatedAt: 200 });
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [...items, { ...old, status: 'completed', summary: 'Finished reasoning', updatedAt: 200 }, answer],
    hasMoreOlder: false, hasMoreNewer: false,
  }));
  setBindingMock('GetThreadLiveState', async () => ({ threadId, activeTurn: null,
    queueItems: [], flushedItems: [], interactive: { approvals: [], userInputs: [] } }));
  await pane.refreshFromBackend(true);
  await vi.waitFor(() => {
    const row = scrollEl.querySelector('[data-item-id="final-answer"]');
    expect(row?.textContent).toContain(answer.summary);
    expect(row && getComputedStyle(row).visibility).toBe('visible');
  }, { timeout: 5000 });
  expect(pane.revealBoundary).toBeNull();
  expect(getFlushedForThread(threadId)).toEqual([]);
  expect(isThreadWorking(threadId)).toBe(false);
});
