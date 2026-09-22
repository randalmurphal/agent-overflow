import { expect, it } from 'vitest';
import { createActivityRunLookup } from './activityRunLookup';
import { activityRunLoadedItems } from './activityRunLoadedItems';
import { activityRunRow as row, activityRunStub as stub } from '../../test/helpers/activityRuns';
import { foldPageStub } from './activityRunStubs';
import type { ActivityRunRecords } from './activityRunStubs';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';

const bounds = { oldest: null, newest: null };
const filter = (items: ReturnType<typeof row>[], stubs: ActivityRunStub[], previous = new Map<string, string>()) =>
  activityRunLoadedItems(items, bounds, createActivityRunLookup(new Map(), stubs), previous);

it('keeps loaded and retained-context membership distinct with overlapping stale descriptions', () => {
  const items = Array.from({ length: 8 }, (_, i) => row(`r${i}`, i));
  const older = stub({ firstItemId: 'r0', lastItemId: 'r7', firstItemIndex: 0, lastItemIndex: 7,
    loadedFirstItemId: 'r3', loadedLastItemId: 'r5' });
  const newer = stub({ firstItemId: 'r2', lastItemId: 'r6', firstItemIndex: 2, lastItemIndex: 6,
    loadedFirstItemId: 'r2', loadedLastItemId: 'r6' });
  expect(filter(items, [older, newer]).map(item => item.id)).toEqual(['r3', 'r4', 'r5']);
  expect(filter(items, [newer, older]).map(item => item.id)).toEqual(['r2', 'r3', 'r4', 'r5', 'r6']);
  expect(filter(items, [older, newer], new Map([['r0', 'r0']])).map(item => item.id)).toEqual(['r0', 'r3', 'r4', 'r5']);
});

it('replaces a prior description without changing its precedence and preserves input order', () => {
  const items = [row('d', 4), row('a', 1), row('c', 3), row('b', 2)];
  const records: ActivityRunRecords = new Map();
  const original = stub({ firstItemId: 'a', lastItemId: 'd', firstItemIndex: 1, lastItemIndex: 4,
    loadedFirstItemId: 'a', loadedLastItemId: 'd' });
  foldPageStub(records, original, null);
  const replacement = { ...original, loadedFirstItemId: 'b', loadedLastItemId: 'c' };
  expect(activityRunLoadedItems(items, bounds, createActivityRunLookup(records, [replacement]), new Map()).map(item => item.id)).toEqual(['c', 'b']);
  expect(activityRunLoadedItems(items, bounds, createActivityRunLookup(records, []), new Map())).toBe(items);
});

it('handles empty spans, missing edges, scoped members, and retained children', () => {
  const items = [row('a', 1, { parentId: 'scope' }), row('b', 2, { parentId: 'scope' }), row('child', 3, { parentId: 'a' })];
  const empty = stub({ firstItemId: 'a', firstItemIndex: 1, loadedFirstItemId: '', loadedLastItemId: '' });
  expect(filter(items, [empty])).toBe(items);
  const scoped = (item: ReturnType<typeof row>) => item.parentId === 'scope';
  expect(activityRunLoadedItems(items, bounds, createActivityRunLookup(new Map(), [empty]), new Map(), scoped).map(item => item.id)).toEqual(['child']);
  const oneEdge = { ...empty, loadedLastItemId: 'a' };
  expect(activityRunLoadedItems(items, bounds, createActivityRunLookup(new Map(), [oneEdge]), new Map(), scoped).map(item => item.id)).toEqual(['a', 'child']);
});

it('does not rescan every run description for each loaded row', () => {
  const count = 1000;
  let reads = 0;
  let positions = 0;
  const items = Array.from({ length: count }, (_, i) => ({ ...row(`r${i}`, i * 2),
    get turnIndex() { positions += 1; return 0; },
  }));
  const stubs = items.map((item, i) => ({ ...stub({ firstItemId: item.id, lastItemId: item.id,
    loadedFirstItemId: item.id, loadedLastItemId: item.id, lastItemIndex: i * 2 }),
    get firstItemIndex() { reads += 1; return i * 2; },
  }));
  expect(filter(items, stubs)).toBe(items);
  expect(reads).toBeLessThan(count * 30);
  expect(positions).toBeLessThan(count * 40);
});

it('matches insertion-order lookup across overlapping ranges and turn boundaries', () => {
  let seed = 421;
  const random = (max: number) => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed % max; };
  const stubs = Array.from({ length: 100 }, (_, i) => {
    const first = random(300);
    const last = first + random(80);
    return stub({ firstItemId: `run${i}`, firstTurnIndex: Math.floor(first / 20), firstItemIndex: first % 20,
      lastTurnIndex: Math.floor(last / 20), lastItemIndex: last % 20 });
  });
  const lookup = createActivityRunLookup(new Map(), stubs);
  for (let index = -1; index <= 400; index += 1) {
    const item = { turnIndex: Math.floor(index / 20), itemIndex: index % 20 };
    const expected = stubs.find(run => index >= run.firstTurnIndex * 20 + run.firstItemIndex
      && index <= run.lastTurnIndex * 20 + run.lastItemIndex);
    expect(lookup.find(item)).toBe(expected);
  }
});
