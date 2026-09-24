// Pane-level coverage for the cold-open window sync
// (docs/architecture/thread-replica-sync.md §5, §6.1): what paints, what the
// attested page replaces, which stamp goes out on the next request, and
// what may be persisted.
//
// fake-indexeddb is imported HERE only. The rest of the suite runs with
// no `indexedDB` global, which is the "replica unavailable" posture the
// app must degrade to.
import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  getReplicaWindow,
  initReplica,
  putReplicaWindow,
  removeReplicaWindow,
  __resetReplicaForTest,
} from '../replica';

import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import {
  __resetBackendIdentityForTest,
  setBackendIdentityFromBootstrap,
} from '../transport/backendIdentity';
import { threadItemCache } from './threadItemCache';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { setupEventListeners } from './events';
import { registerPaneForTest } from './panes.svelte';
import type { Item } from '../types/models';
import type { SyncThreadWindowResult } from './bindings';
import {
  REPLICA_WRITE_BACK_DELAY_MS,
  type PaneScrollController,
} from './threadPaneShared';
import type { HeldWindow, PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import { windowDigest } from './threadWindowDigest';
import { TransportError } from '../transport/wsClient';

type SyncRequest = {
  anchorItemId: string;
  itemBudget: number;
  haveEpoch: number;
  haveRev: number;
  haveWindow?: HeldWindow;
};

const THREAD_ID = 'thread-sync';
/** `UNKNOWN_STAMP_VALUE` as it appears on a request the pane sent stampless. */
const UNKNOWN_REV = -1;

function row(id: string, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id,
    threadId: THREAD_ID,
    turnIndex: 0,
    itemIndex: Number(id.replace(/\D/g, '')) || 0,
    summary: id,
    ...overrides,
  });
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/**
 * A scroll controller that only counts `armWarmup`. Every other member
 * answers with a no-op so the pane can drive it without the test having
 * to model the whole controller surface.
 */
function countingWarmupController(onArm: () => void): PaneScrollController {
  return new Proxy({} as PaneScrollController, {
    get(_target, prop) {
      if (prop === 'armWarmup') return onArm;
      return () => undefined;
    },
  });
}

function page(items: Item[]): PagedItems {
  return {
    items,
    oldestCursor: items[0]
      ? { turnIndex: items[0].turnIndex, itemIndex: items[0].itemIndex, itemId: items[0].id }
      : { turnIndex: -1, itemIndex: -1, itemId: '' },
    newestCursor: items.at(-1)
      ? {
          turnIndex: items.at(-1)!.turnIndex,
          itemIndex: items.at(-1)!.itemIndex,
          itemId: items.at(-1)!.id,
        }
      : { turnIndex: -1, itemIndex: -1, itemId: '' },
    oldestTurnIndex: items[0]?.turnIndex ?? -1,
    newestTurnIndex: items.at(-1)?.turnIndex ?? -1,
    hasMore: false,
    hasMoreOlder: false,
    hasMoreNewer: false,
    runs: [],
  };
}

/** Install a SyncThreadWindow mock and capture every request it saw. */
function installSync(
  answer: (req: SyncRequest, threadId: string) => Partial<SyncThreadWindowResult>,
): SyncRequest[] {
  const requests: SyncRequest[] = [];
  setBindingMock('SyncThreadWindow', async (threadId: unknown, req: unknown) => {
    const request = req as SyncRequest;
    requests.push(request);
    return {
      status: 'stale',
      epoch: 1,
      rev: 1,
      generation: 'gen-1',
      ...answer(request, String(threadId)),
    };
  });
  return requests;
}

function replicaBody(items: Item[], epoch: number, rev: number) {
  return {
    epoch,
    rev,
    savedAt: 1_000,
    items,
    oldestCursor: { turnIndex: 0, itemIndex: 0, itemId: items[0]?.id ?? '' },
    newestCursor: {
      turnIndex: 0,
      itemIndex: items.at(-1)?.itemIndex ?? 0,
      itemId: items.at(-1)?.id ?? '',
    },
    hasMoreOlder: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    runs: [],
  };
}

describe('cold-open window sync', () => {
  beforeEach(async () => {
    __resetReplicaForTest();
    installPaneMocks();
    await initReplica({
      backendId: `backend-${Math.random().toString(36).slice(2)}`,
      generation: 'gen-1',
      name: '',
    });
  });

  it('keeps cached rows visible until a byte-limited replacement covers their window, without attesting mixed reads', async () => {
    const thread = makeThread({ id: THREAD_ID });
    const away = makeThread({ id: 'away' });
    const original = Array.from({ length: 60 }, (_, i) => row(`i${i}`, { rev: 1 }));
    const fresh = original.map(item => ({ ...item, summary: `${item.summary} refreshed`, rev: 2 }));
    const pane = createThreadPane();
    installSync((_request, id) => ({ page: page(id === THREAD_ID ? original : []) }));
    await pane.switchThread(thread);
    await pane.switchThread(away);

    const older = deferred<PagedItems>();
    const read = setBindingMock('ListItemsBeforeCursor', () => older.promise);
    installSync(() => ({ rev: 2, page: { ...page(fresh.slice(-5)), hasMore: true, hasMoreOlder: true } }));
    const switching = pane.switchThread(thread);
    await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
    expect(pane.items.map(item => item.summary)).toEqual(original.map(item => item.summary));
    older.resolve(page(fresh.slice(0, -5)));
    await switching;
    expect(pane.items.map(item => item.summary)).toEqual(fresh.map(item => item.summary));

    installSync(() => ({ page: page([]) }));
    await pane.switchThread(away);
    const requests = installSync(() => ({ status: 'fresh', rev: 3 }));
    await pane.switchThread(thread);
    expect(requests[0]).toMatchObject({ haveEpoch: UNKNOWN_REV, haveRev: UNKNOWN_REV,
      haveWindow: { count: 60, oldestItemId: 'i0', newestItemId: 'i59' } });
    expect(pane.items.map(item => item.summary)).toEqual(fresh.map(item => item.summary));
    pane.clear();
  });

  it('stages the replica window before the sync answers', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0'), row('i1')], 3, 11));
    const staged: string[][] = [];
    const pane = createThreadPane();
    installSync(() => {
      // The replica read precedes the ask, so the durable rows are
      // available as verification evidence before the response.
      staged.push(pane.items.map((it) => it.id));
      return { status: 'fresh', epoch: 3, rev: 11 };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(staged).toEqual([['i0', 'i1']]);
    expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
  });

  it('sends the replica stamp, so an unchanged window can answer fresh', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 3, 11));
    const pane = createThreadPane();
    const requests = installSync(() => ({ status: 'fresh', epoch: 3, rev: 11 }));

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(requests).toHaveLength(1);
    expect(requests[0].haveEpoch).toBe(3);
    expect(requests[0].haveRev).toBe(11);
    // A fresh answer applies nothing; the painted rows ARE the window.
    expect(pane.items.map((it) => it.id)).toEqual(['i0']);
  });

  it('asks with unknown stamps when nothing is cached', async () => {
    const pane = createThreadPane();
    const requests = installSync(() => ({ status: 'stale', epoch: 1, rev: 2, page: page([row('i0')]) }));

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(requests[0].haveEpoch).toBe(-1);
    expect(requests[0].haveRev).toBe(-1);
    expect(pane.items.map((it) => it.id)).toEqual(['i0']);
  });

  it('lets the page replace painted replica rows, keeping === refs for unchanged ones', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0'), row('i1'), row('i2')], 1, 5));
    const pane = createThreadPane();
    let paintedRow: Item | undefined;
    installSync(() => {
      paintedRow = pane.items.find((it) => it.id === 'i0');
      return {
        // `rewritten`: i2 no longer exists, i1 changed, i0 is untouched.
        status: 'rewritten',
        epoch: 2,
        rev: 9,
        page: page([row('i0'), row('i1', { summary: 'changed' })]),
      };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
    expect(pane.items.find((it) => it.id === 'i1')?.summary).toBe('changed');
    // Unchanged rows keep their reference so the reconcile re-renders nothing.
    expect(pane.items.find((it) => it.id === 'i0')).toBe(paintedRow);
  });

  it('keeps rows the wire delivered while the page was in flight', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 5));
    const pane = createThreadPane();
    installSync(() => {
      // The page's read snapshot predates this row, so the page cannot
      // contain it — and dropping it would lose the row a thread opened
      // mid-stream is streaming into.
      pane.applyProviderItemUpserts([row('i9', { itemIndex: 9, status: 'streaming' })]);
      return { status: 'stale', epoch: 1, rev: 6, page: page([row('i0')]) };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i9']);
  });

  it('an L1 hit still fires the sync and reconciles a stale answer', async () => {
    const pane = createThreadPane();
    const requests = installSync(() => ({
      status: 'stale',
      epoch: 1,
      rev: 2,
      page: page([row('i0')]),
    }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    // Switch away to snapshot the window into the in-memory LRU, then
    // drop the durable copy so only the L1 snapshot can supply a stamp.
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    await removeReplicaWindow(THREAD_ID);

    setBindingMock('SyncThreadWindow', async (_threadId: unknown, req: unknown) => {
      requests.push(req as SyncRequest);
      return {
        status: 'stale',
        epoch: 1,
        rev: 8,
        generation: 'gen-1',
        page: page([row('i0'), row('i1')]),
      };
    });
    await pane.switchThread(makeThread({ id: THREAD_ID }));

    // The cache hit no longer skips the fetch — that skip was a real
    // staleness hole — and the page converges the window.
    expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
    const last = requests.at(-1)!;
    expect(last.haveRev).toBe(2);
    expect(last.haveEpoch).toBe(1);
  });

  it('drops the replica entry and empties the pane on gone', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 5));
    const pane = createThreadPane();
    installSync(() => ({ status: 'gone', epoch: 0, rev: 0 }));

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(pane.items).toEqual([]);
    expect(pane.generalError).toContain('no longer exists');
    expect(await getReplicaWindow(THREAD_ID)).toBeNull();
  });

  it('classifies a transient empty-window failure and retries it in place', async () => {
    const pane = createThreadPane();
    let attempts = 0;
    setBindingMock('SyncThreadWindow', async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new TransportError('temporarily_unavailable', 'thread history read timed out');
      }
      return {
        status: 'stale',
        epoch: 1,
        rev: 2,
        generation: 'gen-1',
        page: page([row('i0')]),
      };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(pane.items).toEqual([]);
    expect(pane.generalErrorKind).toBe('history-load');
    expect(pane.generalError).toBe('Thread history took too long to load.');

    expect(pane.loading).toBe(false);
    const retries = Promise.all([pane.retryHistoryLoad(), pane.retryHistoryLoad()]);
    // The retry is a load: the pane shows its loading state until it lands.
    expect(pane.loading).toBe(true);
    await retries;
    expect(pane.loading).toBe(false);
    expect(attempts).toBe(2);
    expect(pane.items.map((item) => item.id)).toEqual(['i0']);
    expect(pane.generalError).toBeNull();
    expect(pane.generalErrorKind).toBeNull();
  });

  it('does not clear a newer unrelated error when a history retry succeeds', async () => {
    const pane = createThreadPane();
    let attempts = 0;
    setBindingMock('SyncThreadWindow', async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('database busy');
      return {
        status: 'stale',
        epoch: 1,
        rev: 2,
        generation: 'gen-1',
        page: page([row('i0')]),
      };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(pane.generalErrorKind).toBe('history-load');
    pane.setGeneralError('Rename failed');

    await pane.retryHistoryLoad();
    expect(pane.items.map((item) => item.id)).toEqual(['i0']);
    expect(pane.generalError).toBe('Rename failed');
    expect(pane.generalErrorKind).toBeNull();
  });

  it('discards a replica read superseded by a newer switch', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 5));
    const pane = createThreadPane();
    installSync((_req, threadId) => ({
      status: 'stale',
      epoch: 1,
      rev: 2,
      page: page(threadId === THREAD_ID ? [row('i0')] : []),
    }));

    const first = pane.switchThread(makeThread({ id: THREAD_ID }));
    const second = pane.switchThread(makeThread({ id: 'other-thread' }));
    await Promise.all([first, second]);

    // The superseded read resolves against a pane that has moved on; its
    // rows must not leak into the thread now mounted.
    expect(pane.thread?.id).toBe('other-thread');
    expect(pane.items).toEqual([]);
  });

  it('persists the window on switch-away once a sync has attested it', async () => {
    const pane = createThreadPane();
    installSync(() => ({ status: 'stale', epoch: 4, rev: 12, page: page([row('i0'), row('i1')]) }));

    await pane.switchThread(makeThread({ id: THREAD_ID }));
    await pane.switchThread(makeThread({ id: 'other-thread' }));

    const stored = await getReplicaWindow(THREAD_ID);
    expect(stored?.epoch).toBe(4);
    expect(stored?.rev).toBe(12);
    expect(stored?.items.map((it) => it.id)).toEqual(['i0', 'i1']);
  });

  it('writes no envelope when no sync attested the window', async () => {
    const pane = createThreadPane();
    // The sync fails outright, so nothing about this window is attested;
    // an event-carried stamp must not stand in for the attestation.
    setBindingMock('SyncThreadWindow', async () => {
      throw new Error('backend down');
    });
    vi.spyOn(console, 'error').mockImplementation(() => {});
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    pane.applyProviderItemUpserts([row('i0')]);
    await pane.switchThread(makeThread({ id: 'other-thread' }));

    expect(await getReplicaWindow(THREAD_ID)).toBeNull();
    vi.restoreAllMocks();
  });

  it('uses the painted window stamp for validation even after a newer turn_completed stamp', async () => {
    const dispose = setupEventListeners();
    const pane = createThreadPane();
    registerPaneForTest('sync-pane', pane);
    installSync(() => ({ status: 'stale', epoch: 1, rev: 2, page: page([row('i0')]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));

    emitWailsEvent('provider:turn_completed', {
      threadId: THREAD_ID,
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      historyEpoch: 1,
      historyRev: 30,
    });

    // Switch away: the window persists, but under the sync-attested
    // stamp (rev 2), never the event-carried one (rev 30). An event
    // stamp can cover a frame this client never received; the attested
    // stamp merely understates, which costs one fetch, not correctness.
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    const envelope = await getReplicaWindow(THREAD_ID);
    expect(envelope).not.toBeNull();
    expect(envelope).toMatchObject({ epoch: 1, rev: 2 });

    const requests = installSync(() => ({ status: 'stale', epoch: 1, rev: 30, page: page([row('i0'), row('i1')]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(requests.at(-1)).toMatchObject({ haveEpoch: 1, haveRev: 2 });
    expect(pane.items.map(item => item.id)).toEqual(['i0', 'i1']);

    dispose();
  });

  it('wipes every cache tier and re-asks when a sync response reveals a new generation', async () => {
    const dispose = setupEventListeners();
    const backendId = `backend-${Math.random().toString(36).slice(2)}`;
    setBackendIdentityFromBootstrap(backendId, 'gen-1');
    await initReplica({ backendId, generation: 'gen-1', name: '' });

    // Session one: attested window, cached in L1 and the replica.
    const pane = createThreadPane();
    registerPaneForTest('sync-pane', pane);
    installSync(() => ({ status: 'stale', epoch: 0, rev: 50, page: page([row('i0'), row('i1')]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    expect(await getReplicaWindow(THREAD_ID)).not.toBeNull();

    // The backend's database is restored to a divergent lineage whose
    // counters coincidentally match the client's stamp — the one shape
    // a counter comparison can never catch. The sync response's live
    // generation is the only signal a connected client gets.
    const requests = installSync((req) =>
      req.haveRev === 50
        ? { status: 'fresh', epoch: 0, rev: 50, generation: 'gen-2' }
        : { status: 'rewritten', epoch: 4, rev: 50, generation: 'gen-2', page: page([row('j0')]) },
    );
    await pane.switchThread(makeThread({ id: THREAD_ID }));

    // The coincidental fresh was refused: a second, stampless ask
    // fetched the divergent lineage's real window.
    expect(requests).toHaveLength(2);
    expect(requests[1]).toMatchObject({ haveEpoch: -1, haveRev: -1 });
    expect(pane.items.map((item) => item.id)).toEqual(['j0']);
    // Old-lineage caches are gone everywhere: the durable replica…
    expect(await getReplicaWindow('other-thread')).toBeNull();
    // …and the L1 snapshot (its paired stamp claimed the dead lineage).
    expect(threadItemCache.get('other-thread')).toBeNull();

    __resetBackendIdentityForTest();
    dispose();
  });

  it('preserves the envelope stamp when validating its painted rows fails', async () => {
    // The durable copy is at rev 100…
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 100));
    const pane = createThreadPane();
    setBindingMock('SyncThreadWindow', async () => {
      throw new Error('transport hiccup');
    });
    vi.spyOn(console, 'error').mockImplementation(() => {});

    await pane.switchThread(makeThread({ id: THREAD_ID }));
    // The paint survives the failure — that is deliberate; blanking a
    // painted window would be strictly worse.
    expect(pane.items.map((it) => it.id)).toEqual(['i0']);

    await pane.switchThread(makeThread({ id: 'other-thread' }));

    // …but the rows on screen descend from the ENVELOPE, so that is the
    // only stamp they may be written back under. Pairing them with rev
    // 150 would answer `fresh` forever over a window missing everything
    // rev 100→150 changed.
    const stored = await getReplicaWindow(THREAD_ID);
    expect(stored).toMatchObject({ epoch: 1, rev: 100 });
    expect(stored?.items.map((it) => it.id)).toEqual(['i0']);
    vi.restoreAllMocks();
  });

  it('keeps an optimistic marker until the WIRE echoes the row', async () => {
    const pane = createThreadPane();
    installSync(() => ({ status: 'stale', epoch: 1, rev: 2, page: page([row('i0')]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));

    pane.trackOptimisticItem('user:1');
    pane.upsertItems([row('user:1', { turnIndex: 1, itemIndex: 0 })]);
    // The pane's OWN optimistic insert must not discharge the marker —
    // only the backend can say the row exists. Discharging it here left
    // `isOptimisticItem` permanently false, so the failed-send rollback
    // never ran and every phantom filter downstream was dead code.
    expect(pane.isOptimisticItem('user:1')).toBe(true);

    pane.applyProviderItemUpserts([
      row('user:1', { turnIndex: 1, itemIndex: 0, summary: 'persisted' }),
    ]);
    expect(pane.isOptimisticItem('user:1')).toBe(false);
  });

  it('keeps an optimistic row, and its stamp, out of the cached tiers', async () => {
    const pane = createThreadPane();
    installSync(() => ({ status: 'stale', epoch: 1, rev: 2, page: page([row('i0')]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));

    // The composer's optimistic user row: it exists only in this pane's
    // hope until the wire echoes the persisted one.
    pane.trackOptimisticItem('user:1');
    pane.upsertItems([row('user:1', { turnIndex: 1, itemIndex: 0 })]);
    expect(pane.items.map((it) => it.id)).toEqual(['i0', 'user:1']);

    await pane.switchThread(makeThread({ id: 'other-thread' }));

    // `resetIncomingPaneState` clears the optimistic-id ledger, so a row
    // cached here would come back on a warm re-entry as an untracked
    // phantom — and the snapshot's stamp would let the next answer
    // attest it straight into the durable replica.
    const cached = threadItemCache.get(THREAD_ID);
    expect(cached?.items.map((it) => it.id)).toEqual(['i0']);
    expect(cached?.newestLoadedCursor?.itemId ?? '').not.toBe('user:1');
    // …and the pairing goes with the row: this is no longer the window
    // any rev had, so the next open re-fetches instead of asking `fresh`
    // about rows it edited on the way out.
    expect(cached?.historyStamp ?? null).toBeNull();
    // The replica has no way to hold rows without a stamp, so it simply
    // is not written while the send is unresolved.
    expect(await getReplicaWindow(THREAD_ID)).toBeNull();
  });

  it('refuses the coincidental fresh in EVERY pane in flight across one generation flip', async () => {
    const backendId = `backend-${Math.random().toString(36).slice(2)}`;
    setBackendIdentityFromBootstrap(backendId, 'gen-1');
    await initReplica({ backendId, generation: 'gen-1', name: '' });

    const A = 'thread-a';
    const B = 'thread-b';
    await putReplicaWindow(A, replicaBody([row('a0', { threadId: A })], 1, 10));
    await putReplicaWindow(B, replicaBody([row('b0', { threadId: B })], 1, 20));

    const held = deferred<void>();
    const bothAsked = deferred<void>();
    const stamped: string[] = [];
    const stampless: string[] = [];
    setBindingMock('SyncThreadWindow', async (threadId: unknown, req: unknown) => {
      const request = req as SyncRequest;
      const id = String(threadId);
      if (request.haveRev === UNKNOWN_REV) {
        stampless.push(id);
        return {
          status: 'rewritten',
          epoch: 9,
          rev: 99,
          generation: 'gen-2',
          page: page([row(`${id}-new`, { threadId: id })]),
        };
      }
      stamped.push(id);
      if (stamped.length === 2) bothAsked.resolve();
      // Both panes are now in flight against the lineage they believe
      // in. Only the FIRST response to land moves the global identity;
      // the second observes "no change" about the very flip that killed
      // its painted rows.
      await held.promise;
      return {
        status: 'fresh',
        epoch: request.haveEpoch,
        rev: request.haveRev,
        generation: 'gen-2',
      };
    });

    const paneA = createThreadPane();
    const paneB = createThreadPane();
    const openA = paneA.switchThread(makeThread({ id: A }));
    const openB = paneB.switchThread(makeThread({ id: B }));
    await bothAsked.promise;
    held.resolve();
    await Promise.all([openA, openB]);

    expect(stampless.sort()).toEqual([A, B]);
    expect(paneA.items.map((it) => it.id)).toEqual([`${A}-new`]);
    expect(paneB.items.map((it) => it.id)).toEqual([`${B}-new`]);

    __resetBackendIdentityForTest();
  });

  it('re-arms the warm gate when a lineage change replaces the painted window', async () => {
    const backendId = `backend-${Math.random().toString(36).slice(2)}`;
    setBackendIdentityFromBootstrap(backendId, 'gen-1');
    await initReplica({ backendId, generation: 'gen-1', name: '' });
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 5));

    const pane = createThreadPane();
    let arms = 0;
    pane.attachScrollController(
      countingWarmupController(() => {
        arms += 1;
      }),
    );
    installSync((req) =>
      req.haveRev === UNKNOWN_REV
        ? {
            status: 'rewritten',
            epoch: 9,
            rev: 9,
            generation: 'gen-2',
            page: page([row('j0')]),
          }
        : { status: 'fresh', epoch: 1, rev: 5, generation: 'gen-2' },
    );

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(pane.items.map((it) => it.id)).toEqual(['j0']);
    // The staged replica is never mounted. Only the final window arms.
    expect(arms).toBe(1);

    __resetBackendIdentityForTest();
  });

  it('arms the warm gate at verification even when a cached window answers fresh', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 3, 11));
    const pane = createThreadPane();
    let arms = 0;
    pane.attachScrollController(countingWarmupController(() => { arms += 1; }));
    const answer = deferred<void>();
    const asked = deferred<void>();
    setBindingMock('SyncThreadWindow', async () => {
      asked.resolve();
      await answer.promise;
      return { status: 'fresh', epoch: 3, rev: 11, generation: 'gen-1' };
    });

    const open = pane.switchThread(makeThread({ id: THREAD_ID }));
    await asked.promise;
    expect(pane.items.map(item => item.id)).toEqual(['i0']);
    expect(pane.historyWindowPending).toBe(true);
    expect(arms).toBe(0);
    answer.resolve();
    await open;
    expect(pane.historyWindowPending).toBe(false);
    expect(arms).toBe(1);
  });

  it('disarms the in-flight-sync ledger when the pane is cleared mid-load', async () => {
    const pane = createThreadPane();
    const held = deferred<void>();
    setBindingMock('SyncThreadWindow', async () => {
      await held.promise;
      return { status: 'stale', epoch: 1, rev: 2, generation: 'gen-1', page: page([row('i0')]) };
    });

    const open = pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(pane.__syncLedgerArmedForTest()).toBe(true);
    expect(pane.historyWindowPending).toBe(true);

    pane.clear();
    // Only `runItemWindowSync`'s finally used to clear this, and clear()
    // invalidates that leg — so the ledger stayed armed for the pane's
    // lifetime and every later upsert accumulated into a set the next
    // page application reads as "arrived during my sync, do not drop".
    expect(pane.__syncLedgerArmedForTest()).toBe(false);
    expect(pane.historyWindowPending).toBe(false);

    held.resolve();
    await open;
    expect(pane.__syncLedgerArmedForTest()).toBe(false);
    expect(pane.historyWindowPending).toBe(false);
  });

  // The second route to a page-less `fresh`: a turn on the open thread
  // moves the thread's rev, so the pane's stamp is worthless on the next
  // open even though every row it holds is still current. It describes
  // the ROWS instead (docs/architecture/thread-replica-sync.md §3.4).
  describe('held-window description', () => {
    it('describes the rows it holds when a turn left it without a stamp', async () => {
      const pane = createThreadPane();
      const requests = installSync(() => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      }));
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      // A turn on the open thread: the upserts carry the revs their
      // writes produced and clear the pane's attestation.
      pane.applyProviderItemUpserts([
        row('i0', { rev: 7, summary: 'settled' }),
        row('i1', { itemIndex: 1, rev: 8 }),
      ]);
      await pane.switchThread(makeThread({ id: 'other-thread' }));
      // Nothing attested that window, so neither stamped tier holds a stamp…
      expect(threadItemCache.get(THREAD_ID)?.historyStamp ?? null).toBeNull();
      expect(await getReplicaWindow(THREAD_ID)).toBeNull();

      await pane.switchThread(makeThread({ id: THREAD_ID }));

      // …and the reopen still has evidence to send: the mutated revs.
      const reopen = requests.at(-1)!;
      expect(reopen.haveEpoch).toBe(UNKNOWN_REV);
      expect(reopen.haveRev).toBe(UNKNOWN_REV);
      expect(reopen.haveWindow).toEqual({
        oldestItemId: 'i0',
        newestItemId: 'i1',
        count: 2,
        hasMoreOlder: false,
        hasMoreNewer: false,
        digest: windowDigest([{ id: 'i0', rev: 7 }, { id: 'i1', rev: 8 }]),
      });
    });

    it('describes a row re-persisted without visible change at its latest rev', async () => {
      const pane = createThreadPane();
      const requests = installSync(() => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      }));
      await pane.switchThread(makeThread({ id: THREAD_ID }));
      const painted = pane.items;

      // The backend re-persisted the row unchanged: the upsert differs
      // only in `rev`. The dedupe keeps the array (no render cascade) and
      // the row, so the page's attestation survives too…
      pane.applyProviderItemUpserts([row('i0', { rev: 7 })]);
      expect(pane.items).toBe(painted);
      await pane.switchThread(makeThread({ id: 'other-thread' }));
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      // …and the reopen sends both: the stamp the page attested, and the
      // window at the revision the backend now reads the row at. Only the
      // latter can verify (the thread's rev moved past the stamp).
      const reopen = requests.at(-1)!;
      expect(reopen).toMatchObject({ haveEpoch: 1, haveRev: 2 });
      expect(reopen.haveWindow?.digest).toBe(windowDigest([{ id: 'i0', rev: 7 }]));
    });

    it('sends the window alongside an attested stamp', async () => {
      await putReplicaWindow(THREAD_ID, replicaBody([row('i0', { rev: 11 })], 3, 11));
      const pane = createThreadPane();
      const requests = installSync(() => ({ status: 'fresh', epoch: 3, rev: 11 }));

      await pane.switchThread(makeThread({ id: THREAD_ID }));

      // The two are independent evidence and both ride the one ask: the
      // stamp answers "did the thread change", the window answers "are
      // these rows still the read".
      expect(requests[0]).toMatchObject({ haveEpoch: 3, haveRev: 11 });
      expect(requests[0].haveWindow).toMatchObject({ count: 1, oldestItemId: 'i0' });
    });

    it('attests a fresh answer over the sent window and writes it back', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        const pane = createThreadPane();
        let answer: () => Partial<SyncThreadWindowResult> = () => ({
          status: 'stale',
          epoch: 1,
          rev: 2,
          page: page([row('i0', { rev: 2 })]),
        });
        const requests = installSync(() => answer());
        await pane.switchThread(makeThread({ id: THREAD_ID }));
        pane.applyProviderItemUpserts([row('i0', { rev: 9, summary: 'after the turn' })]);
        await pane.switchThread(makeThread({ id: 'other-thread' }));

        // The server verified the rows in the same read transaction as
        // the stamp it returns, so a page-less `fresh` over a described
        // window attests exactly as a stamp-validated one does.
        answer = () => ({ status: 'fresh', epoch: 1, rev: 9 });
        await pane.switchThread(makeThread({ id: THREAD_ID }));

        expect(requests.at(-1)?.haveWindow).toMatchObject({ count: 1 });
        expect(pane.items.map((it) => it.id)).toEqual(['i0']);
        expect(pane.items[0].summary).toBe('after the turn');

        await vi.advanceTimersByTimeAsync(REPLICA_WRITE_BACK_DELAY_MS + 50);
        expect(await getReplicaWindow(THREAD_ID)).toMatchObject({ epoch: 1, rev: 9 });

        await pane.switchThread(makeThread({ id: 'other-thread' }));
        expect(threadItemCache.get(THREAD_ID)?.historyStamp).toEqual({
          epoch: 1,
          rev: 9,
          attested: true,
        });
      } finally {
        vi.useRealTimers();
      }
    });

    it('lets a stale answer replace a window the server refused', async () => {
      const pane = createThreadPane();
      let answer: () => Partial<SyncThreadWindowResult> = () => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      });
      const requests = installSync(() => answer());
      await pane.switchThread(makeThread({ id: THREAD_ID }));
      pane.applyProviderItemUpserts([row('i0', { rev: 9 })]);
      await pane.switchThread(makeThread({ id: 'other-thread' }));

      // Another client rewrote the thread, so the described rows are not
      // what a read returns any more. One page, one ask: the refusal is
      // not an error path.
      answer = () => ({
        status: 'stale',
        epoch: 1,
        rev: 14,
        page: page([row('i0', { rev: 14 }), row('i1', { itemIndex: 1, rev: 14 })]),
      });
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      expect(requests.at(-1)?.haveWindow).toMatchObject({ count: 1 });
      expect(requests.filter((req) => req.haveWindow !== undefined)).toHaveLength(1);
      expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
      await pane.switchThread(makeThread({ id: 'other-thread' }));
      expect(threadItemCache.get(THREAD_ID)?.historyStamp).toEqual({
        epoch: 1,
        rev: 14,
        attested: true,
      });
    });

    it('leaves plan_update notifications out of the description', async () => {
      const pane = createThreadPane();
      let answer: () => Partial<SyncThreadWindowResult> = () => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      });
      const requests = installSync(() => answer());
      await pane.switchThread(makeThread({ id: THREAD_ID }));
      // A plan_update notification reaches the pane over the wire but is
      // never in a page (`windowedTimelineFilter`), so counting it would
      // refuse every window the pane holds one in.
      pane.applyProviderItemUpserts([
        row('plan', { itemIndex: 1, rev: 6, kind: 'notification', toolName: 'plan_update' }),
        row('i1', { itemIndex: 2, rev: 7 }),
      ]);
      await pane.switchThread(makeThread({ id: 'other-thread' }));
      answer = () => ({ status: 'fresh', epoch: 1, rev: 7 });
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      expect(pane.items.map((it) => it.id)).toEqual(['i0', 'plan', 'i1']);
      expect(requests.at(-1)?.haveWindow).toEqual({
        oldestItemId: 'i0',
        newestItemId: 'i1',
        count: 2,
        hasMoreOlder: false,
        hasMoreNewer: false,
        digest: windowDigest([{ id: 'i0', rev: 2 }, { id: 'i1', rev: 7 }]),
      });
    });

    it('describes no window when it holds imported history, and takes the page', async () => {
      const pane = createThreadPane();
      let answer: () => Partial<SyncThreadWindowResult> = () => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        // Imported rows carry no per-row stamp; the store reads them as -1.
        page: page([row('i0', { rev: -1 }), row('i1', { itemIndex: 1, rev: 2 })]),
      });
      const requests = installSync(() => answer());
      await pane.switchThread(makeThread({ id: THREAD_ID }));
      pane.applyProviderItemUpserts([row('i1', { itemIndex: 1, rev: 5, summary: 'settled' })]);
      await pane.switchThread(makeThread({ id: 'other-thread' }));

      // Restated: the digest now composes loaded rows with each held
      // activity run's UnshippedDigest (timeline-window-pages §5), and a
      // row with no revision makes that composition a claim the server
      // cannot check. The pane describes nothing and pays the page it
      // would have paid for the server's refusal anyway.
      answer = () => ({
        status: 'stale',
        epoch: 1,
        rev: 5,
        page: page([
          row('i0', { rev: -1 }),
          row('i1', { itemIndex: 1, rev: 5, summary: 'settled' }),
        ]),
      });
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      expect(requests.at(-1)?.haveWindow).toBeUndefined();
      expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
    });

    it('describes no window mid-stream while a row is unstamped', async () => {
      const pane = createThreadPane();
      let answer: () => Partial<SyncThreadWindowResult> = () => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      });
      const requests = installSync(() => answer());
      await pane.switchThread(makeThread({ id: THREAD_ID }));
      // A streaming row's upsert is altered on the wire (blank summary,
      // text arrives as deltas), so the store sends it unstamped at -1
      // until the settle patch supplies the real rev.
      pane.applyProviderItemUpserts([
        row('i1', { itemIndex: 1, rev: -1, status: 'streaming', summary: '' }),
      ]);
      await pane.switchThread(makeThread({ id: 'other-thread' }));

      answer = () => ({
        status: 'stale',
        epoch: 1,
        rev: 6,
        page: page([
          row('i0', { rev: 2 }),
          row('i1', { itemIndex: 1, rev: 6, summary: 'settled' }),
        ]),
      });
      await pane.switchThread(makeThread({ id: THREAD_ID }));

      // Same restatement as the imported case above: an unstamped row
      // costs the description, not a refused round trip. The replacing
      // page brings the stamped rows back either way.
      expect(requests.at(-1)?.haveWindow).toBeUndefined();
      expect(pane.items.map((it) => it.id)).toEqual(['i0', 'i1']);
      expect(pane.items[1].rev).toBe(6);
    });

    it('sends no window when nothing is painted', async () => {
      const pane = createThreadPane();
      const requests = installSync(() => ({
        status: 'stale',
        epoch: 1,
        rev: 2,
        page: page([row('i0', { rev: 2 })]),
      }));

      await pane.switchThread(makeThread({ id: THREAD_ID }));

      expect(requests).toHaveLength(1);
      expect(requests[0].haveWindow).toBeUndefined();
    });
  });

  it('takes a fresh answer at a rev the stamp never named when a window backed it', async () => {
    const dispose = setupEventListeners();
    const pane = createThreadPane();
    registerPaneForTest('sync-pane', pane);
    installSync(() => ({ status: 'stale', epoch: 1, rev: 2, page: page([row('i0', { rev: 2 })]) }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    emitWailsEvent('provider:turn_completed', {
      threadId: THREAD_ID,
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
      historyEpoch: 1,
      historyRev: 30,
    });
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    await removeReplicaWindow(THREAD_ID);

    // The answer does not echo the stamp the pane sent, which on its own
    // would be the "page-less over nothing" anomaly. It is not one here:
    // the request also described the rows, and the server verified those
    // rows against rev 30 in the same read transaction. The reader keeps
    // the window and the pane adopts the rev the server actually holds.
    const requests = installSync(() => ({ status: 'fresh', epoch: 1, rev: 30 }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({ haveEpoch: 1, haveRev: 2 });
    expect(requests[0].haveWindow).toMatchObject({ count: 1, oldestItemId: 'i0' });
    expect(pane.generalErrorKind).toBeNull();
    expect(pane.items.map((it) => it.id)).toEqual(['i0']);

    await pane.switchThread(makeThread({ id: 'other-thread' }));
    expect(await getReplicaWindow(THREAD_ID)).toMatchObject({ epoch: 1, rev: 30 });

    dispose();
  });

  it('reports and refetches a page-less answer with nothing to validate it', async () => {
    const pane = createThreadPane();
    // No L1 snapshot, no replica envelope: the pane painted nothing, so
    // it sent neither a stamp nor a window and a page-less answer
    // describes rows it does not have. The pairing rules make this
    // unreachable, so it is reported and re-asked rather than leaving
    // the pane empty and silent.
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const requests = installSync(() => ({ status: 'fresh', epoch: 1, rev: 30 }));

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(requests).toHaveLength(2);
    expect(requests[0].haveWindow).toBeUndefined();
    expect(requests[1]).toMatchObject({ haveEpoch: UNKNOWN_REV, haveRev: UNKNOWN_REV });
    expect(requests[1].haveWindow).toBeUndefined();
    expect(errors).toHaveBeenCalledWith(
      expect.stringContaining('without a matching painted snapshot'),
    );
    expect(pane.items).toEqual([]);
    expect(pane.generalErrorKind).toBe('history-load');
    vi.restoreAllMocks();
  });
});
