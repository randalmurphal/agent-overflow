import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  getThreadWorkerCounts,
  refreshThreadWorkerSummaries,
  resetThreadWorkerSummariesForTest,
} from './threadWorkerSummaries.svelte';
import { applyThreadAgentRowsChanged, applyThreadAgentRowsGap } from './eventsThreadAgentRows';
import type { ThreadWorkerSummary } from './bindings';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

function summary(threadId: string, open: number, needYou = 0): ThreadWorkerSummary {
  return { threadId, open, needYou };
}

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
      takePinnedBackend() === 'gpu' ? [summary('remote', 2, 1)] : [summary('local', 3)]);
    await refreshThreadWorkerSummaries();
    expect(list).toHaveBeenCalledTimes(2);
    expect(getThreadWorkerCounts('local')).toEqual({ open: 3, needYou: 0 });
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 2, needYou: 1 });
    expect(getThreadWorkerCounts('none')).toEqual({ open: 0, needYou: 0 });

    // A later bulk read that leaves a thread out clears it, on that computer only.
    setBindingMock('ListThreadWorkerSummaries', async () =>
      takePinnedBackend() === 'gpu' ? [] : [summary('local', 3)]);
    await refreshThreadWorkerSummaries('gpu');
    expect(getThreadWorkerCounts('remote')).toEqual({ open: 0, needYou: 0 });
    expect(getThreadWorkerCounts('local')).toEqual({ open: 3, needYou: 0 });
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
    let answerBulk: (rows: ThreadWorkerSummary[]) => void = () => {};
    setBindingMock('ListThreadWorkerSummaries', () => new Promise((resolve) => { answerBulk = resolve; }));
    const bulk = refreshThreadWorkerSummaries(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    setBindingMock('GetThreadWorkerSummary', async (threadId: string) => summary(threadId, 1, 1));
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 1, needYou: 1 });
    // The bulk answer was read before the frame's change.
    answerBulk([summary('spawner', 5)]);
    await bulk;
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 1, needYou: 1 });
  });

  it('keeps a bulk read\'s newer answer over a single read that started before it and answers after', async () => {
    setBindingMock('ListThreadWorkerSummaries', async () => [summary('spawner', 2)]);
    await refreshThreadWorkerSummaries();
    let answerOne: (row: ThreadWorkerSummary) => void = () => {};
    setBindingMock('GetThreadWorkerSummary', () => new Promise((resolve) => { answerOne = resolve; }));
    applyThreadAgentRowsChanged({ threadId: 'spawner' }, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(500);
    setBindingMock('ListThreadWorkerSummaries', async () => [summary('spawner', 7, 3)]);
    await refreshThreadWorkerSummaries();
    answerOne(summary('spawner', 0));
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadWorkerCounts('spawner')).toEqual({ open: 7, needYou: 3 });
  });

  it('reads only the computer that lost a frame again', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadWorkerSummaries', async () =>
      takePinnedBackend() === 'gpu' ? [summary('remote', 1)] : [summary('local', 1)]);
    await refreshThreadWorkerSummaries();
    const reads: string[] = [];
    setBindingMock('ListThreadWorkerSummaries', async () => {
      const backend = takePinnedBackend() ?? HOME_BACKEND;
      reads.push(backend);
      return backend === 'gpu' ? [summary('remote', 6)] : [];
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
      takePinnedBackend() === 'gpu' ? [summary('remote', 2, 2)] : [summary('local', 1)]);
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
    setBindingMock('ListThreadWorkerSummaries', async () => [summary('spawner', 2, 1)]);
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
