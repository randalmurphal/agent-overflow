import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  getThreadWorkerCounts,
  refreshThreadWorkerSummaries,
  resetThreadWorkerSummariesForTest,
  threadWorkerSummaryRetentionForTest,
} from './threadWorkerSummaries.svelte';
import { applyThreadAgentRowsChanged, applyThreadAgentRowsGap } from './eventsThreadAgentRows';
import type { ThreadWorkerSummary, ThreadWorkerSummaryPage } from './bindings';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

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

const EMPTY_RETENTION = { counts: 0, owners: 0, landed: 0, snapshots: 0 };

describe('thread worker summaries', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    resetThreadWorkerSummariesForTest();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

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
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('a', 1), summary('z', 4)]));
    await refreshThreadWorkerSummaries();
    setBindingMock('ListThreadWorkerSummaries', async (after: string) => {
      if (after === '') return page([summary('a', 3)], 'm');
      throw new Error('database is locked');
    });
    await refreshThreadWorkerSummaries();
    expect(getThreadWorkerCounts('a')).toEqual({ open: 1, needYou: 0 });
    expect(getThreadWorkerCounts('z')).toEqual({ open: 4, needYou: 0 });
    expect(error).toHaveBeenCalledTimes(1);
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
    expect(threadWorkerSummaryRetentionForTest()).toEqual({ counts: 50, owners: 50, landed: 0, snapshots: 1 });
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

  it('keeps the last counts when a read fails, and reports it', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreadWorkerSummaries', async () => page([summary('spawner', 2, 1)]));
    await refreshThreadWorkerSummaries();
    setBindingMock('GetThreadWorkerSummary', async () => { throw new Error('database is locked'); });
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => { throw new Error('database is locked'); });
    await refreshThreadWorkerSummaries();
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 2, needYou: 1 });
    expect(error).toHaveBeenCalledTimes(2);
  });
});
