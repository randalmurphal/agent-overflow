// Transport-gap stamp discipline (docs/specs/thread-replica-sync.md
// §3.4). A gap carries no entity key, so the only safe answer is to
// forget what we claimed to know about the backend's counters — in every
// tier that holds a stamp, not just the registry.
import { beforeEach, describe, expect, it } from 'vitest';
import { applyTransportGap } from './eventsTransportGap';
import {
  __resetThreadHistoryStampsForTest,
  getThreadHistoryStamp,
  recordAttestedStamp,
} from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem } from '../../test/helpers/chat';
import type { ThreadItemSnapshot } from './threadItemCache';
import type { ThreadHistoryStamp } from './threadHistoryStamps';

function snapshot(threadId: string, stamp: ThreadHistoryStamp): ThreadItemSnapshot {
  return {
    items: [makeItem({ id: `${threadId}-i0`, threadId })],
    oldestLoadedCursor: null,
    newestLoadedCursor: null,
    oldestLoadedTurnIndex: 0,
    newestLoadedTurnIndex: 0,
    hasMoreHistory: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
    historyStamp: stamp,
  };
}

describe('transport gap', () => {
  beforeEach(() => {
    __resetThreadHistoryStampsForTest();
    threadItemCache.clear();
    // The gap handler resyncs the sidebar; both legs swallow their own
    // errors, but an unmocked binding would still log and toast.
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  it('strips unattested stamps from cached snapshots and keeps attested ones', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));
    threadItemCache.set('t-sync', snapshot('t-sync', { epoch: 1, rev: 12, attested: true }));
    recordAttestedStamp('t-sync', 1, 12);

    applyTransportGap({ channel: 'provider:item_event', seq: 7 });

    // The registry is dropped wholesale — it is one entry per thread and
    // re-earning it costs one window fetch.
    expect(getThreadHistoryStamp('t-sync')).toBeNull();
    // The unattested COPY paired with L1 rows would otherwise outlive the
    // drop and name a rev whose frames this gap ate.
    expect(threadItemCache.get('t-event')?.historyStamp).toBeNull();
    expect(threadItemCache.get('t-event')?.items).toHaveLength(1);
    // An attested copy describes rows a sync returned; any mutation since
    // has advanced the backend's rev past it, so `fresh` cannot lie.
    expect(threadItemCache.get('t-sync')?.historyStamp).toEqual({
      epoch: 1,
      rev: 12,
      attested: true,
    });
  });

  it('strips them on an unknown channel too', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));

    applyTransportGap({ channel: 'future:channel', seq: 1 });

    expect(threadItemCache.get('t-event')?.historyStamp).toBeNull();
  });

  it('leaves stamps alone on the self-repairing channels', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));
    recordAttestedStamp('t-sync', 1, 12);

    applyTransportGap({ channel: 'system:stats', seq: 3 });

    expect(threadItemCache.get('t-event')?.historyStamp).toEqual({
      epoch: 1,
      rev: 30,
      attested: false,
    });
    expect(getThreadHistoryStamp('t-sync')).not.toBeNull();
  });
});
