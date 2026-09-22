import { afterEach, beforeEach, expect, it } from 'vitest';
import { refreshRetainedRunWindows } from './threadRetainedRunRefresh';
import { groupActivityRunSpans } from '../utils/activityRunSpans';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { ActivityRunStub, PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import type { ThreadPane } from './thread.svelte';
import type { Item } from '../types/models';

const shape = { runWindowRows: 30, inlinePreviews: false, maxBytes: 524288 };
const row = (i: number, overrides: Partial<Item> = {}) => makeItem({ id: `r${i}`, threadId: 't',
  itemIndex: i, turnIndex: 0, kind: 'tool_call', toolName: 'Bash', rev: i + 1, ...overrides });
const cursor = (i: number) => ({ turnIndex: 0, itemIndex: i, itemId: `r${i}` });
function run(first: number, last: number, loadedFirst: number, loadedLast: number) {
  return new ActivityRunStub({ firstItemId: `r${first}`, lastItemId: `r${last}`,
    firstTurnIndex: 0, firstItemIndex: first, lastTurnIndex: 0, lastItemIndex: last,
    loadedFirstItemId: `r${loadedFirst}`, loadedLastItemId: `r${loadedLast}`,
    memberCount: last - first + 1, unshippedBefore: loadedFirst - first, unshippedAfter: last - loadedLast,
    unshippedDigest: '0000000000000000' });
}
function page(items: Item[], runs: ActivityRunStub[]) {
  return new PagedItems({ items, runs,
    oldestCursor: runs.length ? cursor(runs[0].firstItemIndex) : cursor(items[0]?.itemIndex ?? 0),
    newestCursor: runs.length ? cursor(runs.at(-1)!.lastItemIndex) : cursor(items.at(-1)?.itemIndex ?? 0),
    hasMoreOlder: false, hasMoreNewer: false, oldestTurnIndex: 0, newestTurnIndex: 0 });
}
const rows = (first: number, count: number) => Array.from({ length: count }, (_, i) => row(first + i));
let pane: ThreadPane;
beforeEach(async () => { installThreadPaneTestEnv(); pane = await buildPane(makeThread({ id: 't' })); });
afterEach(() => pane.clear());

it('keeps unchanged and default-sized windows free of extra reads', async () => {
  const read = setBindingMock('ListItemsBeforeCursor', async () => { throw new Error('unnecessary read'); });
  const fresh = page(rows(70, 30), [run(0, 99, 70, 99)]);
  expect(await refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(rows(60, 30)), shape, () => true)).toBe(fresh);
  const whole = page(rows(0, 100), [run(0, 99, 0, 99)]);
  expect(await refreshRetainedRunWindows('t', whole, groupActivityRunSpans(rows(0, 100)), shape, () => true)).toBe(whole);
  expect(read).not.toHaveBeenCalled();
});

it('walks shipped cursors across a run larger than one response and restates the whole span', async () => {
  const all = rows(0, 650);
  const fresh = page(all.slice(-30), [run(0, 649, 620, 649)]);
  const read = setBindingMock('ListItemsBeforeCursor', async (_thread, before: { itemIndex: number }, budget, requestShape) => {
    expect(budget).toBe(1);
    expect(requestShape).toMatchObject({ runWindowRows: 200, maxBytes: shape.maxBytes });
    const last = before.itemIndex - 1;
    const first = Math.max(0, last - 199);
    return page(all.slice(first, last + 1), [run(0, 649, first, last)]);
  });
  const restate = setBindingMock('ListActivityRunMembers', async (_thread, request) => {
    expect(request).toMatchObject({ loadedFirstItemId: 'r0', loadedLastItemId: 'r649', limit: 0 });
    return { items: [], stub: run(0, 649, 0, 649) };
  });
  const result = await refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(all), shape, () => true);
  expect(result.items.map(item => item.id)).toEqual(all.map(item => item.id));
  expect(result.runs).toEqual([run(0, 649, 0, 649)]);
  expect(read).toHaveBeenCalledTimes(4);
  expect(restate).toHaveBeenCalledOnce();
});

it('re-reads a run split by prose and removes a deleted member', async () => {
  const previous = rows(0, 70);
  const middle = row(35, { kind: 'assistant_text', toolName: '', summary: 'new divider' });
  const left = rows(0, 34);
  const right = rows(36, 34);
  const fresh = page([...left.slice(-30), middle, ...right.slice(-30)], [run(0, 33, 4, 33), run(36, 69, 40, 69)]);
  setBindingMock('ListItemsBeforeCursor', async (_thread, before: { itemIndex: number }) => {
    if (before.itemIndex > 36) return page(right, [run(36, 69, 36, 69)]);
    if (before.itemIndex > 35) return page([middle], []);
    return page(left, [run(0, 33, 0, 33)]);
  });
  setBindingMock('ListActivityRunMembers', async (_thread, request: { runFirstItemId: string }) => ({ items: [],
    stub: request.runFirstItemId === 'r0' ? run(0, 33, 0, 33) : run(36, 69, 36, 69) }));
  const result = await refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(previous), shape, () => true);
  expect(result.items.map(item => item.id)).toEqual([...left, middle, ...right].map(item => item.id));
  expect(result.items.some(item => item.id === 'r34')).toBe(false);
  expect(result.runs).toHaveLength(2);
});

it('combines retained ranges when two runs joined', async () => {
  const previous = [...rows(0, 35), row(35, { kind: 'assistant_text' }), ...rows(36, 34)];
  const all = rows(0, 70);
  const fresh = page(all.slice(-30), [run(0, 69, 40, 69)]);
  const read = setBindingMock('ListItemsBeforeCursor', async () => page(all, [run(0, 69, 0, 69)]));
  setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: run(0, 69, 0, 69) }));
  const result = await refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(previous), shape, () => true);
  expect(result.items.map(item => item.id)).toEqual(all.map(item => item.id));
  expect(read).toHaveBeenCalledOnce();
});

it('discards cancelled reads without starting follow-up requests', async () => {
  const fresh = page(rows(20, 30), [run(0, 49, 20, 49)]);
  let current = true;
  setBindingMock('ListItemsBeforeCursor', async () => { current = false; return page(rows(0, 50), [run(0, 49, 0, 49)]); });
  const restate = setBindingMock('ListActivityRunMembers', async () => { throw new Error('late request'); });
  expect(await refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(rows(0, 50)), shape, () => current)).toBe(fresh);
  expect(restate).not.toHaveBeenCalled();
});

it('rejects a non-advancing cursor response without looping', async () => {
  const fresh = page(rows(20, 30), [run(0, 49, 20, 49)]);
  const read = setBindingMock('ListItemsBeforeCursor', async () => page([row(50)], [run(0, 50, 50, 50)]));
  await expect(refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(rows(0, 50)), shape, () => true)).rejects.toThrow('did not advance');
  expect(read).toHaveBeenCalledOnce();
});

it('rejects a span changed during its reads without publishing partial history', async () => {
  const fresh = page(rows(20, 30), [run(0, 49, 20, 49)]);
  setBindingMock('ListItemsBeforeCursor', async () => page(rows(0, 50), [run(0, 49, 0, 49)]));
  setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: { ...run(0, 49, 0, 49), memberCount: 51 } }));
  await expect(refreshRetainedRunWindows('t', fresh, groupActivityRunSpans(rows(0, 50)), shape, () => true)).rejects.toThrow('changed during refresh');
  expect(fresh.items).toHaveLength(30);
});

it('does not repeatedly merge the accumulated window when byte limits produce small pages', async () => {
  let reads = 0;
  const all = rows(0, 120).map((item, index) => ({ ...item, get id() { reads += 1; return `r${index}`; } }));
  const fresh = page(all.slice(-30), [run(0, 119, 90, 119)]);
  const previous = groupActivityRunSpans(all);
  setBindingMock('ListItemsBeforeCursor', async (_thread, before: { itemIndex: number }) => {
    const index = before.itemIndex - 1;
    return page([all[index]], [run(0, 119, index, index)]);
  });
  setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: run(0, 119, 0, 119) }));
  reads = 0;
  const result = await refreshRetainedRunWindows('t', fresh, previous, shape, () => true);
  expect(result.items).toHaveLength(120);
  expect(reads).toBeLessThan(120 * 20);
});
