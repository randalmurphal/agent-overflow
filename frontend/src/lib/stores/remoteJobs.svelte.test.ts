import { describe, expect, it, beforeEach, vi } from 'vitest';
import { waitFor } from '@testing-library/svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import {
  __resetRemoteJobsForTest,
  attachRemoteJobs,
  remoteJobComputerName,
  remoteJobRecord,
} from './remoteJobs.svelte';

// The transcript names jobs and computers from the thread's listing: one
// listing per thread, held while a row needs it, re-read when the thread's
// background tasks change, and gone once the last row lets go.
describe('remoteJobs store', () => {
  const THREAD = 'thread-remote';
  const OTHER = 'thread-other';
  const REQUEST = '98312d67-2222-4222-8222-222222222222';
  const record = (state: string, extra: Record<string, unknown> = {}) => ({
    computerId: 'far', computerName: 'Macaroni-air', requestId: REQUEST, threadId: THREAD,
    label: 'Go tests', command: 'go test ./...', notification: 'pending', createdAt: 1,
    receipt: { id: REQUEST, sourceThreadId: THREAD, state, startedAt: 1000, exitCode: -1 },
    ...extra,
  });

  beforeEach(() => {
    resetBindingMocks();
    __resetEntityIndexForTest();
    __resetRemoteJobsForTest();
    noteThread(THREAD, HOME_BACKEND);
    noteThread(OTHER, HOME_BACKEND);
  });

  it('reads the listing once attached and names the job and its computer', async () => {
    const list = setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record('running')]));
    expect(remoteJobRecord(THREAD, REQUEST)).toBeNull();
    expect(remoteJobComputerName(THREAD, 'far')).toBe('');
    const held = attachRemoteJobs(THREAD);
    await waitFor(() => expect(remoteJobRecord(THREAD, REQUEST)?.label).toBe('Go tests'));
    expect(list).toHaveBeenCalledWith(THREAD);
    expect(remoteJobComputerName(THREAD, 'far')).toBe('Macaroni-air');
    expect(remoteJobComputerName(THREAD, 'elsewhere')).toBe('');
    expect(remoteJobRecord(OTHER, REQUEST)).toBeNull();
    held.release();
  });

  it("re-reads on the thread's background task changes and ignores other threads'", async () => {
    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record('running')]));
    const held = attachRemoteJobs(THREAD);
    await waitFor(() => expect(remoteJobRecord(THREAD, REQUEST)?.receipt.state).toBe('running'));

    const settled = setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record('succeeded', { receipt: { id: REQUEST, sourceThreadId: THREAD, state: 'succeeded', startedAt: 1000, finishedAt: 4000, exitCode: 0 } })]));
    emitWailsEvent('provider:background_tasks_changed', { threadId: OTHER });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(settled).not.toHaveBeenCalled();
    expect(remoteJobRecord(THREAD, REQUEST)?.receipt.state).toBe('running');

    emitWailsEvent('provider:background_tasks_changed', { threadId: THREAD });
    await waitFor(() => expect(remoteJobRecord(THREAD, REQUEST)?.receipt.state).toBe('succeeded'));
    expect(settled).toHaveBeenCalledTimes(1);
    held.release();
  });

  it('holds one listing for every row of a thread and drops it with the last', async () => {
    const list = setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record('running')]));
    const first = attachRemoteJobs(THREAD);
    const second = attachRemoteJobs(THREAD);
    await waitFor(() => expect(remoteJobRecord(THREAD, REQUEST)).not.toBeNull());
    expect(list).toHaveBeenCalledTimes(1);
    first.release();
    expect(remoteJobRecord(THREAD, REQUEST)).not.toBeNull();
    second.release();
    expect(remoteJobRecord(THREAD, REQUEST)).toBeNull();
    // A released thread's events read nothing.
    emitWailsEvent('provider:background_tasks_changed', { threadId: THREAD });
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(list).toHaveBeenCalledTimes(1);
  });

  it('keeps a listing that fails to read as unknown names', async () => {
    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => { throw new Error('offline'); }));
    const held = attachRemoteJobs(THREAD);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(remoteJobRecord(THREAD, REQUEST)).toBeNull();
    expect(remoteJobComputerName(THREAD, 'far')).toBe('');
    held.release();
  });
});
