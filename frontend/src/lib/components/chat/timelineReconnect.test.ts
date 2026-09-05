import { afterEach, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/types';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';

const events = vi.hoisted(() => new Set<(backend: string, phase: 'start' | 'complete' | 'cancel') => void>());
vi.mock('../../stores/transportRecovery', () => ({
  onBackendRecovery(fn: (backend: string, phase: 'start' | 'complete' | 'cancel') => void) {
    events.add(fn); return () => events.delete(fn);
  },
}));
vi.mock('../../stores/attachedBackends.svelte', () => ({ threadMachine: () => 'remote' }));

import { installTimelineReconnect } from './timelineReconnect';

const cleanups: Array<() => void> = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
  vi.unstubAllGlobals();
});

function setup() {
  const frames = new Map<number, FrameRequestCallback>();
  let nextFrame = 0;
  vi.stubGlobal('requestAnimationFrame', (fn: FrameRequestCallback) => {
    frames.set(++nextFrame, fn); return nextFrame;
  });
  vi.stubGlobal('cancelAnimationFrame', (id: number) => { frames.delete(id); });
  const recovery = { finish: vi.fn(), cancel: vi.fn() };
  const beginReconnectRecovery = vi.fn(() => recovery);
  const snap = vi.fn();
  const revalidate = vi.fn();
  const cleanup = installTimelineReconnect({
    pane: { threadId: 'thread', snapSmoothersToReceived: snap } as unknown as ThreadPane,
    stick: { beginReconnectRecovery } as unknown as UseStickToBottomController,
    getList: () => ({ revalidate }) as TimelineVirtualizerHandle,
  });
  cleanups.push(cleanup);
  return { frames, recovery, beginReconnectRecovery, snap, revalidate, cleanup };
}
function emit(phase: 'start' | 'complete' | 'cancel', backend = 'remote') {
  for (const fn of events) fn(backend, phase);
}
async function flush() {
  // tick resolves after Svelte's microtask, then runs the recovery callback.
  for (let i = 0; i < 4; i++) await Promise.resolve();
}

it('drains received text and measures after layout only for the owning backend', async () => {
  const f = setup();
  emit('start', 'other');
  expect(f.beginReconnectRecovery).not.toHaveBeenCalled();
  emit('start');
  emit('complete');
  expect(f.snap).toHaveBeenCalledOnce();
  expect(f.recovery.finish).not.toHaveBeenCalled();
  await flush();
  for (const fn of f.frames.values()) fn(0);
  expect(f.revalidate).toHaveBeenCalledOnce();
  await flush();
  expect(f.recovery.finish).toHaveBeenCalledOnce();
});

it.each(['disconnect', 'unmount'] as const)('cancels queued layout on %s', async (cause) => {
  const f = setup();
  emit('start');
  emit('complete');
  await flush();
  expect(f.frames.size).toBe(1);
  if (cause === 'disconnect') emit('cancel');
  else f.cleanup();
  expect(f.frames.size).toBe(0);
  await flush();
  expect(f.recovery.cancel).toHaveBeenCalledOnce();
  expect(f.recovery.finish).not.toHaveBeenCalled();
});

it('does not finish an old recovery when another reconnect starts after measurement', async () => {
  const f = setup();
  emit('start');
  emit('complete');
  await flush();
  for (const fn of f.frames.values()) fn(0);
  const next = { finish: vi.fn(), cancel: vi.fn() };
  f.beginReconnectRecovery.mockReturnValue(next);
  emit('start');
  await flush();
  expect(f.recovery.cancel).toHaveBeenCalledOnce();
  expect(f.recovery.finish).not.toHaveBeenCalled();
  expect(next.finish).not.toHaveBeenCalled();
});
