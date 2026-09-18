// The registry's run-record half (docs/architecture/timeline-window-pages.md
// §6): folding a page's stubs against the span the pane holds, routing a
// pushed row that lands inside a run the pane holds only part of,
// accounting for a window cut, and fetching members on demand.
//
// Separate from `threadActivityRuns.svelte.test.ts`, which drives the
// registry through projection passes and owns run IDENTITY and collapse.
// This file drives it through the pane's window instead.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { windowDigest } from './threadWindowDigest';
import { formatFnv1a64 } from '../utils/fnv1a';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';

function row(id: string, index: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    threadId: 't',
    turnIndex: 0,
    itemIndex: index,
    kind: 'tool_call',
    toolName: 'Bash',
    rev: index + 1,
    ...overrides,
  });
}

function prose(id: string, index: number): Item {
  return row(id, index, { kind: 'assistant_text', toolName: '' });
}

function stub(overrides: Partial<ActivityRunStub> = {}): ActivityRunStub {
  return {
    firstItemId: 'a',
    lastItemId: 'e',
    firstTurnIndex: 0,
    firstItemIndex: 1,
    lastTurnIndex: 0,
    lastItemIndex: 5,
    memberCount: 5,
    loadedFirstItemId: 'b',
    loadedLastItemId: 'd',
    unshippedBefore: 1,
    unshippedAfter: 1,
    unshippedDigest: windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }]),
    unshippedGroups: [],
    unshippedPairedLaunchIds: [],
    shippedSupersededLaunchIds: [],
    unshippedFailed: false,
    runningBefore: null,
    runningAfter: null,
    ...overrides,
  } as ActivityRunStub;
}

/** The pane's window, mutable so the registry's getters see the change. */
function fixture(initial: Item[] = []) {
  let items = initial;
  const mounted: { rows: Item[]; dropIds: string[] }[] = [];
  const failures: { message: string; silent: boolean }[] = [];
  const reloads = vi.fn();
  const runs = createThreadActivityRuns({
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
    reloadWindow: reloads,
    reportFetchFailure: (message, _err, silent) => failures.push({ message, silent }),
  });
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
    const items = [prose('p0', 0), row('b', 1), row('c', 2), row('d', 3), prose('p1', 4)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    expect(f.runs.heldRunFold()).toEqual({
      count: 2,
      digest: expect.objectContaining({}),
    });
    expect(formatFnv1a64(f.runs.heldRunFold()!.digest)).toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: 'e', rev: 5 }]),
    );
  });

  it('states no held window when a stub describes a span the pane does not hold', () => {
    const items = [row('b', 1), row('c', 2)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    expect(f.runs.heldRunFold()).toBeNull();
  });

  it('drops a record whose run left the window', () => {
    const items = [prose('p0', 0), row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    expect(f.runs.snapshotStubs()).toHaveLength(1);

    const next = [prose('p0', 0)];
    f.setItems(next);
    f.runs.syncRunSpans(next);
    expect(f.runs.snapshotStubs()).toEqual([]);
  });

  it('re-points a record whose span grew, and goes dirty', () => {
    const items = [row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    const grown = [...items, row('e', 4)];
    f.setItems(grown);
    f.runs.syncRunSpans(grown);
    expect(f.runs.heldRunFold()).toBeNull();
    expect(f.runs.snapshotStubs()).toBeNull();
  });
});

describe('runCoveringUnshipped', () => {
  const items = [prose('p0', 0), row('b', 2), row('c', 3), row('d', 4), prose('p1', 8)];

  function held() {
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    return f;
  }

  // The stub places the run's edges at 1 (a) and 5 (e); the window holds
  // b..d at 2..4 with prose either side. Membership is "between the
  // edges": the side counts say a side has something to fetch, the edge
  // coordinates say whether a given row is on it.
  it('claims a row between the run\'s first edge and its loaded span', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(row('new', 1))).toBe('a');
  });

  it('claims a row between its loaded span and the run\'s last edge', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(row('new', 5))).toBe('a');
  });

  it('claims nothing inside the loaded span, or outside the run', () => {
    const f = held();
    // Between two loaded members there is nothing unshipped to belong to.
    expect(f.runs.runCoveringUnshipped(row('new', 3))).toBeNull();
    // Past an edge is outside the run, whether or not the window holds
    // anything there: 6 sits between e and the prose at 8, 9 past it.
    expect(f.runs.runCoveringUnshipped(row('new', 6))).toBeNull();
    expect(f.runs.runCoveringUnshipped(row('new', 9))).toBeNull();
    expect(f.runs.runCoveringUnshipped(prose('older', 0))).toBeNull();
  });

  it('claims nothing on a side the stub says is empty', () => {
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub({ unshippedAfter: 0, unshippedDigest: windowDigest([{ id: 'a', rev: 1 }]) })]);
    expect(f.runs.runCoveringUnshipped(row('new', 5))).toBeNull();
    expect(f.runs.runCoveringUnshipped(row('new', 1))).toBe('a');
  });

  it('claims nothing for a row a page would not return', () => {
    const f = held();
    expect(f.runs.runCoveringUnshipped(row('child', 6, { parentId: 'b' }))).toBeNull();
  });
});

describe('applyWindowCut', () => {
  it('sheds the members a cut dropped from a run\'s older side', () => {
    const items = [row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);

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
    const items = [row('b', 1), row('c', 2), prose('p1', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub({ loadedLastItemId: 'c', unshippedAfter: 2 })]);

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
    const items = [row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);

    const next = items.slice(0, 2);
    f.runs.applyWindowCut(items, next);
    f.setItems(next);
    expect(f.runs.heldRunFold()).toBeNull();
  });
});

describe('fetchMembers', () => {
  function heldRun() {
    const items = [prose('p0', 0), row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    // The registry links a run entry to its record through a projection
    // pass, which is how a caller holding a runId reaches the record.
    f.runs.beginPass();
    const resolved = f.runs.resolve([['b'], ['c'], ['d']], 't');
    f.runs.endPass();
    return { f, runId: resolved.runId, resolved };
  }

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
    const whole = fixture([row('b', 1)]);
    whole.runs.beginPass();
    const resolved = whole.runs.resolve([['b']], 't');
    whole.runs.endPass();
    expect(whole.runs.summaryFacts(resolved.runId)).toBeNull();
  });

  it('bumps the revision on every record change, so nodes and headers re-derive', async () => {
    const items = [prose('p0', 0), row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    const start = f.runs.revision;
    f.runs.syncRunSpans(items, [stub()]);
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
      stub: stub({ loadedFirstItemId: 'c', unshippedBefore: 2, unshippedAfter: 3, memberCount: 7 }),
    }));
    await f.runs.fetchMembers(resolved.runId, { direction: 'before', limit: 0 });
    expect(f.runs.revision).toBeGreaterThan(afterCut);
    expect(f.runs.summaryFacts(resolved.runId)).toMatchObject({ memberCount: 7, unshippedAfter: 3 });
  });

  it('reports a run the pane holds whole as complete', () => {
    const f = fixture([row('b', 1)]);
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
        items: [row('a', 1, { itemIndex: 0 })],
        stub: stub({ loadedFirstItemId: 'a', unshippedBefore: 0 }),
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
      items: [row('x', 6)],
      stub: stub({ loadedFirstItemId: 'x', loadedLastItemId: 'x', unshippedBefore: 4, unshippedAfter: 0 }),
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
        items: [row('e', 5)],
        stub: stub({ loadedFirstItemId: 'e', loadedLastItemId: 'e', unshippedBefore: 4, unshippedAfter: 0 }),
      };
    });
    // `e` sits after the loaded span, inside the run's counted region.
    expect(await f.runs.loadUnshippedMember(row('e', 5))).toBe(true);
    expect(seen[0]).toMatchObject({ direction: 'around', aroundItemId: 'e', limit: 30 });
    expect(f.mounted[0].dropIds.sort()).toEqual(['b', 'c', 'd']);
    // A row no held run counts is not this path's to fetch.
    expect(await f.runs.loadUnshippedMember(row('far', 50))).toBe(false);
    expect(seen).toHaveLength(1);
  });

  it('does not mistake a row beyond the run\'s edges for an unshipped member', async () => {
    // The window opens ON the run: nothing loaded above b, and the stub
    // counts one member (a) before it. A jump to the user message of that
    // turn, older than a, is a jump into history this run does not cover.
    // Asking the server "around" it would be refused as a stale run and
    // reload the window under the reader. The stub's edge coordinates say
    // where the run starts; the side count only says a is unshipped.
    const items = [row('b', 2), row('c', 3), row('d', 4)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    const seen: unknown[] = [];
    setBindingMock('ListActivityRunMembers', async (_threadId: unknown, req: unknown) => {
      seen.push(req);
      return { items: [], stub: stub() };
    });
    expect(await f.runs.loadUnshippedMember(prose('u', 0))).toBe(false);
    expect(await f.runs.loadUnshippedMember(prose('later', 9))).toBe(false);
    expect(seen).toHaveLength(0);
    // The edges themselves are members.
    expect(await f.runs.loadUnshippedMember(row('a', 1))).toBe(false);
    expect(await f.runs.loadUnshippedMember(row('e', 5))).toBe(false);
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
    expect(f.failures).toEqual([{ message: 'Activity moved while it was loading', silent: false }]);
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
    const f = fixture([row('b', 1)]);
    f.runs.beginPass();
    const resolved = f.runs.resolve([['b']], 't');
    f.runs.endPass();
    expect(await f.runs.fetchMembers(resolved.runId, { direction: 'before', limit: 5 })).toEqual([]);
  });
});

describe('clear', () => {
  it('drops every record with the thread', () => {
    const items = [row('b', 1), row('c', 2), row('d', 3)];
    const f = fixture(items);
    f.runs.syncRunSpans(items, [stub()]);
    f.runs.clear();
    expect(f.runs.snapshotStubs()).toEqual([]);
    expect(f.runs.runCoveringUnshipped(row('new', 0))).toBeNull();
  });
});
