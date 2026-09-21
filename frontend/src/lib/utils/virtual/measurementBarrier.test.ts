import { expect, it, vi } from 'vitest';
import { createMeasurementBarrier } from './measurementBarrier';

it('holds through retained and newly mounted measurements until geometry commits', async () => {
  const barrier = createMeasurementBarrier<string>();
  const done = vi.fn();
  const finished = barrier.request(['retained', 'removed'], new AbortController().signal).then(done);
  barrier.measured('retained');
  barrier.added('new');
  barrier.removed('removed');
  barrier.commit();
  await Promise.resolve();
  expect(done).not.toHaveBeenCalled();
  barrier.measured('new');
  await Promise.resolve();
  expect(done).not.toHaveBeenCalled();
  barrier.commit();
  await finished;
  expect(done).toHaveBeenCalledOnce();
  expect(barrier.active).toBe(false);
});

it('cancels an obsolete request without releasing a newer request', async () => {
  const barrier = createMeasurementBarrier<string>();
  const first = new AbortController();
  const second = new AbortController();
  const old = barrier.request(['obsolete'], first.signal);
  const done = vi.fn();
  const next = barrier.request(['row'], second.signal).then(done);
  first.abort();
  await old;
  barrier.commit();
  await Promise.resolve();
  expect(done).not.toHaveBeenCalled();
  barrier.measured('row');
  barrier.commit();
  await next;
  expect(done).toHaveBeenCalledOnce();
  expect(barrier.active).toBe(false);
});

it('drops cancelled state before reuse and releases requests on destruction', async () => {
  const barrier = createMeasurementBarrier<string>();
  const abort = new AbortController();
  const cancelled = barrier.request(['old'], abort.signal);
  abort.abort();
  await cancelled;
  const next = barrier.request(['new'], new AbortController().signal);
  barrier.measured('new');
  barrier.commit();
  await next;
  const destroyed = barrier.request(['unmounted'], new AbortController().signal);
  barrier.cancel();
  await destroyed;
  barrier.cancel();
  const empty = barrier.request([], new AbortController().signal);
  barrier.commit();
  await empty;
  await barrier.request(['ignored'], abort.signal);
});
