import { expect, it } from 'vitest';
import '../../../app.css';
import { mountTimeline, setupTimelineHarness, userScrollTo, distanceToBottom } from '../../../test/helpers/timelineBrowserHarness';
import { installThreadSwitchMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { getThreadScrollSnapshot } from '../../utils/threadScrollSnapshots';

setupTimelineHarness();

it('persists a wheel re-stick without another scroll event and follows new history on return', async () => {
  const thread = makeThread({ id: 'wheel-follow-return' });
  const items = Array.from({ length: 45 }, (_, i) => makeItem({
    id: `message-${i}`, threadId: thread.id, turnIndex: i, itemIndex: 0,
    summary: `Message ${i}. ${'Saved conversation text. '.repeat(70)}`,
  }));
  const { pane, scrollEl, host } = await mountTimeline(thread.id, items, { stableFrames: 12, frameBudget: 600, epsilonPx: 2 });
  await userScrollTo(scrollEl, scrollEl.scrollTop - 120);
  await waitFor(() => getThreadScrollSnapshot(thread.id)?.kind === 'anchor', 'reader anchor saved');
  // Exactly one downward gesture and landing: no extra wheel event at
  // the bottom and no button click that could save the intent for us.
  await userScrollTo(scrollEl, scrollEl.scrollHeight, 1);
  await waitFor(() => !host.textContent?.includes('Jump to bottom'), 'bottom following resumed');
  expect(getThreadScrollSnapshot(thread.id)).toEqual({ kind: 'bottom' });
  const other = makeThread({ id: 'away-from-wheel-follow' });
  installThreadSwitchMocks(other, [makeItem({ threadId: other.id })]);
  await pane.switchThread(other);
  for (let i = 0; i < 10; i++) await raf();
  const next = [...items, makeItem({ id: 'new-tail', threadId: thread.id, turnIndex: 45,
    summary: 'New work arrived while away. '.repeat(80) })];
  installThreadSwitchMocks(thread, next);
  await pane.switchThread(thread);
  await waitFor(() => distanceToBottom(scrollEl) <= 2 && !!scrollEl.querySelector('[data-item-id="new-tail"]'), 'new history followed');
  expect(getThreadScrollSnapshot(thread.id)).toEqual({ kind: 'bottom' });
});

it('persists bottom following when the last newer page needs no scrolling', async () => {
  const { setBindingMock } = await import('../../../test/mocks/bindings-app');
  const { setThreadScrollSnapshot } = await import('../../utils/threadScrollSnapshots');
  const thread = makeThread({ id: 'short-newer-page' });
  const items = Array.from({ length: 45 }, (_, i) => makeItem({
    id: `long-${i}`, threadId: thread.id, turnIndex: i,
    summary: 'History text. '.repeat(100),
  }));
  const { pane, scrollEl, host } = await mountTimeline(thread.id, items, { stableFrames: 8, frameBudget: 600, epsilonPx: 2 });
  const short = makeItem({ id: 'short', threadId: thread.id, turnIndex: 0, itemIndex: 0, summary: 'A short window.' });
  setBindingMock('ListThreadSliceAround', async () => ({ items: [short], runs: [],
    oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: short.id },
    newestCursor: { turnIndex: 0, itemIndex: 0, itemId: short.id },
    hasMoreOlder: false, hasMoreNewer: true, oldestTurnIndex: 0, newestTurnIndex: 0 }));
  let finishNewer!: (value: unknown) => void;
  setBindingMock('ListItemsAfterCursor', () => new Promise(resolve => { finishNewer = resolve; }));
  await pane.refreshFromBackend(true);
  await waitFor(() => scrollEl.scrollHeight === scrollEl.clientHeight, 'short window fits viewport');
  await waitFor(() => !!host.textContent?.includes('Jump to bottom'), 'newer history remains');
  setThreadScrollSnapshot(thread.id, { kind: 'anchor', itemId: short.id, offsetTop: 0 });
  await waitFor(() => !!finishNewer, 'automatic newer page requested');
  finishNewer({ items: [], runs: [], hasMoreNewer: false });
  await waitFor(() => !host.textContent?.includes('Jump to bottom'), 'last page reached');
  expect(getThreadScrollSnapshot(thread.id)).toEqual({ kind: 'bottom' });
});
