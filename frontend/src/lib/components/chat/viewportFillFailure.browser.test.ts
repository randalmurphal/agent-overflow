// The viewport fill (timelinePaging.ts) pages a window too short to scroll
// into an auto-load zone. Its quiet pass reruns on every scheduler lap
// (a started load counts as a geometry change, timelineQuietWork.ts), on
// every new row and on every scroll end. A page that failed must not go out
// again from the same edge until that edge moves, the rule auto-load
// follows; otherwise a failing backend gets the same request, and the reader
// the same error toast, about ten times a second.
import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { mount, unmount } from 'svelte';
import MessageTimeline from './MessageTimeline.svelte';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { cursorFromItem } from '../../stores/threadItems';
import type { Item } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { setupTimelineHarness } from '../../../test/helpers/timelineBrowserHarness';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

setupTimelineHarness();

const THREAD_ID = 'fill-failure';
const VIEWPORT_H_PX = 600;
// Before the fix the request went out again about every 100ms; a scroll end
// lands about 150ms after the last scroll event.
const REPEAT_WINDOW_MS = 1500;

const hosts: { app: object; host: HTMLElement }[] = [];
afterEach(() => {
  for (const { app, host } of hosts.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function row(turnIndex: number): Item {
  return makeItem({
    id: `row-${turnIndex}`,
    threadId: THREAD_ID,
    turnIndex,
    itemIndex: 0,
    summary: `Row ${turnIndex}. ` + 'A paragraph long enough to wrap over several lines of the timeline. '.repeat(4),
  });
}

type Edge = 'older' | 'newer';

// A window of six rows: taller than the viewport, so it scrolls, and
// shorter than the viewport plus both auto-load zones, so only the fill
// pages it. More history lies past `edge` and nowhere else.
async function openShortWindow(edge: Edge) {
  const items = [10, 11, 12, 13, 14, 15].map(row);
  installPaneMocks(items);
  setBindingMock('ListThreadSliceAround', async () => ({
    items,
    oldestCursor: cursorFromItem(items[0]),
    newestCursor: cursorFromItem(items.at(-1)!),
    oldestTurnIndex: 10,
    newestTurnIndex: 15,
    hasMore: true,
    hasMoreOlder: edge === 'older',
    hasMoreNewer: edge === 'newer',
    runs: [],
  }));
  // Every page at the edge fails until the test says otherwise.
  let answer: (() => object) | null = null;
  const method = edge === 'older' ? 'ListItemsBeforeCursor' : 'ListItemsAfterCursor';
  const pages = setBindingMock(method, async () => {
    if (!answer) throw new Error(`${edge} page failed`);
    return answer();
  });
  const pane: ThreadPane = createThreadPane({ paneId: 'main' });
  await pane.switchThread(makeThread({ id: THREAD_ID }));
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: 0; width: 800px; height: ${VIEWPORT_H_PX}px; background: #111;`;
  document.body.appendChild(host);
  hosts.push({ app: mount(MessageTimeline, { target: host, props: { pane } }), host });
  await waitFor(() => host.querySelector('[data-testid="message-timeline-scroll"]') !== null, 'scroll container');
  const scrollEl = host.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
  return {
    pane, host, scrollEl, pages,
    answerWith(page: () => object) { answer = page; },
  };
}

async function settles(predicate: () => boolean, ms: number): Promise<boolean> {
  const deadline = performance.now() + ms;
  while (performance.now() < deadline) {
    if (predicate()) return true;
    await raf();
  }
  return predicate();
}

describe('a viewport fill whose page failed', () => {
  it.each([
    ['older', 'by itself'],
    ['newer', 'by itself'],
    ['older', 'on a new row'],
    ['older', 'on a scroll end'],
    ['newer', 'on a scroll end'],
  ] as const)('does not send the %s page again %s until that edge moves', async (edge, trigger) => {
    const t = await openShortWindow(edge);
    const loading = () => (edge === 'older' ? t.pane.loadingOlder : t.pane.loadingNewer);
    await waitFor(() => t.pages.mock.calls.length === 1 && !loading(), 'the fill to send its first page and fail');
    // The geometry this case is about: scrollable, and too short for the
    // auto-load zones.
    expect(t.scrollEl.scrollHeight).toBeGreaterThan(t.scrollEl.clientHeight);
    expect(t.scrollEl.scrollHeight).toBeLessThan(t.scrollEl.clientHeight + 1600);

    if (trigger === 'on a new row') {
      t.pane.applyProviderItemUpserts([row(16)]);
      await waitFor(() => t.pane.items.some((item) => item.id === 'row-16'), 'the new row');
    } else if (trigger === 'on a scroll end') {
      t.scrollEl.scrollTop = Math.max(0, t.scrollEl.scrollTop - 120);
    }
    expect(await settles(() => t.pages.mock.calls.length > 1, REPEAT_WINDOW_MS), 'the failed page went out again').toBe(false);

    // Once the edge moves, the fill pages it again: the explicit button
    // lands a page, and the fill's next pass asks for the one after it.
    const pageAfter = (turnIndex: number) => ({
      items: [row(turnIndex)],
      hasMore: true,
      hasMoreOlder: edge === 'older',
      hasMoreNewer: edge === 'newer',
    });
    let next = edge === 'older' ? 9 : 17;
    t.answerWith(() => pageAfter(edge === 'older' ? next-- : next++));
    (t.host.querySelector(`[data-testid="load-${edge}-messages"]`) as HTMLElement).click();
    await waitFor(() => t.pages.mock.calls.length >= 3, 'the fill to page the moved edge');
  });
});
