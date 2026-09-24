// A `resync` item event (docs/architecture/thread-replica-sync.md,
// pointer-fork stamps): a write to another thread moved this thread's
// stamps, so what the client shows of it is re-read, and nothing else.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from './eventsItemStream';
import { resetPanesForTest } from './panes.svelte';
import { registerTimelineSurface } from './timelineSurfaces';
import { threadItemCache, type ThreadItemSnapshot } from './threadItemCache';
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
    historyStamp: stamp,
  };
}

describe('resync item event', () => {
  const releases: Array<() => void> = [];
  beforeEach(() => {
    installThreadPaneTestEnv();
    threadItemCache.clear();
  });
  afterEach(() => {
    for (const release of releases.splice(0)) release();
    resetItemEventQueue();
    resetPanesForTest();
  });

  async function forkAndOther() {
    const fork = await buildPane(makeThread({ id: 'fork' }), [], 'pane-fork');
    const other = await buildPane(makeThread({ id: 'other' }), [], 'pane-other');
    const paneRefresh = {
      fork: vi.spyOn(fork, 'refreshFromBackend').mockResolvedValue(),
      other: vi.spyOn(other, 'refreshFromBackend').mockResolvedValue(),
    };
    const surfaceRefresh = { fork: vi.fn(async () => {}), other: vi.fn(async () => {}) };
    for (const threadId of ['fork', 'other'] as const) {
      releases.push(registerTimelineSurface({
        threadId,
        backend: () => undefined,
        apply: () => {},
        refresh: surfaceRefresh[threadId],
      }));
    }
    threadItemCache.set('fork', snapshot('fork', { epoch: 1, rev: 30, attested: false }));
    threadItemCache.set('other', snapshot('other', { epoch: 1, rev: 31, attested: false }));
    return { paneRefresh, surfaceRefresh };
  }

  it('re-reads the named thread and leaves every other one alone', async () => {
    const { paneRefresh, surfaceRefresh } = await forkAndOther();

    applyItemStreamEvent({ action: 'resync', threadId: 'fork' });

    expect(paneRefresh.fork).toHaveBeenCalledOnce();
    expect(surfaceRefresh.fork).toHaveBeenCalledOnce();
    expect(threadItemCache.get('fork')?.historyStamp).toBeNull();
    expect(paneRefresh.other).not.toHaveBeenCalled();
    expect(surfaceRefresh.other).not.toHaveBeenCalled();
    expect(threadItemCache.get('other')?.historyStamp).toEqual({ epoch: 1, rev: 31, attested: false });
  });

  it('re-reads after the item events queued ahead of it apply', async () => {
    const { paneRefresh } = await forkAndOther();
    applyItemStreamEvent({
      action: 'upsert',
      threadId: 'fork',
      item: makeItem({ id: 'own', threadId: 'fork', turnIndex: 2, itemIndex: 0, kind: 'assistant_text', summary: 'own row' }),
    });

    applyItemStreamEvent({ action: 'resync', threadId: 'fork' });
    expect(paneRefresh.fork).not.toHaveBeenCalled();

    flushItemEventQueue();
    await Promise.resolve();
    expect(paneRefresh.fork).toHaveBeenCalledOnce();
  });

  it('ignores a frame that names no thread', async () => {
    const { paneRefresh } = await forkAndOther();
    applyItemStreamEvent({ action: 'resync', threadId: '' });
    applyItemStreamEvent({ action: 'resync', threadId: 'x'.repeat(513) });
    expect(paneRefresh.fork).not.toHaveBeenCalled();
    expect(paneRefresh.other).not.toHaveBeenCalled();
  });
});
