import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createActivityRunMemberFetch } from './activityRunMemberFetch';
import { foldPageStub } from './activityRunStubs';
import type { ActivityRunRecords } from './activityRunStubs';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
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
beforeEach(() => { resetBindingMocks(); vi.useFakeTimers(); });
afterEach(() => vi.useRealTimers());

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
  const rpc = vi.fn(async () => { throw stale(); });
  setBindingMock('ListActivityRunMembers', rpc);
  const first = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  const second = f.fetcher.fetch('second', { direction: 'before', limit: 5 });
  await vi.advanceTimersByTimeAsync(1);
  expect(f.reloadWindow).toHaveBeenCalledOnce();
  await f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  expect(rpc).toHaveBeenCalledTimes(2);
  recovering.resolve();
  await Promise.all([first, second]);
  expect(f.reportFailure.mock.calls.every(call => call[2] === true)).toBe(true);
  f.fetcher.dispose();
});

it('discards a members answer superseded by a new page description', async () => {
  const f = fixture();
  const result = deferred<unknown>();
  setBindingMock('ListActivityRunMembers', () => result.promise);
  const pending = f.fetcher.fetch('run', { direction: 'before', limit: 5 });
  foldPageStub(f.records, { ...f.stub }, null);
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
  f.record.stub = { ...f.stub };
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
