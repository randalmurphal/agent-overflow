import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { answerAsV0019 } from '../../test/helpers/olderComputer';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';
import {
  getThreadWorkerCounts,
  refreshThreadWorkerSummaries,
  threadWorkerSummaryRetentionForTest,
} from './threadWorkerSummaries.svelte';
import { applyThreadAgentRowsChanged, applyThreadAgentRowsGap } from './eventsThreadAgentRows';
import { applyThreadDeleted, applyThreadUpdated } from './eventsThreadRows';
import { DisconnectedError } from '../transport/wsClient';
import type { ThreadWorkerSummary, ThreadWorkerSummaryPage } from './bindings';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { stageBackend } from '../../test/helpers/backends';

function summary(threadId: string, open: number, needYou = 0): ThreadWorkerSummary {
  return { threadId, open, needYou };
}

/** One page, the last unless `next` names where the following one starts. */
function page(summaries: ThreadWorkerSummary[], next = ''): ThreadWorkerSummaryPage {
  return { summaries, next };
}

/** A read that answers when the test says. */
function held<T>(): { promise: Promise<T>; answer: (value: T) => void } {
  let answer: (value: T) => void = () => {};
  const promise = new Promise<T>((resolve) => { answer = resolve; });
  return { promise, answer };
}

const EMPTY_RETENTION = { counts: 0, owners: 0, landed: 0, snapshots: 0, failed: 0 };

describe('thread worker summaries', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });
  // After the reset above, which would drop its binding mock.
  const diagnostics = installDiagnosticsCapture();
  const reported = async () => (await diagnostics.all()).filter((record) => record.message === 'threadWorkerSummaries: read failed');

  it('reads every computer in bulk and files each count under the computer that answered', async () => {
    stageBackend({ id: 'gpu' });
    const list = setBindingMock('ListThreadWorkerSummaries', async () =>
      page(takePinnedBackend() === 'gpu' ? [summary('remote', 2, 1)] : [summary('local', 3)]));
    await refreshThreadWorkerSummaries();
    expect(list).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('local')).toEqual({ open: 3, needYou: 0 });
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 2, needYou: 1 });
    expect(getThreadWorkerCounts('none')).toEqual({ open: 0, needYou: 0 });

    // A later bulk read that leaves a thread out clears it, on that computer only.
    setBindingMock('ListThreadWorkerSummaries', async () =>
      page(takePinnedBackend() === 'gpu' ? [] : [summary('local', 3)]));
    await refreshThreadWorkerSummaries('gpu');
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('local')).toEqual({ open: 3, needYou: 0 });
  });

  it('reads every page of a computer, and clears a thread the pages leave out only after the last', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('a', 1), summary('z', 4)]));
    await refreshThreadWorkerSummaries();
    const second = held<ThreadWorkerSummaryPage>();
    const asked: string[] = [];
    setBindingMock('ListThreadWorkerSummaries', (after: string) => {
      asked.push(after);
      return after === '' ? Promise.resolve(page([summary('a', 2)], 'm')) : second.promise;
    });
    const bulk = refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(0);
    // The first page answered: nothing it holds or leaves out has landed.
    expect(asked).toEqual(['', 'm']);
    expect(getThreadWorkerCounts('a')).toEqual({ open: 1, needYou: 0 });
    expect(getThreadWorkerCounts('z')).toEqual({ open: 4, needYou: 0 });
    second.answer(page([summary('q', 5, 5)]));
    await bulk;
    expect(getThreadWorkerCounts('a')).toEqual({ open: 2, needYou: 0 });
    expect(getThreadWorkerCounts('q')).toEqual({ open: 5, needYou: 5 });
    expect(getThreadWorkerCounts('z')).toEqual({ open: 0, needYou: 0 });
  });

  it('keeps the counts a computer had when a later page of its bulk read fails', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('a', 1), summary('z', 4)]));
    await refreshThreadWorkerSummaries();
    setBindingMock('ListThreadWorkerSummaries', async (after: string) => {
      if (after === '') return page([summary('a', 3)], 'm');
      throw new Error('database is locked');
    });
    await refreshThreadWorkerSummaries();
    expect(getThreadWorkerCounts('a')).toEqual({ open: 1, needYou: 0 });
    expect(getThreadWorkerCounts('z')).toEqual({ open: 4, needYou: 0 });
    expect(await reported()).toHaveLength(1);
  });

  it('reads a framed thread once for a burst, from the computer that sent the frames', async () => {
    stageBackend({ id: 'gpu' });
    const asked: string[] = [];
    const get = setBindingMock('GetThreadWorkerSummary', async (threadId: string) => {
      asked.push(`${takePinnedBackend()}:${threadId}`);
      return summary(threadId, 4, 2);
    });
    for (let i = 0; i < 5; i++) applyThreadAgentRowsChanged({ threadId: 'spawner', id: `t${i}` }, 'gpu');
    expect(get).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(500);
    expect(asked).toEqual(['gpu:spawner']);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 4, needYou: 2 });
  });

  it('keeps a frame\'s newer answer over a bulk read that started before it and answers after', async () => {
    const bulkAnswer = held<ThreadWorkerSummaryPage>();
    setBindingMock('ListThreadWorkerSummaries', () => bulkAnswer.promise);
    const bulk = refreshThreadWorkerSummaries(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    setBindingMock('GetThreadWorkerSummary', async (threadId: string) => summary(threadId, 1, 1));
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 1, needYou: 1 });
    // The bulk answer was read before the frame's change.
    bulkAnswer.answer(page([summary('spawner', 5)]));
    await bulk;
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 1, needYou: 1 });
  });

  it('keeps a bulk read\'s newer answer over a single read that started before it and answers after', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2)]));
    await refreshThreadWorkerSummaries();
    const one = held<ThreadWorkerSummary>();
    setBindingMock('GetThreadWorkerSummary', () => one.promise);
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 7, 3)]));
    await refreshThreadWorkerSummaries();
    one.answer(summary('spawner', 0));
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 7, needYou: 3 });
  });

  it('keeps a newer bulk read\'s absence over a single read that started before it, for a thread never counted', async () => {
    const one = held<ThreadWorkerSummary>();
    setBindingMock('GetThreadWorkerSummary', () => one.promise);
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    // The read was out when its work closed; a resync reads the computer
    // and finds no thread with an open worker.
    setBindingMock('ListThreadWorkerSummaries', async () => page([]));
    await refreshThreadWorkerSummaries();
    one.answer(summary('spawner', 1));
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 0, needYou: 0 });
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, snapshots: 1 });
  });

  it('keeps a newer bulk read over an older one that answers after it, and its watermark', async () => {
    const older = held<ThreadWorkerSummaryPage>();
    setBindingMock('ListThreadWorkerSummaries', () => older.promise);
    const first = refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(0);
    // A thread's own read starts between the two bulk reads.
    const one = held<ThreadWorkerSummary>();
    setBindingMock('GetThreadWorkerSummary', () => one.promise);
    applyThreadAgentRowsChanged({ threadId: 'ghost' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 3)]));
    await refreshThreadWorkerSummaries();
    older.answer(page([summary('spawner', 9), summary('other', 1)]));
    await first;
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 3, needYou: 0 });
    expect(getThreadWorkerCounts('other')).toEqual({ open: 0, needYou: 0 });
    // The newer read left the thread out; the older read answering late
    // did not take its place as the computer's watermark.
    one.answer(summary('ghost', 8));
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadWorkerCounts('ghost')).toEqual({ open: 0, needYou: 0 });
  });

  it('applies no answer of a computer detached and attached again while the read was out', async () => {
    stageBackend({ id: 'gpu' });
    const one = held<ThreadWorkerSummary>();
    setBindingMock('GetThreadWorkerSummary', () => one.promise);
    applyThreadAgentRowsChanged({ threadId: 'remote' }, 'gpu');
    await vi.advanceTimersByTimeAsync(500);
    const bulk = held<ThreadWorkerSummaryPage>();
    setBindingMock('ListThreadWorkerSummaries', () => bulk.promise);
    const reading = refreshThreadWorkerSummaries('gpu');
    await vi.advanceTimersByTimeAsync(0);
    detachBackend('gpu');
    stageBackend({ id: 'gpu' });
    one.answer(summary('remote', 2));
    bulk.answer(page([summary('remote', 4), summary('other', 1)]));
    await reading;
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('other')).toEqual({ open: 0, needYou: 0 });
    expect(threadWorkerSummaryRetentionForTest()).toEqual(EMPTY_RETENTION);
  });

  it('holds nothing for threads whose workers closed', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () =>
      page(Array.from({ length: 50 }, (_, i) => summary(`t${i}`, 1 + (i % 3)))));
    await refreshThreadWorkerSummaries();
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, counts: 50, owners: 50, snapshots: 1 });
    // Each thread's work closes and a frame reads it again.
    setBindingMock('GetThreadWorkerSummary', async (threadId: string) => summary(threadId, 0));
    for (let i = 0; i < 50; i++) applyThreadAgentRowsChanged({ threadId: `t${i}` }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    for (let i = 0; i < 50; i++) expect(getThreadWorkerCounts(`t${i}`)).toEqual({ open: 0, needYou: 0 });
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, snapshots: 1 });
    detachBackend(HOME_BACKEND);
    expect(threadWorkerSummaryRetentionForTest()).toEqual(EMPTY_RETENTION);
  });

  it('reads only the computer that lost a frame again', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadWorkerSummaries', async () =>
      page(takePinnedBackend() === 'gpu' ? [summary('remote', 1)] : [summary('local', 1)]));
    await refreshThreadWorkerSummaries();
    const reads: string[] = [];
    setBindingMock('ListThreadWorkerSummaries', async () => {
      const backend = takePinnedBackend() ?? HOME_BACKEND;
      reads.push(backend);
      return page(backend === 'gpu' ? [summary('remote', 6)] : []);
    });
    applyThreadAgentRowsGap('gpu');
    await vi.advanceTimersByTimeAsync(0);
    expect(reads).toEqual(['gpu']);
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 6, needYou: 0 });
    expect(getThreadWorkerCounts('local')).toEqual({ open: 1, needYou: 0 });
  });

  it('drops a detached computer\'s counts and its queued reads', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadWorkerSummaries', async () =>
      page(takePinnedBackend() === 'gpu' ? [summary('remote', 2, 2)] : [summary('local', 1)]));
    await refreshThreadWorkerSummaries();
    const get = setBindingMock('GetThreadWorkerSummary', async (threadId: string) => summary(threadId, 9));
    applyThreadAgentRowsChanged({ threadId: 'remote' }, 'gpu');
    detachBackend('gpu');
    await vi.advanceTimersByTimeAsync(500);
    expect(get).not.toHaveBeenCalled();
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('local')).toEqual({ open: 1, needYou: 0 });
  });

  it('keeps the last counts when a read fails, and reports it to the diagnostics log', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2, 1)]));
    await refreshThreadWorkerSummaries();
    setBindingMock('GetThreadWorkerSummary', async () => { throw new Error('database is locked'); });
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => { throw new Error('database is locked'); });
    await refreshThreadWorkerSummaries();
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 2, needYou: 1 });
    expect(await reported()).toHaveLength(2);
    expect((await reported())[0]!.detail).toContain('database is locked');
    expect(diagnostics.warnings().filter((line) => line.includes('database is locked'))).toHaveLength(2);
    expect(error).not.toHaveBeenCalled();
  });

  it('reads a computer whose first bulk read failed again on a growing backoff until one answers', async () => {
    let fail = 2;
    const list = setBindingMock('ListThreadWorkerSummaries', async () => {
      if (fail-- > 0) throw new Error('database is locked');
      return page([summary('spawner', 3, 1)]);
    });
    await refreshThreadWorkerSummaries();
    expect(list).toHaveBeenCalledTimes(1);
    // Until a read answers the row shows no badge, as for no workers.
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 0, needYou: 0 });
    await vi.advanceTimersByTimeAsync(2_999);
    expect(list).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 0, needYou: 0 });
    // The second retry waits twice as long.
    await vi.advanceTimersByTimeAsync(5_999);
    expect(list).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(3);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 3, needYou: 1 });
    expect(await reported()).toHaveLength(2);
    // Once it answered nothing reads it again.
    await vi.advanceTimersByTimeAsync(120_000);
    expect(list).toHaveBeenCalledTimes(3);
    expect(threadWorkerSummaryRetentionForTest().failed).toBe(0);
  });

  // v0.0.19 has no worker summaries: its threads have no workers to count,
  // and the read is neither reported nor read again.
  it('counts no workers on a computer without the read, and reads it once', async () => {
    answerAsV0019();
    await refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(getBindingMock('ListThreadWorkerSummaries')).toHaveBeenCalledTimes(1);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 0, needYou: 0 });
    expect(await reported()).toHaveLength(0);
    expect(threadWorkerSummaryRetentionForTest().failed).toBe(0);
  });

  it('reads a thread whose read failed again after a backoff, keeping its last counts meanwhile', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2, 1)]));
    await refreshThreadWorkerSummaries();
    let fail = 2;
    const get = setBindingMock('GetThreadWorkerSummary', async (threadId: string) => {
      if (fail-- > 0) throw new Error('database is locked');
      return summary(threadId, 4, 0);
    });
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(1);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 2, needYou: 1 });
    await vi.advanceTimersByTimeAsync(3_000);
    expect(get).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 2, needYou: 1 });
    await vi.advanceTimersByTimeAsync(6_000);
    expect(get).toHaveBeenCalledTimes(3);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 4, needYou: 0 });
    await vi.advanceTimersByTimeAsync(120_000);
    expect(get).toHaveBeenCalledTimes(3);
    expect(threadWorkerSummaryRetentionForTest().failed).toBe(0);

    // The answer reset the curve: the next failure waits the first step again.
    fail = 1;
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(4);
    await vi.advanceTimersByTimeAsync(3_000);
    expect(get).toHaveBeenCalledTimes(5);
  });

  it('retries no read a newer one answered for', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2)]));
    await refreshThreadWorkerSummaries();
    const one = held<ThreadWorkerSummary>();
    const get = setBindingMock('GetThreadWorkerSummary', () => one.promise);
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 5)]));
    await refreshThreadWorkerSummaries();
    one.answer(Promise.reject(new Error('database is locked')) as unknown as ThreadWorkerSummary);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(get).toHaveBeenCalledTimes(1);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 5, needYou: 0 });
  });

  it('keeps the retry of a failed read when an older bulk read answers after it', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2)]));
    await refreshThreadWorkerSummaries();
    const bulk = held<ThreadWorkerSummaryPage>();
    setBindingMock('ListThreadWorkerSummaries', () => bulk.promise);
    const reading = refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(0);
    // A change after the bulk read began: its own read fails.
    let fail = true;
    const get = setBindingMock('GetThreadWorkerSummary', async () => {
      if (fail) throw new Error('database is locked');
      return summary('spawner', 3, 1);
    });
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(1);
    // The older bulk read answers with what it saw before the change.
    bulk.answer(page([summary('spawner', 2)]));
    await reading;
    fail = false;
    await vi.advanceTimersByTimeAsync(3_000);
    expect(get).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 3, needYou: 1 });
  });

  it('keeps the retry of a failed bulk read when an older one answers after it', async () => {
    const older = held<ThreadWorkerSummaryPage>();
    let calls = 0;
    const list = setBindingMock('ListThreadWorkerSummaries', () => {
      calls += 1;
      if (calls === 1) return older.promise;
      if (calls === 2) return Promise.reject(new Error('database is locked'));
      return Promise.resolve(page([summary('spawner', 4)]));
    });
    const first = refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(0);
    await refreshThreadWorkerSummaries();
    older.answer(page([summary('spawner', 1)]));
    await first;
    await vi.advanceTimersByTimeAsync(3_500);
    expect(list).toHaveBeenCalledTimes(3);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 4, needYou: 0 });
  });

  it('leaves a read the connection dropped to the reconnect, which reads the computer again', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => { throw new DisconnectedError(); });
    const get = setBindingMock('GetThreadWorkerSummary', async () => { throw new DisconnectedError(); });
    await refreshThreadWorkerSummaries();
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(get).toHaveBeenCalledTimes(1);
    expect(await reported()).toHaveLength(0);
    const list = setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 1, 1)]));
    // The reconnect's resync.
    await refreshThreadWorkerSummaries();
    expect(list).toHaveBeenCalledTimes(1);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 1, needYou: 1 });
  });

  it('forgets a deleted thread\'s counts, and every answer for it still out', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('gone', 3, 2), summary('kept', 1), summary('failing', 2)]));
    await refreshThreadWorkerSummaries();
    const one = held<ThreadWorkerSummary>();
    const failing = held<ThreadWorkerSummary>();
    const get = setBindingMock('GetThreadWorkerSummary', (threadId: string) => (threadId === 'gone' ? one.promise : failing.promise));
    applyThreadAgentRowsChanged({ threadId: 'gone' }, HOME_BACKEND);
    applyThreadAgentRowsChanged({ threadId: 'failing' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(2);
    const bulk = held<ThreadWorkerSummaryPage>();
    setBindingMock('ListThreadWorkerSummaries', () => bulk.promise);
    const reading = refreshThreadWorkerSummaries();
    await vi.advanceTimersByTimeAsync(0);

    applyThreadUpdated({ action: 'deleted', id: 'gone' });
    applyThreadUpdated({ action: 'deleted', id: 'failing' });
    expect(getThreadWorkerCounts('gone')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('failing')).toEqual({ open: 0, needYou: 0 });

    // The reads out when the threads went answer: none lands, and the one
    // that failed for a deleted thread is not retried.
    one.answer(summary('gone', 4, 4));
    failing.answer(Promise.reject(new Error('thread not found')) as unknown as ThreadWorkerSummary);
    bulk.answer(page([summary('gone', 5), summary('failing', 5), summary('kept', 2)]));
    await reading;
    await vi.advanceTimersByTimeAsync(120_000);
    expect(get).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('gone')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('failing')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('kept')).toEqual({ open: 2, needYou: 0 });
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, counts: 1, owners: 1, snapshots: 1 });

    // A thread deleted while its failed read waits on the backoff is not read again.
    get.mockImplementation(async () => { throw new Error('database is locked'); });
    applyThreadAgentRowsChanged({ threadId: 'kept' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(3);
    applyThreadUpdated({ action: 'deleted', id: 'kept' });
    await vi.advanceTimersByTimeAsync(120_000);
    expect(get).toHaveBeenCalledTimes(3);
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, snapshots: 1 });

    // A frame queued for a thread deleted before its read starts reads nothing.
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('kept', 1)]));
    await refreshThreadWorkerSummaries();
    applyThreadAgentRowsChanged({ threadId: 'kept' }, HOME_BACKEND);
    expect(getThreadWorkerCounts('kept')).toEqual({ open: 1, needYou: 0 });
    applyThreadUpdated({ action: 'deleted', id: 'kept' });
    await vi.advanceTimersByTimeAsync(500);
    expect(get).toHaveBeenCalledTimes(3);
    expect(getThreadWorkerCounts('kept')).toEqual({ open: 0, needYou: 0 });
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ ...EMPTY_RETENTION, snapshots: 1 });
  });

  it('forgets the counts of a thread a draft save found deleted, before its broadcast', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('gone', 2, 1), summary('kept', 1)]));
    await refreshThreadWorkerSummaries();
    applyThreadDeleted('gone');
    expect(getThreadWorkerCounts('gone')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('kept')).toEqual({ open: 1, needYou: 0 });
  });
});
