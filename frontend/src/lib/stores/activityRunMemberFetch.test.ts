import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createActivityRunMemberFetch } from './activityRunMemberFetch';
import { foldPageStub } from './activityRunStubs';
import type { ActivityRunRecords } from './activityRunStubs';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
import { noteThread, __resetEntityIndexForTest } from '../transport/entityIndex';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const stale = () => Object.assign(new Error('changed'), { code: 'activity_run_stale' });
function fixture() {
  const records: ActivityRunRecords = new Map();
  const stub = { firstItemId: 'run', loadedFirstItemId: '', loadedLastItemId: '' } as ActivityRunStub;
  const record = foldPageStub(records, stub, null);
  const reloadWindow = vi.fn(async () => {});
  const onStubApplied = vi.fn(() => [] as string[]);
  const reportFailure = vi.fn();
  const fetcher = createActivityRunMemberFetch({
    threadId: () => 'thread', records: () => records,
    shape: () => ({ inlinePreviews: true, runWindowRows: 30, maxBytes: 10000 }),
    mountMembers: vi.fn(() => []), reloadWindow, reportFailure, onStubApplied,
  });
  return { records, record, stub, fetcher, reloadWindow, onStubApplied, reportFailure };
}
beforeEach(() => { resetBindingMocks(); __resetEntityIndexForTest(); vi.useFakeTimers(); });
afterEach(() => { __resetEntityIndexForTest(); vi.useRealTimers(); });

it('does not turn a failed background refresh into an automatic retry loop', async () => {
  const f = fixture();
  const rpc = vi.fn(async () => { throw new Error('offline'); });
  setBindingMock('ListActivityRunMembers', rpc);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(5000);
  expect(rpc).toHaveBeenCalledTimes(1);
  expect(f.record.dirty).toBe(true);
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(500);
  expect(rpc).toHaveBeenCalledTimes(2);
  f.fetcher.dispose();
});

for (const outcome of ['success', 'stale'] as const) {
  it(`ignores a late ${outcome} after returning to the same thread`, async () => {
    const f = fixture();
    const old = deferred<unknown>();
    const next = deferred<unknown>();
    const rpc = vi.fn().mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise);
    setBindingMock('ListActivityRunMembers', rpc);
    const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
    f.fetcher.reset();
    const second = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
    if (outcome === 'success') old.resolve({ items: [], stub: f.stub });
    else old.reject(stale());
    await first;
    expect(f.onStubApplied).not.toHaveBeenCalled();
    expect(f.reloadWindow).not.toHaveBeenCalled();
    expect(f.reportFailure).not.toHaveBeenCalled();
    await f.fetcher.fetch('run', { direction: 'before', limit: 5 });
    expect(rpc).toHaveBeenCalledTimes(2); // Old finally must not release the new claim.
    next.resolve({ items: [], stub: f.stub });
    await second;
    expect(f.onStubApplied).toHaveBeenCalledOnce();
    f.fetcher.dispose();
  });
}

it('coalesces stale refusals and waits for recovery before fetching again', async () => {
  const f = fixture();
  foldPageStub(f.records, { ...f.stub, firstItemId: 'second' }, null);
  const recovering = deferred<void>();
  f.reloadWindow.mockReturnValue(recovering.promise);
  const rpc = vi.fn<(...args: unknown[]) => Promise<unknown>>(async () => { throw stale(); });
  setBindingMock('ListActivityRunMembers', rpc);
  const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  const second = f.fetcher.fetch('second', { direction: 'before', limit: 5 });
  await vi.advanceTimersByTimeAsync(1);
  expect(f.reloadWindow).toHaveBeenCalledOnce();
  const queued = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  expect(rpc).toHaveBeenCalledTimes(2);
  rpc.mockResolvedValue({ items: [], stub: f.stub });
  recovering.resolve();
  await Promise.all([first, second, queued]);
  expect(rpc).toHaveBeenCalledTimes(3);
  expect(f.reportFailure.mock.calls.every(call => call[2] === true)).toBe(true);
  f.fetcher.dispose();
});

it('discards a members answer superseded by a page that moved its loaded edge', async () => {
  const f = fixture();
  const result = deferred<unknown>();
  setBindingMock('ListActivityRunMembers', () => result.promise);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  foldPageStub(f.records, { ...f.stub }, { firstItemId: 'moved', lastItemId: 'moved', items: [] });
  result.resolve({ items: [], stub: f.stub });
  await pending;
  expect(f.onStubApplied).not.toHaveBeenCalled();
  f.fetcher.dispose();
});

it('reports a failed shared recovery once without rejecting callers or retrying itself', async () => {
  const f = fixture();
  foldPageStub(f.records, { ...f.stub, firstItemId: 'second' }, null);
  const recovering = deferred<void>();
  f.reloadWindow.mockReturnValue(recovering.promise);
  setBindingMock('ListActivityRunMembers', async () => { throw stale(); });
  const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  const second = f.fetcher.fetch('second', { direction: 'before', limit: 5 });
  await vi.advanceTimersByTimeAsync(1);
  f.fetcher.scheduleRefresh();
  recovering.reject(new Error('offline'));
  await expect(Promise.all([first, second])).resolves.toEqual([[], []]);
  await vi.advanceTimersByTimeAsync(5000);
  expect(f.reloadWindow).toHaveBeenCalledOnce();
  expect(f.reportFailure.mock.calls.filter(call => call[2] === false)).toHaveLength(1);
  f.fetcher.dispose();
});

it('honors a new refresh requested during successful recovery', async () => {
  const f = fixture();
  const recovering = deferred<void>();
  f.reloadWindow.mockReturnValue(recovering.promise);
  const rpc = vi.fn().mockRejectedValueOnce(stale()).mockResolvedValue({ items: [], stub: f.stub });
  setBindingMock('ListActivityRunMembers', rpc);
  const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  await vi.advanceTimersByTimeAsync(1);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(1000);
  expect(rpc).toHaveBeenCalledOnce();
  recovering.resolve();
  await first;
  await vi.advanceTimersByTimeAsync(500);
  expect(rpc).toHaveBeenCalledTimes(2);
  f.fetcher.dispose();
});

it('preserves a background refresh requested while a superseded member page is in flight', async () => {
  const f = fixture();
  const firstAnswer = deferred<unknown>();
  const rpc = vi.fn().mockReturnValueOnce(firstAnswer.promise).mockResolvedValue({ items: [], stub: f.stub });
  setBindingMock('ListActivityRunMembers', rpc);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  foldPageStub(f.records, { ...f.stub }, { firstItemId: 'moved', lastItemId: 'moved', items: [] });
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(1000);
  expect(rpc).toHaveBeenCalledOnce();
  firstAnswer.resolve({ items: [], stub: f.stub });
  await pending;
  await vi.advanceTimersByTimeAsync(1000);
  expect(rpc).toHaveBeenCalledTimes(2);
  expect(f.onStubApplied).toHaveBeenCalledOnce();
  f.fetcher.dispose();
});

it('does not start deferred recovery after the pane reset', async () => {
  const f = fixture();
  const response = deferred<unknown>();
  setBindingMock('ListActivityRunMembers', () => response.promise);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  response.reject(stale());
  await Promise.resolve(); // The refusal queued recovery, but its read has not started.
  f.fetcher.reset();
  await pending;
  expect(f.reloadWindow).not.toHaveBeenCalled();
  f.fetcher.dispose();
});

it('waits for a background stub refresh before loading requested history', async () => {
  const f = fixture();
  const background = deferred<unknown>();
  const rpc = setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: f.stub }));
  rpc.mockReturnValueOnce(background.promise);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(200);
  expect(rpc).toHaveBeenCalledOnce();
  let completed = false;
  const read = f.fetcher.fetch('run', { direction: 'before', limit: 25 }).then(() => { completed = true; });
  await Promise.resolve();
  expect(completed).toBe(false);
  background.resolve({ items: [], stub: f.stub });
  await read;
  expect(rpc).toHaveBeenCalledTimes(2);
  expect(rpc.mock.calls[1][1]).toMatchObject({ direction: 'before', limit: 25 });
  f.fetcher.dispose();
});

it.each(['reset', 'dispose'] as const)('releases a queued history read on %s without waiting for the old RPC', async (operation) => {
  const f = fixture();
  const background = deferred<unknown>();
  const rpc = setBindingMock('ListActivityRunMembers', () => background.promise);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(200);
  const read = f.fetcher.fetch('run', { direction: 'before', limit: 25 });
  f.fetcher[operation]();
  await expect(read).resolves.toEqual([]);
  expect(rpc).toHaveBeenCalledOnce();
  background.resolve({ items: [], stub: f.stub });
  await vi.advanceTimersByTimeAsync(1000);
  expect(f.onStubApplied).not.toHaveBeenCalled();
  expect(rpc).toHaveBeenCalledOnce();
  f.fetcher.dispose();
});

it.each(['reset', 'dispose'] as const)('cancels a reader waiting for recovery on %s', async (operation) => {
  const f = fixture();
  const recovering = deferred<void>();
  f.reloadWindow.mockReturnValue(recovering.promise);
  const rpc = setBindingMock('ListActivityRunMembers', async () => { throw stale(); });
  const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  await vi.advanceTimersByTimeAsync(1);
  const read = f.fetcher.fetch('run', { direction: 'before', limit: 25 });
  f.fetcher[operation]();
  await expect(read).resolves.toEqual([]);
  recovering.resolve();
  await first;
  expect(rpc).toHaveBeenCalledOnce();
  f.fetcher.dispose();
});

it('keeps a requested member page when concurrent metadata describes the same loaded span', async () => {
  const f = fixture();
  const result = deferred<unknown>();
  setBindingMock('ListActivityRunMembers', () => result.promise);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 25 });
  foldPageStub(f.records, { ...f.stub }, null);
  result.resolve({ items: [], stub: f.stub });
  await pending;
  expect(f.onStubApplied).toHaveBeenCalledExactlyOnceWith(f.stub);
  f.fetcher.dispose();
});

it.each([
  ['before', '', 'appended', true],
  ['before', 'moved', '', false],
  ['after', 'prepended', '', true],
  ['after', '', 'moved', false],
  ['around', 'moved', '', false],
  ['around', '', 'moved', false],
] as const)('fences %s history by its requested edge (%s, %s)', async (direction, firstItemId, lastItemId, accepted) => {
  const f = fixture();
  const result = deferred<unknown>();
  setBindingMock('ListActivityRunMembers', () => result.promise);
  const pending = f.fetcher.fetch('run', { direction, limit: 25, aroundItemId: 'target' });
  foldPageStub(f.records, { ...f.stub }, { firstItemId, lastItemId, items: [] });
  result.resolve({ items: [], stub: f.stub });
  await pending;
  expect(f.onStubApplied).toHaveBeenCalledTimes(accepted ? 1 : 0);
  f.fetcher.dispose();
});

it('drops queued history when the conversation changes computers', async () => {
  const f = fixture();
  const background = deferred<unknown>();
  const rpc = setBindingMock('ListActivityRunMembers', () => background.promise);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(200);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 25 });
  noteThread('thread', 'moved');
  background.resolve({ items: [], stub: f.stub });
  await expect(pending).resolves.toEqual([]);
  expect(rpc).toHaveBeenCalledOnce();
  expect(f.onStubApplied).not.toHaveBeenCalled();
  f.fetcher.dispose();
});

it('re-reads the current run description after queued maintenance finishes', async () => {
  const f = fixture();
  const background = deferred<unknown>();
  const rpc = setBindingMock('ListActivityRunMembers', async () => ({ items: [], stub: f.stub }));
  rpc.mockReturnValueOnce(background.promise);
  f.record.dirty = true;
  f.fetcher.scheduleRefresh();
  await vi.advanceTimersByTimeAsync(200);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 25 });
  f.records.delete('run');
  foldPageStub(f.records, { ...f.stub }, { firstItemId: 'new-first', lastItemId: 'new-last', items: [] });
  background.resolve({ items: [], stub: f.stub });
  await pending;
  expect(rpc).toHaveBeenCalledTimes(2);
  expect(rpc.mock.calls[1][1]).toMatchObject({ loadedFirstItemId: 'new-first', loadedLastItemId: 'new-last', limit: 25 });
  f.fetcher.dispose();
});
