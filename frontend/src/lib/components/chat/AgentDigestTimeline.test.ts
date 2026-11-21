// The digest regroups its scope only when the scope's structure changes:
// settled content and patches reach the mounted rows through their boxes.
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import AgentDigestTimeline from './AgentDigestTimeline.svelte';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { applyItemStreamEvent, flushItemEventQueue } from '../../stores/eventsItemStream';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { Item } from '../../types/models';
import type { PagedItems } from '../../../../bindings/agent-overflow/internal/store/models';

vi.mock('../../utils/subagentGrouping', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../utils/subagentGrouping')>();
  return { ...actual, groupItemsBySubagent: vi.fn(actual.groupItemsBySubagent) };
});

const threadId = 'digest-thread';
const launch = makeItem({ id: 'launch', threadId, itemIndex: 0, kind: 'tool_call', toolName: 'Agent', status: 'running', isBackground: true });
const tool = (id: string, itemIndex: number, extra: Partial<Item> = {}) => makeItem({
  id, threadId, parentId: launch.id, itemIndex, kind: 'tool_call', toolName: 'Bash', status: 'running', summary: `${id} running`, ...extra,
});
const push = (item: Item) => { applyItemStreamEvent({ action: 'upsert', threadId, item }); flushItemEventQueue(); };

beforeEach(async () => {
  installTimelineScopeCapability();
  resetBindingMocks();
  resetPanesForTest();
  setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'expanded' }));
  await loadSettings();
});
afterEach(() => { cleanup(); resetPanesForTest(); vi.unstubAllGlobals(); });

it('regroups on structural changes only', async () => {
  const rows = [tool('t1', 1), tool('t2', 2), tool('t3', 3)];
  installPaneMocks([launch, ...rows]);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: threadId }));
  const view = render(AgentDigestTimeline, { props: { pane, scopeId: launch.id, id: 'digest', viewKey: 'tray', live: true } });
  const body = view.getByTestId('subagent-group-body');
  await waitFor(() => expect(body.querySelectorAll('[data-item-id]')).toHaveLength(3));
  const grouping = vi.mocked(groupItemsBySubagent);
  grouping.mockClear();

  for (const [index, row] of rows.entries()) {
    push({ ...row, status: 'completed', summary: `${row.id} done`, updatedAt: row.updatedAt + 1 });
    applyItemStreamEvent({ action: 'patch', threadId, itemId: row.id, kind: row.kind, patch: { summary: `${row.id} patched`, rev: index + 10 } });
    flushItemEventQueue();
  }
  await tick();
  expect(grouping).not.toHaveBeenCalled();
  await waitFor(() => expect(body.querySelector('[data-item-id="t3"]')?.textContent).toContain('t3 patched'));

  push(tool('t4', 4));
  await waitFor(() => expect(body.querySelectorAll('[data-item-id]')).toHaveLength(4));
  expect(grouping).toHaveBeenCalled();
});

// Both boundary rows load when they reach the clip, so a short digest asks
// for its earlier and its newer activities at once. Each page extends its
// own edge of the scope's window, and both land.
it('lands earlier and newer activities asked for together', async () => {
  installPaneMocks([launch, tool('t2', 2), tool('t3', 3)]);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: threadId }));
  const page = await getBindingMock('ListThreadSliceAround')!(threadId, '', 200, { selection: { scopeRootId: launch.id, tools: true } }) as PagedItems;
  setBindingMock('SyncThreadWindow', async () => ({
    status: 'stale', generation: 'test-generation', page: { ...page, hasMore: true, hasMoreOlder: true, hasMoreNewer: true },
  }));
  const older: ((page: unknown) => void)[] = [];
  const newer: ((page: unknown) => void)[] = [];
  setBindingMock('ListItemsBeforeCursor', () => new Promise(resolve => older.push(resolve)));
  setBindingMock('ListItemsAfterCursor', () => new Promise(resolve => newer.push(resolve)));
  const view = render(AgentDigestTimeline, { props: { pane, scopeId: launch.id, id: 'digest', viewKey: 'tray', live: true } });
  await waitFor(() => expect(view.getByText('Load earlier activities')).toBeEnabled());
  await waitFor(() => expect(view.getByText('Load newer activities')).toBeEnabled());

  await fireEvent.click(view.getByText('Load earlier activities'));
  await fireEvent.click(view.getByText('Load newer activities'));
  await waitFor(() => expect([older.length, newer.length]).toEqual([1, 1]));
  older[0]({ items: [tool('t1', 1)], hasMore: false, hasMoreOlder: false, hasMoreNewer: false });
  newer[0]({ items: [tool('t4', 4)], hasMore: false, hasMoreOlder: false, hasMoreNewer: false });

  const body = view.getByTestId('subagent-group-body');
  await waitFor(() => expect([...body.querySelectorAll('[data-item-id]')].map(row => row.getAttribute('data-item-id')))
    .toEqual(['t1', 't2', 't3', 't4']));
  expect(view.queryByText('Load earlier activities')).toBeNull();
  expect(view.queryByText('Load newer activities')).toBeNull();
});

// Stands in for the browser's observer so a test decides when a boundary
// enters and leaves the clip.
class BoundaryObserver {
  static all: BoundaryObserver[] = [];
  readonly targets = new Set<Element>();
  constructor(private readonly callback: IntersectionObserverCallback) { BoundaryObserver.all.push(this); }
  observe(target: Element) { this.targets.add(target); }
  unobserve(target: Element) { this.targets.delete(target); }
  disconnect() { this.targets.clear(); }
  takeRecords(): IntersectionObserverEntry[] { return []; }
  static show(target: Element, isIntersecting: boolean) {
    for (const observer of BoundaryObserver.all) {
      if (!observer.targets.has(target)) continue;
      observer.callback([{ target, isIntersecting } as IntersectionObserverEntry], observer as unknown as IntersectionObserver);
    }
  }
}

// A boundary reaching the clip is automatic paging, under the rule
// auto-load and the viewport fill follow: an edge whose page failed waits
// until the edge moves, and a click still asks.
it.each([
  { label: 'earlier', binding: 'ListItemsBeforeCursor', moved: tool('t1', 1) },
  { label: 'newer', binding: 'ListItemsAfterCursor', moved: tool('t4', 4) },
] as const)('after a failed page the $label boundary waits for its edge to move before loading on sight', async ({ label, binding, moved }) => {
  BoundaryObserver.all = [];
  vi.stubGlobal('IntersectionObserver', BoundaryObserver);
  installPaneMocks([launch, tool('t2', 2), tool('t3', 3)]);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: threadId }));
  const page = await getBindingMock('ListThreadSliceAround')!(threadId, '', 200, { selection: { scopeRootId: launch.id, tools: true } }) as PagedItems;
  setBindingMock('SyncThreadWindow', async () => ({
    status: 'stale', generation: 'test-generation', page: { ...page, hasMore: true, hasMoreOlder: true, hasMoreNewer: true },
  }));
  const request = setBindingMock(binding, async () => { throw new Error('page failed'); });
  const view = render(AgentDigestTimeline, { props: { pane, scopeId: launch.id, id: 'digest', viewKey: 'tray', live: true } });
  const text = `Load ${label} activities`;
  await waitFor(() => expect(view.getByText(text)).toBeEnabled());
  const passBy = async () => {
    BoundaryObserver.show(view.getByText(text), false);
    BoundaryObserver.show(view.getByText(text), true);
    await new Promise(resolve => setTimeout(resolve, 0));
    await waitFor(() => expect(view.getByText(text)).toBeEnabled());
  };

  BoundaryObserver.show(view.getByText(text), true);
  await waitFor(() => expect(request).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(view.getByText(text)).toBeEnabled());
  await passBy();
  expect(request).toHaveBeenCalledTimes(1);

  // A click is the reader asking: it retries, and failing again keeps
  // the edge where the observer leaves it alone.
  await fireEvent.click(view.getByText(text));
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(view.getByText(text)).toBeEnabled());
  await passBy();
  expect(request).toHaveBeenCalledTimes(2);

  // A page that moves the edge hands it back to the observer.
  request.mockImplementation(async () => ({ items: [moved], hasMore: true, hasMoreOlder: true, hasMoreNewer: true }));
  await fireEvent.click(view.getByText(text));
  await waitFor(() => expect(view.getByTestId('subagent-group-body').querySelector(`[data-item-id="${moved.id}"]`)).not.toBeNull());
  await waitFor(() => expect(view.getByText(text)).toBeEnabled());
  await passBy();
  expect(request).toHaveBeenCalledTimes(4);
});
