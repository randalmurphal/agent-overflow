// A run keeps growing at its newer end while the pane holds only part of
// it (docs/architecture/timeline-window-pages.md §6, Upserts). This drives
// the registry through the pane's real admission path
// (`applyItemUpsertsToWindow`) against a server that answers
// `ListActivityRunMembers` from the thread's physical rows, so a record is
// checked against what the server would say about the same span.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { applyItemUpsertsToWindow } from './threadItemUpserts';
import { reconcileSnapshotPage } from './threadItems';
import { windowDigest } from './threadWindowDigest';
import { isActivityRailRow } from '../utils/activityRunSpans';
import { activityRunProse, activityRunRow } from '../../test/helpers/activityRuns';
import { formatFnv1a64 } from '../utils/fnv1a';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Item } from '../types/models';
import type {
  ActivityRunMembers,
  ActivityRunStub,
} from '../../../bindings/agent-overflow/internal/store/models';

interface MembersRequest {
  runFirstItemId: string;
  loadedFirstItemId: string;
  loadedLastItemId: string;
  direction: 'before' | 'after' | 'around';
  aroundItemId: string;
  limit: number;
}

/**
 * The thread's rows as the store holds them, and the members RPC over
 * them: the run is the maximal stretch of rail rows and bells from its
 * first member, and a request's loaded span is taken at its word.
 */
function server(initial: Item[]) {
  const rows = [...initial];
  const reads: MembersRequest[] = [];
  let hold: Promise<void> | null = null;

  function run(firstItemId: string): Item[] {
    const start = rows.findIndex(row => row.id === firstItemId);
    let end = start + 1;
    while (end < rows.length && (isActivityRailRow(rows[end]) || rows[end].kind === 'notification')) end += 1;
    return rows.slice(start, end);
  }

  function stub(members: Item[], from: number, to: number): ActivityRunStub {
    const first = members[0];
    const last = members[members.length - 1];
    const unshipped = [...members.slice(0, from), ...members.slice(to)];
    return {
      firstItemId: first.id,
      lastItemId: last.id,
      firstTurnIndex: first.turnIndex,
      firstItemIndex: first.itemIndex,
      lastTurnIndex: last.turnIndex,
      lastItemIndex: last.itemIndex,
      memberCount: members.length,
      loadedFirstItemId: from < to ? members[from].id : '',
      loadedLastItemId: from < to ? members[to - 1].id : '',
      unshippedBefore: from,
      unshippedAfter: members.length - to,
      unshippedDigest: windowDigest(unshipped.map(row => ({ id: row.id, rev: row.rev }))),
      unshippedGroups: [],
      unshippedPairedLaunchIds: [],
      shippedSupersededLaunchIds: [],
      unshippedFailed: false,
      runningBefore: null,
      runningAfter: null,
    } as ActivityRunStub;
  }

  function answer(request: MembersRequest): ActivityRunMembers {
    const members = run(request.runFirstItemId);
    const index = (id: string) => members.findIndex(row => row.id === id);
    const held = request.loadedFirstItemId === ''
      ? null
      : { from: index(request.loadedFirstItemId), to: index(request.loadedLastItemId) + 1 };
    if (held && (held.from < 0 || held.to <= held.from)) throw new Error('not a span of the run');
    let from = held?.from ?? 0;
    let to = held?.to ?? 0;
    if (request.direction === 'around' && request.limit > 0) {
      const width = Math.min(request.limit, members.length);
      from = Math.min(Math.max(0, index(request.aroundItemId) - Math.floor(width / 2)), members.length - width);
      to = from + width;
    } else if (request.direction === 'after' && request.limit > 0) {
      to = Math.min(members.length, to + request.limit);
    } else if (request.direction === 'before' && request.limit > 0) {
      from = Math.max(0, from - request.limit);
    }
    const shipped = request.direction === 'around'
      ? members.slice(from, to)
      : members.slice(from, to).filter(row => !held || index(row.id) < held.from || index(row.id) >= held.to);
    return { items: shipped, stub: stub(members, from, to) } as unknown as ActivityRunMembers;
  }

  return {
    rows,
    reads,
    /** The stub the server holds for a run and a span. */
    stubFor(runFirstItemId: string, loadedFirstItemId: string, loadedLastItemId: string): ActivityRunStub {
      const members = run(runFirstItemId);
      const from = members.findIndex(row => row.id === loadedFirstItemId);
      const to = members.findIndex(row => row.id === loadedLastItemId) + 1;
      return stub(members, from, to);
    },
    /** Answers wait for `release` until then. The read itself happens at request time. */
    holdAnswers(): () => void {
      let release!: () => void;
      hold = new Promise(resolve => { release = resolve; });
      return () => { hold = null; release(); };
    },
    install() {
      setBindingMock('ListActivityRunMembers', async (_threadId: string, request: MembersRequest) => {
        reads.push(request);
        const result = answer(request);
        if (hold) await hold;
        return result;
      });
    },
  };
}

const panes: { runs: ReturnType<typeof createThreadActivityRuns>; reports: string[] }[] = [];

/**
 * A pane at the live edge: no floor, no ceiling, every pushed row offered.
 * A window reload runs `onReload`; the silent report that announces it is
 * kept in `reports`, and any other failure throws.
 */
function pane(initial: Item[], windowRows = 1) {
  let items = [...initial];
  const reports: string[] = [];
  const runs = createThreadActivityRuns({
    defaultCollapsed: () => false,
    windowRows: () => windowRows,
    windowVerified: () => true,
    scrollController: () => null,
    items: () => items,
    threadId: () => 't',
    pageShape: () => ({ inlinePreviews: true, runWindowRows: windowRows, maxBytes: 1024 }),
    mountRunMembers: (rows, dropIds) => {
      const byId = new Map(items.filter(item => !dropIds.has(item.id)).map(item => [item.id, item]));
      for (const row of rows) byId.set(row.id, row);
      items = [...byId.values()].sort((a, b) => a.itemIndex - b.itemIndex);
    },
    windowBounds: () => ({ oldest: null, newest: null }),
    reloadWindow: async () => { result.reloads += 1; result.onReload(); },
    reportFetchFailure: (message, _err, silent) => {
      if (!silent) throw new Error(message);
      reports.push(message);
    },
  });
  const result = {
    runs,
    reports,
    reloads: 0,
    onReload: () => {},
    get items() { return items; },
    get ids() { return items.map(item => item.id); },
    /** One live batch through the pane's admission and post-commit sync. */
    push(...incoming: Item[]) {
      const next = applyItemUpsertsToWindow({
        current: items,
        incoming,
        itemIndexById: new Map(items.map((item, index) => [item.id, index])),
        currentThreadId: 't',
        hasMoreNewer: false,
        runCoveringUnshipped: (item, batch) => runs.runCoveringUnshipped(item, batch),
      });
      if (!next) return;
      runs.noteRefusals(next.refusals);
      items = next.items;
      if (next.appendedItems.length > 0) runs.syncRunSpans(items);
    },
    /**
     * A page installed over the window the way a sync or a gap refresh
     * installs it: rows touched during the read are carried, then the
     * page's stubs fold.
     */
    installPage(page: Item[], stubs: ActivityRunStub[], touched: ReadonlySet<string> = new Set()) {
      const { items: next, refusals } = runs.admitCarriedRows(reconcileSnapshotPage(page, items, touched), page, stubs);
      items = next;
      runs.syncRunSpans(items, stubs);
      runs.noteRefusals(refusals);
    },
    runId(memberId: string): string {
      runs.beginPass();
      const { runId } = runs.resolve([[memberId]], 't');
      runs.endPass();
      return runId;
    },
  };
  panes.push(result);
  return result;
}

beforeEach(() => {
  resetBindingMocks();
  vi.useFakeTimers();
});

afterEach(() => {
  for (const p of panes.splice(0)) {
    p.runs.clear();
    expect(p.reports).toEqual([]);
  }
  vi.useRealTimers();
});

const digestOf = (rows: Item[]) => windowDigest(rows.map(row => ({ id: row.id, rev: row.rev })));
const held = (p: ReturnType<typeof pane>) => {
  const fold = p.runs.heldRunFold();
  return fold && { count: fold.count, digest: formatFnv1a64(fold.digest) };
};

// The thread: prose, then the live tail run a..d.
const p0 = activityRunProse('p0', 0);
const [a, b, c, d] = ['a', 'b', 'c', 'd'].map((id, index) => activityRunRow(id, index + 1));

/**
 * The pane holds only `a` of the live tail run a..d, with b..d counted
 * after it, reached either way a window can get there.
 */
const intoTailRunHead = {
  // A jump into the run's older member re-centers the span on it.
  async 'a jump into the run'() {
    const s = server([p0, a, b, c, d]);
    s.install();
    const p = pane([p0, b, c, d]);
    p.runs.syncRunSpans([p0, b, c, d], [s.stubFor('a', 'b', 'd')]);
    expect(await p.runs.loadUnshippedMember(a)).toBe(true);
    return { s, p };
  },
  // A page anchored on that member ships the anchor's run centered on it.
  async 'a page anchored in the run'() {
    const s = server([p0, a, b, c, d]);
    s.install();
    const p = pane([p0, a]);
    p.runs.syncRunSpans([p0, a], [s.stubFor('a', 'a', 'a')]);
    return { s, p };
  },
};

describe('a live member past a run the pane holds only the head of', () => {
  it.each(Object.keys(intoTailRunHead))('is counted after the span, reached by %s', async (path) => {
    const { s, p } = await intoTailRunHead[path as keyof typeof intoTailRunHead]();
    expect(p.ids).toEqual(['p0', 'a']);
    expect(held(p)).toEqual({ count: 3, digest: digestOf([b, c, d]) });

    const e = activityRunRow('e', 5);
    s.rows.push(e);
    p.push(e);
    await vi.advanceTimersByTimeAsync(2000);

    // e is not placed next to a: the pane would then describe a..e as one
    // loaded span and every count would drop b..d.
    expect(p.ids).toEqual(['p0', 'a']);
    expect(s.reads.at(-1)).toMatchObject({ limit: 0, loadedFirstItemId: 'a', loadedLastItemId: 'a' });
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'a')]);
    expect(held(p)).toEqual({ count: 4, digest: digestOf([b, c, d, e]) });
    expect(p.runs.summaryFacts(p.runId('a'))).toMatchObject({ memberCount: 5, unshippedAfter: 4 });
  });

  it('appends prose, which ends the run, and the rows after it', async () => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const reads = s.reads.length;
    const reply = activityRunProse('reply', 5);
    const next = activityRunRow('next', 6);
    s.rows.push(reply, next);
    p.push(reply);
    p.push(next);
    await vi.advanceTimersByTimeAsync(2000);

    expect(p.ids).toEqual(['p0', 'a', 'reply', 'next']);
    expect(s.reads).toHaveLength(reads);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'a')]);
    expect(held(p)).toEqual({ count: 3, digest: digestOf([b, c, d]) });
  });

  it.each([
    ['in position order', ['reply', 'next']],
    ['out of position order', ['next', 'reply']],
  ])('appends a row that prose arriving in the same batch ends the run before, %s', async (_name, order) => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const rows = { reply: activityRunProse('reply', 5), next: activityRunRow('next', 6) };
    s.rows.push(rows.reply, rows.next);
    p.push(...order.map(id => rows[id as keyof typeof rows]));
    await vi.advanceTimersByTimeAsync(2000);

    expect(p.ids).toEqual(['p0', 'a', 'reply', 'next']);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'a')]);
  });

  it('counts a member and a bell ahead of prose in the same batch, and appends the prose', async () => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const e = activityRunRow('e', 5);
    const bell = activityRunRow('bell', 6, { kind: 'notification', toolName: '' });
    const reply = activityRunProse('reply', 7);
    s.rows.push(e, bell, reply);
    p.push(e, bell, reply);
    await vi.advanceTimersByTimeAsync(2000);

    expect(p.ids).toEqual(['p0', 'a', 'reply']);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'a')]);
    expect(held(p)).toEqual({ count: 5, digest: digestOf([b, c, d, e, bell]) });
  });

  it('converges with the server when the reader loads the rest of the run', async () => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const e = activityRunRow('e', 5);
    s.rows.push(e);
    p.push(e);
    await vi.advanceTimersByTimeAsync(2000);
    const runId = p.runId('a');

    // A member lands while the reader's "later" fetch is in flight: the
    // answer does not count it, so the run refreshes and still counts it
    // after the span.
    const release = s.holdAnswers();
    const later = p.runs.fetchMembers(runId, { direction: 'after', limit: 10 });
    const f = activityRunRow('f', 6);
    s.rows.push(f);
    p.push(f);
    release();
    await later;
    await vi.advanceTimersByTimeAsync(2000);
    expect(p.ids).toEqual(['p0', 'a', 'b', 'c', 'd', 'e']);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'e')]);
    expect(held(p)).toEqual({ count: 1, digest: digestOf([f]) });

    // The next fetch reaches the run's newest member, and from there the
    // live members append and extend the span in place.
    await p.runs.fetchMembers(runId, { direction: 'after', limit: 10 });
    const reads = s.reads.length;
    const g = activityRunRow('g', 7);
    s.rows.push(g);
    p.push(g);
    await vi.advanceTimersByTimeAsync(2000);
    expect(p.ids).toEqual(['p0', 'a', 'b', 'c', 'd', 'e', 'f', 'g']);
    expect(s.reads).toHaveLength(reads);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'g')]);
    expect(held(p)).toEqual({ count: 0, digest: '0000000000000000' });
  });

  it('leaves a jump past the run\'s newer edge to the window slice', async () => {
    // A jump target past the edge could be anywhere after the window, and
    // "around" a row that is not a member is a stale-run refusal. The
    // counted region between the edges is the jump's only claim.
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const e = activityRunRow('e', 5);
    s.rows.push(e);
    const reads = s.reads.length;
    expect(await p.runs.loadUnshippedMember(e)).toBe(false);
    expect(s.reads).toHaveLength(reads);
  });
});

// A refusal stands until a refresh read after it confirms the run reaches
// the refused row: the prose that ends the run before that row can arrive
// after it, and a page read before the refusal says nothing about the row.
describe('a refused row the run\'s refresh settles', () => {
  it('reloads the window when the row turns out to follow the run\'s end', async () => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const reply = activityRunProse('reply', 5);
    const next = activityRunRow('next', 6);
    s.rows.push(reply, next);
    p.onReload = () => p.installPage([p0, a, reply, next], [s.stubFor('a', 'a', 'a')]);
    // The prose that ends the run arrives one batch after the row past it.
    p.push(next);
    p.push(reply);
    expect(p.ids).toEqual(['p0', 'a', 'reply']);
    await vi.advanceTimersByTimeAsync(2000);

    expect(s.reads.at(-1)).toMatchObject({ limit: 0, loadedFirstItemId: 'a', loadedLastItemId: 'a' });
    expect(p.reloads).toBe(1);
    expect(p.reports.splice(0)).toEqual(['Activity changed; refreshing history']);
    expect(p.ids).toEqual(['p0', 'a', 'reply', 'next']);
    expect(held(p)).toEqual({ count: 3, digest: digestOf([b, c, d]) });

    // The refusal is settled: nothing reads or reloads again.
    const reads = s.reads.length;
    await vi.advanceTimersByTimeAsync(5000);
    expect(s.reads).toHaveLength(reads);
    expect(p.reloads).toBe(1);
  });

  it('counts a refused member when a page read before it lands first', async () => {
    const { s, p } = await intoTailRunHead['a jump into the run']();
    const page = s.stubFor('a', 'a', 'a');
    const e = activityRunRow('e', 5);
    s.rows.push(e);
    p.push(e);
    p.installPage([p0, a], [page]);
    await vi.advanceTimersByTimeAsync(2000);

    expect(s.reads.at(-1)).toMatchObject({ limit: 0, loadedFirstItemId: 'a', loadedLastItemId: 'a' });
    expect(p.reloads).toBe(0);
    expect(held(p)).toEqual({ count: 4, digest: digestOf([b, c, d, e]) });
    expect(p.runs.summaryFacts(p.runId('a'))).toMatchObject({ memberCount: 5, unshippedAfter: 4 });
  });
});

// A sync or a gap refresh keeps rows the wire touched during its read,
// which the page it installs may not hold. Such a row passes the same rule
// as a pushed one, against the page.
describe('a live member carried over a page install', () => {
  /**
   * The pane holds the live tail run a..d whole, and a page read before e
   * lands after e was admitted: `page` is its stub for the span it ships.
   */
  function heldWhole(pageFirst: string, pageLast: string) {
    const s = server([p0, a, b, c, d]);
    s.install();
    const p = pane([p0, a, b, c, d]);
    p.runs.syncRunSpans(p.items, [s.stubFor('a', 'a', 'd')]);
    const page = s.stubFor('a', pageFirst, pageLast);
    const e = activityRunRow('e', 5);
    s.rows.push(e);
    p.push(e);
    expect(p.ids).toEqual(['p0', 'a', 'b', 'c', 'd', 'e']);
    return { s, p, e, page };
  }

  it('is refused and counted when the page ships the run without the members before it', async () => {
    // Anchored in the run, the page ships b alone.
    const { s, p, e, page } = heldWhole('b', 'b');
    p.installPage([p0, b], [page], new Set(['e']));
    expect(p.ids).toEqual(['p0', 'b']);
    await vi.advanceTimersByTimeAsync(2000);

    expect(s.reads.at(-1)).toMatchObject({ limit: 0, loadedFirstItemId: 'b', loadedLastItemId: 'b' });
    expect(p.reloads).toBe(0);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'b', 'b')]);
    expect(held(p)).toEqual({ count: 4, digest: digestOf([a, c, d, e]) });
  });

  it('stays in place when the page ships the run through its newest member', async () => {
    const { s, p, e, page } = heldWhole('c', 'd');
    const reads = s.reads.length;
    p.installPage([p0, c, d], [page], new Set(['e']));
    expect(p.ids).toEqual(['p0', 'c', 'd', 'e']);
    expect(p.items.at(-1)).toBe(e);
    await vi.advanceTimersByTimeAsync(2000);

    expect(s.reads).toHaveLength(reads);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'c', 'e')]);
  });

  it('stays in place past a row that ends the run', async () => {
    const { s, p, e, page } = heldWhole('b', 'b');
    const reply = activityRunProse('reply', 6);
    const next = activityRunRow('next', 7);
    s.rows.push(reply, next);
    p.push(reply, next);
    p.installPage([p0, b], [page], new Set(['e', 'reply', 'next']));
    expect(p.ids).toEqual(['p0', 'b', 'reply', 'next']);
    await vi.advanceTimersByTimeAsync(2000);
    expect(held(p)).toEqual({ count: 4, digest: digestOf([a, c, d, e]) });
  });
});

describe('a jump into a run the window holds more rows after', () => {
  it('appends live rows at the tail as before', async () => {
    const reply = activityRunProse('reply', 5);
    const x = activityRunRow('x', 6);
    const s = server([p0, a, b, c, d, reply, x]);
    s.install();
    const p = pane([p0, b, c, d, reply, x]);
    p.runs.syncRunSpans([p0, b, c, d, reply, x], [s.stubFor('a', 'b', 'd'), s.stubFor('x', 'x', 'x')]);
    expect(await p.runs.loadUnshippedMember(a)).toBe(true);
    expect(p.ids).toEqual(['p0', 'a', 'reply', 'x']);
    const reads = s.reads.length;

    const y = activityRunRow('y', 7);
    s.rows.push(y);
    p.push(y);
    await vi.advanceTimersByTimeAsync(2000);

    expect(p.ids).toEqual(['p0', 'a', 'reply', 'x', 'y']);
    expect(s.reads).toHaveLength(reads);
    expect(p.runs.snapshotStubs()).toEqual([s.stubFor('a', 'a', 'a'), s.stubFor('x', 'x', 'y')]);
  });
});
