import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { BackendKey } from '../transport/backendKey';
import { bumpUsageRefresh } from './usageRefresh.svelte';

const recovery = vi.hoisted(() => ({ listeners: new Set<(backend: string, phase: 'start' | 'complete' | 'cancel') => void>() }));
vi.mock('./transportRecovery', async (importOriginal) => ({
  ...await importOriginal<typeof import('./transportRecovery')>(),
  onBackendRecovery(listener: (backend: string, phase: 'start' | 'complete' | 'cancel') => void) {
    recovery.listeners.add(listener); return () => recovery.listeners.delete(listener);
  },
}));
import { flushSync } from 'svelte';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { noteThread, noteProject } from '../transport/entityIndex';
import { takePinnedBackend } from '../transport/backends';
import { UsageBucket, UsageQuery } from './bindings';
import { createUsageStats } from './usageQuery.svelte';
import { setTelemetrySelection, resetTelemetryForTest } from './telemetryComputers.svelte';

let release = () => {};
beforeEach(resetTelemetryForTest);
afterEach(() => { release(); vi.useRealTimers(); resetTelemetryForTest(); resetStagedBackends(); });
async function flush() { flushSync(); await Promise.resolve(); await Promise.resolve(); flushSync(); }

it('aggregates only selected online hosts and exposes offline/failed hosts as partial results', async () => {
  const gpu = stageBackend({ id: 'gpu', name: 'GPU' });
  const targets: unknown[] = [];
  setBindingMock('GetUsageStats', async () => { const key = takePinnedBackend(); targets.push(key); return [new UsageBucket({ bucket: 'claude', outputTokens: key === 'gpu' ? 200 : 100 })]; });
  let stats!: ReturnType<typeof createUsageStats>;
  release = $effect.root(() => { stats = createUsageStats(() => new UsageQuery({ groupBy: 'provider' })); });
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(300);
  expect(targets).toEqual(['', 'gpu']);
  gpu.setStatus('reconnecting');
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(100);
  expect(stats.unavailable).toEqual(['GPU']);
  setTelemetrySelection('usage', ['gpu']);
  await flush();
  expect(stats.buckets).toEqual([]);
  setBindingMock('GetUsageStats', async () => { throw new Error('refused'); });
  gpu.setStatus('connected');
  await flush();
  expect(stats.unavailable).toEqual(['GPU']);
});

it('fences a slow old selection and leaves per-thread queries independent of global filters', async () => {
  stageBackend({ id: 'gpu', name: 'GPU' });
  setTelemetrySelection('usage', ['gpu']);
  let finish!: (buckets: UsageBucket[]) => void;
  setBindingMock('GetUsageStats', () => new Promise((resolve) => { finish = resolve; }));
  let stats!: ReturnType<typeof createUsageStats>;
  release = $effect.root(() => { stats = createUsageStats(() => new UsageQuery({ groupBy: 'provider' })); });
  await flush();
  setBindingMock('GetUsageStats', async () => [new UsageBucket({ bucket: 'claude', outputTokens: 10 })]);
  setTelemetrySelection('usage', ['']);
  await flush();
  finish([new UsageBucket({ bucket: 'claude', outputTokens: 99 })]);
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(10);
  release();
  noteThread('specific-thread', 'gpu');
  const calls = setBindingMock('GetUsageStats', async () => {
    expect(takePinnedBackend()).toBe('gpu');
    return [new UsageBucket({ outputTokens: 1 })];
  });
  release = $effect.root(() => { stats = createUsageStats(() => new UsageQuery({ threadId: 'specific-thread' })); });
  await flush();
  setTelemetrySelection('usage', ['gpu']);
  await flush();
  expect(calls).toHaveBeenCalledTimes(1);
  expect(stats.buckets?.[0].outputTokens).toBe(1);
});

it('queries a known project only on its owner and preserves historical project lookup', async () => {
  stageBackend({ id: 'gpu', name: 'GPU' });
  noteProject('gpu-project', 'gpu');
  const targets: unknown[] = [];
  setBindingMock('GetUsageStats', async () => { targets.push(takePinnedBackend()); return []; });
  let projectId = $state('gpu-project');
  release = $effect.root(() => { createUsageStats(() => new UsageQuery({ projectId })); });
  await flush();
  expect(targets).toEqual(['gpu']);
  targets.length = 0;
  projectId = 'deleted-project';
  await flush();
  expect(targets).toEqual(['', 'gpu']);
});


it('serializes sustained progress without discarding each slow answer and detaches cleanly', async () => {
  vi.useFakeTimers();
  noteThread('live-thread', '' as BackendKey);
  const pending: Array<(rows: UsageBucket[]) => void> = [];
  const calls = setBindingMock('GetUsageStats', () => new Promise((resolve) => pending.push(resolve)));
  let stats!: ReturnType<typeof createUsageStats>;
  release = $effect.root(() => { stats = createUsageStats(() => new UsageQuery({ threadId: 'live-thread' })); });
  await flush();
  for (let i = 0; i < 10; i++) { bumpUsageRefresh('live-thread'); await vi.advanceTimersByTimeAsync(100); }
  expect(calls).toHaveBeenCalledTimes(1);
  pending.shift()!([new UsageBucket({ outputTokens: 10 })]);
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(10);
  await vi.advanceTimersByTimeAsync(500);
  expect(calls).toHaveBeenCalledTimes(2);
  pending.shift()!([new UsageBucket({ outputTokens: 20 })]);
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(20);
  release(); release = () => {};
  bumpUsageRefresh('live-thread');
  await vi.advanceTimersByTimeAsync(1000);
  expect(calls).toHaveBeenCalledTimes(2);
});

it('refreshes on its owner recovery and fences an older response', async () => {
  stageBackend({ id: 'gpu', name: 'GPU' });
  noteThread('recovering-thread', 'gpu');
  let finish!: (rows: UsageBucket[]) => void;
  setBindingMock('GetUsageStats', () => new Promise((resolve) => { finish = resolve; }));
  let stats!: ReturnType<typeof createUsageStats>;
  release = $effect.root(() => { stats = createUsageStats(() => new UsageQuery({ threadId: 'recovering-thread' })); });
  await flush();
  const oldFinish = finish;
  const calls = setBindingMock('GetUsageStats', async () => [new UsageBucket({ outputTokens: 30 })]);
  for (const fn of recovery.listeners) fn('', 'start');
  await flush();
  expect(calls).not.toHaveBeenCalled();
  for (const fn of recovery.listeners) fn('gpu', 'start');
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(30);
  oldFinish([new UsageBucket({ outputTokens: 5 })]);
  await flush();
  expect(stats.buckets?.[0].outputTokens).toBe(30);
});
