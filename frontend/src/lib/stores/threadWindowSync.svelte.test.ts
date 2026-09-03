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
import {
  __resetThreadHistoryStampsForTest,
  adoptEventStamp,
  dropThreadHistoryStamp,
  getThreadHistoryStamp,
  recordAttestedStamp,
} from './threadHistoryStamps';
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
import type { PaneScrollController } from './threadPaneShared';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import { TransportError } from '../transport/wsClient';

type SyncRequest = { anchorItemId: string; itemBudget: number; haveEpoch: number; haveRev: number };

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
    subagentFolds: null,
  };
}

describe('cold-open window sync', () => {
  beforeEach(async () => {
    __resetReplicaForTest();
    __resetThreadHistoryStampsForTest();
    installPaneMocks();
    await initReplica({
      backendId: `backend-${Math.random().toString(36).slice(2)}`,
      generation: 'gen-1',
      name: '',
    });
  });

  it('paints the replica window before the sync answers', async () => {
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0'), row('i1')], 3, 11));
    const painted: string[][] = [];
    const pane = createThreadPane();
    installSync(() => {
      // Runs after the replica read, so the pane already shows the
      // durable copy: this is the "instant paint" the design exists for.
      painted.push(pane.items.map((it) => it.id));
      return { status: 'fresh', epoch: 3, rev: 11 };
    });

    await pane.switchThread(makeThread({ id: THREAD_ID }));

    expect(painted).toEqual([['i0', 'i1']]);
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
    expect(getThreadHistoryStamp(THREAD_ID)).toEqual({ epoch: 3, rev: 11, attested: true });
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
    expect(getThreadHistoryStamp(THREAD_ID)).toEqual({ epoch: 2, rev: 9, attested: true });
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

    await Promise.all([pane.retryHistoryLoad(), pane.retryHistoryLoad()]);
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
    recordAttestedStampForAnotherThread();
    await pane.switchThread(makeThread({ id: 'other-thread' }));

    expect(await getReplicaWindow(THREAD_ID)).toBeNull();
    vi.restoreAllMocks();
  });

  it('uses a turn_completed stamp for the next request but persists only the attested one', async () => {
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
    expect(getThreadHistoryStamp(THREAD_ID)).toEqual({ epoch: 1, rev: 30, attested: false });

    // Switch away: the window persists, but under the sync-attested
    // stamp (rev 2), never the event-carried one (rev 30). An event
    // stamp can cover a frame this client never received; the attested
    // stamp merely understates, which costs one fetch, not correctness.
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    const envelope = await getReplicaWindow(THREAD_ID);
    expect(envelope).not.toBeNull();
    expect(envelope).toMatchObject({ epoch: 1, rev: 2 });

    // …and the unattested stamp is still what the next request carries.
    const requests = installSync(() => ({ status: 'fresh', epoch: 1, rev: 30 }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    expect(requests.at(-1)).toMatchObject({ haveEpoch: 1, haveRev: 30 });

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
    // The new lineage's page attested normally.
    expect(getThreadHistoryStamp(THREAD_ID)).toEqual({ epoch: 4, rev: 50, attested: true });

    __resetBackendIdentityForTest();
    dispose();
  });

  it('persists a failed-sync replica paint under the ENVELOPE stamp, not the registry one', async () => {
    // The durable copy is at rev 100…
    await putReplicaWindow(THREAD_ID, replicaBody([row('i0')], 1, 100));
    // …while a sync earlier in the session attested rev 150 for this
    // thread and its write-back never fired (the pane was cleared, or
    // the app went away). The registry still holds that attestation.
    recordAttestedStamp(THREAD_ID, 5, 150);
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
    // re-attest it straight into the durable replica.
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
    // One arm for the replica paint, one for the page that replaced
    // 100% of it. A dead-lineage paint is not a reconcile the reader can
    // be left looking at, so the gate closes over the replacement the
    // same way a first content mount does.
    expect(arms).toBe(2);

    __resetBackendIdentityForTest();
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

    pane.clear();
    // Only `runItemWindowSync`'s finally used to clear this, and clear()
    // invalidates that leg — so the ledger stayed armed for the pane's
    // lifetime and every later upsert accumulated into a set the next
    // page application reads as "arrived during my sync, do not drop".
    expect(pane.__syncLedgerArmedForTest()).toBe(false);

    held.resolve();
    await open;
    expect(pane.__syncLedgerArmedForTest()).toBe(false);
  });

  it('does not upgrade a fresh echo of an event stamp into an attested one', async () => {
    const dispose = setupEventListeners();
    const pane = createThreadPane();
    registerPaneForTest('sync-pane', pane);
    // No sync ever attested this thread: its only stamp is event-carried
    // (adopted while the thread streamed in another session state), and
    // the L1 snapshot pairs that stamp with the window on switch-away.
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
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    await removeReplicaWindow(THREAD_ID);
    dropThreadHistoryStamp(THREAD_ID);
    adoptEventStamp(THREAD_ID, 1, 30);

    // The server echoes the event stamp as fresh. That confirms the
    // SERVER's counter — not that this client received every frame up
    // to rev 30 — so the echo must not unlock a replica write.
    installSync(() => ({ status: 'fresh', epoch: 1, rev: 30 }));
    await pane.switchThread(makeThread({ id: THREAD_ID }));
    await pane.switchThread(makeThread({ id: 'other-thread' }));
    expect(await getReplicaWindow(THREAD_ID)).toBeNull();
    expect(getThreadHistoryStamp(THREAD_ID)).toEqual({ epoch: 1, rev: 30, attested: false });

    dispose();
  });
});

/** Attest an unrelated thread, so a global stamp cannot be mistaken for
 *  the gating this test is about. */
function recordAttestedStampForAnotherThread(): void {
  recordAttestedStamp('unrelated-thread', 1, 1);
}
