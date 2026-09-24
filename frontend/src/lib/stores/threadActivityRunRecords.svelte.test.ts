// The registry's run-record half (docs/architecture/timeline-window-pages.md
// §6): folding a page's stubs against the span the pane holds, routing a
// pushed row that lands inside a run the pane holds only part of,
// accounting for a window cut, and fetching members on demand.
//
// Separate from `threadActivityRuns.svelte.test.ts`, which drives the
// registry through projection passes and owns run IDENTITY and collapse.
// This file drives it through the pane's window instead.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { windowDigest } from './threadWindowDigest';
import { activityRunRow, activityRunProse, activityRunStub } from '../../test/helpers/activityRuns';
import { formatFnv1a64 } from '../utils/fnv1a';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Item } from '../types/models';
import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';

/** The pane's window, mutable so the registry's getters see the change. */
const registries: ReturnType<typeof createThreadActivityRuns>[] = [];
afterEach(() => { for (const runs of registries.splice(0)) runs.clear(); });

function fixture(initial: Item[] = [], bounds = { oldest: null, newest: null } as import('./threadActivityRuns.svelte').RunWindowBounds, selection?: TimelineSelection) {
  let items = initial;
  const mounted: { rows: Item[]; dropIds: string[] }[] = [];
  const failures: { message: string; silent: boolean }[] = [];
  const reloads = vi.fn(async () => {});
  const runs = createThreadActivityRuns({
    selection: () => selection ?? {},
    defaultCollapsed: () => false,
    windowRows: () => 30,
    windowVerified: () => true,
    scrollController: () => null,
    items: () => items,
    threadId: () => 't',
    pageShape: () => ({ inlinePreviews: true, runWindowRows: 3, maxBytes: 1024 }),
    mountRunMembers: (incoming, dropIds) => {
      mounted.push({ rows: [...incoming], dropIds: [...dropIds] });
      const kept = items.filter((item) => !dropIds.has(item.id));
      const byId = new Map(kept.map((item) => [item.id, item]));
      for (const item of incoming) byId.set(item.id, item);
      items = [...byId.values()].sort((a, b) => a.itemIndex - b.itemIndex);
    },
    windowBounds: () => bounds,
    reloadWindow: reloads,
    reportFetchFailure: (message, _err, silent) => failures.push({ message, silent }),
  });
  registries.push(runs);
  return {
    runs,
    mounted,
    failures,
    reloads,
    get items() {
      return items;
    },
    setItems(next: Item[]) {
      items = next;
    },
  };
}

beforeEach(() => {
  resetBindingMocks();
});

describe('syncRunSpans', () => {
  it('folds a page stub against the span the pane holds', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3), activityRunProse('p1', 4)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    expect(f.runs.heldRunFold()).toEqual({
      count: 2,
      digest: expect.objectContaining({}),
    });
    expect(formatFnv1a64(f.runs.heldRunFold()!.digest)).toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }]),
    );
  });

  it('states no held window when a stub describes a span the pane does not hold', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    expect(f.runs.heldRunFold()).toBeNull();
  });

  it('drops a record whose run left the window', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    expect(f.runs.snapshotStubs()).toHaveLength(1);

    const next = [activityRunProse('p0', 0)];
    f.setItems(next);
    f.runs.syncRunSpans(next);
    expect(f.runs.snapshotStubs()).toEqual([]);
  });

  it('re-points a record whose span grew, and goes dirty', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const revision = f.runs.revision;
    const windowRevision = f.runs.windowRevision;
    const grown = [...items, activityRunRow('e', 6)];
    f.setItems(grown);
    f.runs.syncRunSpans(grown);
    expect(f.runs.heldRunFold()).toBeNull();
    expect(f.runs.snapshotStubs()).toBeNull();
    expect(f.runs.revision).toBe(revision + 1);
    expect(f.runs.windowRevision).toBe(windowRevision + 1);
  });

  it('moves no revision when a resync finds every held span unchanged', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4), activityRunProse('p1', 8)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const revision = f.runs.revision;
    const windowRevision = f.runs.windowRevision;

    f.runs.syncRunSpans(items);
    const appended = [...items, activityRunProse('p2', 9), activityRunRow('z', 10)];
    f.setItems(appended);
    f.runs.syncRunSpans(appended);

    expect(f.runs.revision).toBe(revision);
    expect(f.runs.windowRevision).toBe(windowRevision);
    expect(f.runs.snapshotStubs()).toHaveLength(1);
  });

  it('moves the revisions when a held run gains a member inside its span', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 2), activityRunRow('d', 4), activityRunProse('p1', 8)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const windowRevision = f.runs.windowRevision;
    const grown = [items[0], items[1], activityRunRow('c', 3), ...items.slice(2)];
    f.setItems(grown);
    f.runs.syncRunSpans(grown);
    expect(f.runs.isLoadedMember('c')).toBe(true);
    expect(f.runs.windowRevision).toBe(windowRevision + 1);
  });

  it('moves no revision for a window that holds no record', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2)];
    const f = fixture(items);
    const revision = f.runs.revision;
    const windowRevision = f.runs.windowRevision;
    f.runs.syncRunSpans(items);
    f.runs.applyWindowCut(items, items.slice(1));
    expect(f.runs.revision).toBe(revision);
    expect(f.runs.windowRevision).toBe(windowRevision);
  });

  it('moves the revisions once when a record leaves, and not on the next resync', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const revision = f.runs.revision;
    const next = [activityRunProse('p0', 0)];
    f.setItems(next);
    f.runs.syncRunSpans(next);
    f.runs.syncRunSpans(next);
    expect(f.runs.revision).toBe(revision + 1);
    expect(f.runs.isLoadedMember('b')).toBe(false);
  });
});

describe('runCoveringUnshipped', () => {
  const items = [activityRunProse('p0', 0), activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4), activityRunProse('p1', 8)];

  function held() {
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    return f;
  }

  // The stub places the run's edges at 1 (a) and 5 (e); the window holds
  // b..d at 2..4 with prose either side. Membership is "between the
  // edges": the side counts say a side has something to fetch, the edge
  // coordinates say whether a given row is on it.
  it('claims a row between the run\'s first edge and its loaded span', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 1))).toBe('a');
  });

  it('claims a row between its loaded span and the run\'s last edge', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 5))).toBe('a');
  });

  it('claims nothing inside the loaded span, or outside the run', () => {
    const f = held();
    // Between two loaded members there is nothing unshipped to belong to.
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 3))).toBeNull();
    // Past an edge is outside the run, whether or not the window holds
    // anything there: 6 sits between e and the prose at 8, 9 past it.
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 6))).toBeNull();
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 9))).toBeNull();
    expect(f.runs.runCoveringUnshipped(activityRunProse('older', 0))).toBeNull();
  });

  it('claims nothing on a side the stub says is empty', () => {
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub({ unshippedAfter: 0, unshippedDigest: windowDigest([{ id: 'a', rev: 1 }]) })]);
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 5))).toBeNull();
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 1))).toBe('a');
  });

  it('claims nothing for a row a page would not return', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(activityRunRow('child', 6, { parentId: 'b' }))).toBeNull();
  });
});

describe('applyWindowCut', () => {
  it('moves no revision for a cut that leaves every held run whole', () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4), activityRunProse('p1', 8), activityRunProse('p2', 9)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const revision = f.runs.revision;
    const windowRevision = f.runs.windowRevision;
    f.runs.applyWindowCut(items, items.slice(0, -1));
    expect(f.runs.revision).toBe(revision);
    expect(f.runs.windowRevision).toBe(windowRevision);
  });

  it('sheds the members a cut dropped from a run\'s older side', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);

    const next = items.slice(1);
    f.runs.applyWindowCut(items, next);
    f.setItems(next);
    f.runs.syncRunSpans(next);

    // The record survives and now counts the shed row as earlier history.
    const [folded] = f.runs.snapshotStubs()!;
    expect(folded.unshippedBefore).toBe(2);
    expect(folded.loadedFirstItemId).toBe('c');
    expect(formatFnv1a64(f.runs.heldRunFold()!.digest)).toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }, { id: 'b', rev: 2 }]),
    );
  });

  it('drops a record whose run left entirely', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2), activityRunProse('p1', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub({ loadedLastItemId: 'c', unshippedAfter: 2 })]);

    const next = items.slice(2);
    f.runs.applyWindowCut(items, next);
    f.setItems(next);
    f.runs.syncRunSpans(next);
    expect(f.runs.snapshotStubs()).toEqual([]);
  });

  it('keeps a record dirty when a cut took members off the newer side', () => {
    // Only reachable for a run larger than the whole retention target;
    // the shed list cannot hold newer-side members, so a refresh restates
    // the run and the pane describes no window meanwhile.
    const items = [activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);

    const next = items.slice(0, 2);
    f.runs.applyWindowCut(items, next);
    f.setItems(next);
    expect(f.runs.heldRunFold()).toBeNull();
  });
});

function heldRun() {
  const items = [activityRunProse('p0', 0), activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
  const f = fixture(items);
  f.runs.syncRunSpans(items, [activityRunStub()]);
  // The registry links a run entry to its record through a projection
  // pass, which is how a caller holding a runId reaches the record.
  f.runs.beginPass();
  const resolved = f.runs.resolve([['b'], ['c'], ['d']], 't');
  f.runs.endPass();
  return { f, runId: resolved.runId, resolved };
}

describe('fetchMembers', () => {

  it('stamps the record\'s counts onto the resolved node', () => {
    const { resolved } = heldRun();
    expect(resolved).toMatchObject({
      memberCount: 5,
      unshippedBefore: 1,
      unshippedAfter: 1,
      loadedFirstItemId: 'b',
      loadedLastItemId: 'd',
    });
  });

  it('hands the header the record\'s facts, and nothing for a run held whole', () => {
    const { f, runId } = heldRun();
    expect(f.runs.summaryFacts(runId)).toMatchObject({
      memberCount: 5,
      unshippedBefore: 1,
      unshippedAfter: 1,
      shed: [],
    });
    const whole = fixture([activityRunRow('b', 1)]);
    whole.runs.beginPass();
    const resolved = whole.runs.resolve([['b']], 't');
    whole.runs.endPass();
    expect(whole.runs.summaryFacts(resolved.runId)).toBeNull();
  });

  it('bumps the revision on every record change, so nodes and headers re-derive', async () => {
    const items = [activityRunProse('p0', 0), activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    const start = f.runs.revision;
    f.runs.syncRunSpans(items, [activityRunStub()]);
    expect(f.runs.revision).toBeGreaterThan(start);
    const afterSync = f.runs.revision;
    f.runs.applyWindowCut(items, items.slice(2));
    expect(f.runs.revision).toBeGreaterThan(afterSync);
    const afterCut = f.runs.revision;
    // A stub-only refresh mounts no row, so the revision is the only thing
    // that can tell a header its counts moved.
    f.runs.beginPass();
    const resolved = f.runs.resolve([['c'], ['d']], 't');
    f.runs.endPass();
    setBindingMock('ListActivityRunMembers', async () => ({
      items: [],
      stub: activityRunStub({ loadedFirstItemId: 'c', unshippedBefore: 2, unshippedAfter: 3, memberCount: 7 }),
    }));
    await f.runs.fetchMembers(resolved.runId, { direction: 'before', limit: 0 });
    expect(f.runs.revision).toBeGreaterThan(afterCut);
    expect(f.runs.summaryFacts(resolved.runId)).toMatchObject({ memberCount: 7, unshippedAfter: 3 });
  });

  it('reports a run the pane holds whole as complete', () => {
    const f = fixture([activityRunRow('b', 1)]);
    f.runs.beginPass();
    const resolved = f.runs.resolve([['b']], 't');
    f.runs.endPass();
    expect(resolved).toMatchObject({
      memberCount: 1,
      unshippedBefore: 0,
      unshippedAfter: 0,
      loadedFirstItemId: 'b',
      loadedLastItemId: 'b',
    });
  });

  it('mounts the answer\'s rows and applies its stub', async () => {
    const { f, runId } = heldRun();
    const seen: unknown[] = [];
    setBindingMock('ListActivityRunMembers', async (_threadId: unknown, req: unknown) => {
      seen.push(req);
      return {
        items: [activityRunRow('a', 1, { itemIndex: 0 })],
        stub: activityRunStub({ loadedFirstItemId: 'a', unshippedBefore: 0 }),
      };
    });

    const mountedIds = await f.runs.fetchMembers(runId, { direction: 'before', limit: 5 });
    expect(mountedIds).toEqual(['a', 'b', 'c', 'd']);
    expect(f.mounted).toHaveLength(1);
    expect(f.mounted[0].dropIds).toEqual([]);
    expect(seen[0]).toMatchObject({
      runFirstItemId: 'a',
      loadedFirstItemId: 'b',
      loadedLastItemId: 'd',
      direction: 'before',
      limit: 5,
      shape: { inlinePreviews: true, runWindowRows: 3, maxBytes: 1024 },
    });
    const [folded] = f.runs.snapshotStubs()!;
    expect(folded.unshippedBefore).toBe(0);
  });

  it('replaces the loaded span for an around answer', async () => {
    const { f, runId } = heldRun();
    setBindingMock('ListActivityRunMembers', async () => ({
      items: [activityRunRow('x', 6)],
      stub: activityRunStub({ loadedFirstItemId: 'x', loadedLastItemId: 'x', unshippedBefore: 4, unshippedAfter: 0 }),
    }));
    await f.runs.fetchMembers(runId, { direction: 'around', limit: 5, aroundItemId: 'x' });
    expect(f.mounted[0].dropIds.sort()).toEqual(['b', 'c', 'd']);
  });

  it('brings an unshipped member in by re-centering the run on it', async () => {
    const { f } = heldRun();
    const seen: unknown[] = [];
    setBindingMock('ListActivityRunMembers', async (_threadId: unknown, req: unknown) => {
      seen.push(req);
      return {
        items: [activityRunRow('e', 5)],
        stub: activityRunStub({ loadedFirstItemId: 'e', loadedLastItemId: 'e', unshippedBefore: 4, unshippedAfter: 0 }),
      };
    });
    // `e` sits after the loaded span, inside the run's counted region.
    expect(await f.runs.loadUnshippedMember(activityRunRow('e', 5))).toBe(true);
    expect(seen[0]).toMatchObject({ direction: 'around', aroundItemId: 'e', limit: 30 });
    expect(f.mounted[0].dropIds.sort()).toEqual(['b', 'c', 'd']);
    // A row no held run counts is not this path's to fetch.
    expect(await f.runs.loadUnshippedMember(activityRunRow('far', 50))).toBe(false);
    expect(seen).toHaveLength(1);
  });

  it('does not mistake a row beyond the run\'s edges for an unshipped member', async () => {
    // The window opens ON the run: nothing loaded above b, and the stub
    // counts one member (a) before it. A jump to the user message of that
    // turn, older than a, is a jump into history this run does not cover.
    // Asking the server "around" it would be refused as a stale run and
    // reload the window under the reader. The stub's edge coordinates say
    // where the run starts; the side count only says a is unshipped.
    const items = [activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    const seen: unknown[] = [];
    setBindingMock('ListActivityRunMembers', async (_threadId: unknown, req: unknown) => {
      seen.push(req);
      return { items: [], stub: activityRunStub() };
    });
    expect(await f.runs.loadUnshippedMember(activityRunProse('u', 0))).toBe(false);
    expect(await f.runs.loadUnshippedMember(activityRunProse('later', 9))).toBe(false);
    expect(seen).toHaveLength(0);
    // The edges themselves are members.
    expect(await f.runs.loadUnshippedMember(activityRunRow('a', 1))).toBe(false);
    expect(await f.runs.loadUnshippedMember(activityRunRow('e', 5))).toBe(false);
    expect(seen).toHaveLength(2);
    expect(seen[0]).toMatchObject({ direction: 'around', aroundItemId: 'a' });
    expect(seen[1]).toMatchObject({ direction: 'around', aroundItemId: 'e' });
  });

  it('reloads the window when the server says the run moved', async () => {
    const { f, runId } = heldRun();
    setBindingMock('ListActivityRunMembers', async () => {
      throw Object.assign(new Error('This activity run changed while it was loading.'), { code: 'activity_run_stale' });
    });
    expect(await f.runs.fetchMembers(runId, { direction: 'before', limit: 5 })).toEqual([]);
    expect(f.reloads).toHaveBeenCalledOnce();
    expect(f.failures).toEqual([{ message: 'Activity changed; refreshing history', silent: true }]);
  });

  it('reports any other failure to the reader and keeps the record', async () => {
    const { f, runId } = heldRun();
    setBindingMock('ListActivityRunMembers', async () => {
      throw new Error('transport closed');
    });
    expect(await f.runs.fetchMembers(runId, { direction: 'before', limit: 5 })).toEqual([]);
    expect(f.reloads).not.toHaveBeenCalled();
    expect(f.failures).toEqual([{ message: 'Failed to load activity', silent: false }]);
    expect(f.runs.snapshotStubs()).toHaveLength(1);
  });

  it('answers nothing for a run with no record', async () => {
    const f = fixture([activityRunRow('b', 1)]);
    f.runs.beginPass();
    const resolved = f.runs.resolve([['b']], 't');
    f.runs.endPass();
    expect(await f.runs.fetchMembers(resolved.runId, { direction: 'before', limit: 5 })).toEqual([]);
  });
});

describe('clear', () => {
  it('drops every record with the thread', () => {
    const items = [activityRunRow('b', 1), activityRunRow('c', 2), activityRunRow('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [activityRunStub()]);
    f.runs.clear();
    expect(f.runs.snapshotStubs()).toEqual([]);
    expect(f.runs.runCoveringUnshipped(activityRunRow('new', 0))).toBeNull();
  });
});

it('keeps held agent islands outside the run span and its digest', () => {
  const items = [activityRunRow('old-agent', -100), activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4), activityRunRow('new-agent', 100)];
  const f = fixture(items, {
    oldest: { turnIndex: 0, itemIndex: 1, itemId: 'a' },
    newest: { turnIndex: 0, itemIndex: 5, itemId: 'e' },
  });
  f.runs.syncRunSpans(items, [activityRunStub()]);
  expect(f.runs.snapshotStubs()).toEqual([activityRunStub()]);
  expect(f.runs.isLoadedMember('old-agent')).toBe(false);
  expect(f.runs.isLoadedMember('new-agent')).toBe(false);
});

it('retains a leading notification as a member when the page omitted its predecessor', () => {
  const bell = { ...activityRunRow('b', 2), kind: 'notification' };
  const items = [activityRunProse('before', 0), bell, activityRunRow('c', 3), activityRunRow('d', 4)];
  const f = fixture(items);
  f.runs.syncRunSpans(items, [activityRunStub()]);
  expect(f.runs.isLoadedMember('b')).toBe(true);
  expect(f.runs.snapshotStubs()).toEqual([activityRunStub()]);
});

it('does not overwrite a live append with the span of an older refresh response', async () => {
  const { f, runId } = heldRun();
  let answer!: (value: unknown) => void;
  setBindingMock('ListActivityRunMembers', () => new Promise(resolve => { answer = resolve; }));
  const request = f.runs.fetchMembers(runId, { direction: 'before', limit: 0 });
  const next = [...f.items, activityRunRow('e', 6)];
  f.setItems(next);
  f.runs.syncRunSpans(next);
  answer({ items: [], stub: activityRunStub() });
  await request;
  expect(f.runs.heldRunFold()).toBeNull();
  expect(f.runs.summaryFacts(runId)?.loadedLastItemId).toBe('e');
});

it('does not count retained launch context inside an unshipped interval as loaded history', () => {
  const items = [activityRunRow('a', 1), activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4)];
  const f = fixture(items);
  f.runs.syncRunSpans(items, [activityRunStub()]);
  expect(f.runs.loadedItems(items).map(item => item.id)).toEqual(['b', 'c', 'd']);
  expect(f.runs.snapshotStubs()).toEqual([activityRunStub()]);
  expect(f.runs.heldRunFold()?.count).toBe(2);
});

it('accounts for a cut over a crossing page using the expanded window bounds', () => {
  const items = ['a', 'b', 'c', 'd', 'e'].map((id, index) => activityRunRow(id, index + 1));
  const bounds = { oldest: { turnIndex: 0, itemIndex: 3, itemId: 'c' }, newest: { turnIndex: 0, itemIndex: 5, itemId: 'e' } };
  const f = fixture(items.slice(2), bounds);
  const expanded = { ...bounds, oldest: { turnIndex: 0, itemIndex: 1, itemId: 'a' } };
  f.runs.syncRunSpans(items, [activityRunStub({ loadedFirstItemId: 'a', loadedLastItemId: 'e',
    unshippedBefore: 0, unshippedAfter: 0, unshippedDigest: '0000000000000000' })], expanded);
  const next = items.slice(1);
  f.runs.applyWindowCut(items, next, expanded);
  bounds.oldest = { turnIndex: 0, itemIndex: 2, itemId: 'b' };
  f.setItems(next);
  f.runs.syncRunSpans(next);
  expect(f.runs.heldRunFold()?.count).toBe(1);
  expect(f.runs.snapshotStubs()?.[0]).toMatchObject({ unshippedBefore: 1, loadedFirstItemId: 'b' });
});

it('does not let an older stub consume an invalidation received during its read', async () => {
  vi.useFakeTimers();
  const f = fixture([activityRunRow('b', 2), activityRunRow('c', 3), activityRunRow('d', 4)]);
  try {
    f.runs.syncRunSpans(f.items, [activityRunStub()]);
    let respond!: (answer: unknown) => void;
    const pending = new Promise(resolve => { respond = resolve; });
    const rpc = vi.fn().mockReturnValueOnce(pending).mockResolvedValue({ items: [], stub: activityRunStub({ unshippedFailed: true }) });
    setBindingMock('ListActivityRunMembers', rpc);
    f.runs.markRunDirty('a');
    await vi.advanceTimersByTimeAsync(250);
    expect(rpc).toHaveBeenCalledOnce();
    f.runs.markRunDirty('a'); // A previously omitted member failed after the read began.
    respond({ items: [], stub: activityRunStub() });
    await vi.advanceTimersByTimeAsync(2000);
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(f.runs.snapshotStubs()?.[0].unshippedFailed).toBe(true);
  } finally {
    f.runs.clear();
    vi.useRealTimers();
  }
});

it('discards a member response when the window was cut while it was loading', async () => {
  const { f, runId } = heldRun();
  let answer!: (value: unknown) => void;
  setBindingMock('ListActivityRunMembers', () => new Promise(resolve => { answer = resolve; }));
  const request = f.runs.fetchMembers(runId, { direction: 'around', limit: 3, aroundItemId: 'e' });
  const before = f.items;
  const after = before.filter(item => item.id !== 'b');
  f.runs.applyWindowCut(before, after);
  f.setItems(after);
  f.runs.syncRunSpans(after);
  answer({ items: [activityRunRow('c', 2), activityRunRow('d', 3), activityRunRow('e', 4)],
    stub: activityRunStub({ loadedFirstItemId: 'c', loadedLastItemId: 'e', unshippedBefore: 2, unshippedAfter: 0 }) });
  await request;
  expect(f.mounted).toEqual([]);
  expect(f.runs.snapshotStubs()?.[0]).toMatchObject({ loadedFirstItemId: 'c', loadedLastItemId: 'd', unshippedBefore: 2 });
  setBindingMock('ListActivityRunMembers', async () => ({ items: [activityRunRow('e', 4)],
    stub: activityRunStub({ loadedFirstItemId: 'c', loadedLastItemId: 'e', unshippedBefore: 2, unshippedAfter: 0 }) }));
  await f.runs.fetchMembers(runId, { direction: 'after', limit: 1 });
  expect(f.items.map(item => item.id)).toContain('e');
});

it('uses the selected transcript when excluding context from unloaded run intervals', () => {
  const items = ['a', 'b', 'c', 'd'].map((id, index) => activityRunRow(id, index + 1, { parentId: 'agent' }));
  const f = fixture(items, { oldest: null, newest: null }, { scopeRootId: 'agent' });
  f.runs.syncRunSpans(items, [activityRunStub()]);
  expect(f.runs.loadedItems(items).map(item => item.id)).toEqual(['b', 'c', 'd']);
  expect(f.runs.snapshotStubs()).toEqual([activityRunStub()]);
  expect(f.runs.isLoadedMember('a')).toBe(false);
  expect(f.runs.isLoadedMember('b')).toBe(true);
});
