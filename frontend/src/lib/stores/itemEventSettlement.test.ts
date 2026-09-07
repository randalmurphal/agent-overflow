import { afterEach, expect, it, vi } from 'vitest';
import { itemEventQueued, itemEventsSettled, pendingItemEventsSettled, resetItemEventSettlement } from './itemEventSettlement';

afterEach(resetItemEventSettlement);

it('waits through budgeted batches without waiting for later live events', async () => {
  itemEventQueued();
  itemEventQueued();
  const ready = vi.fn();
  const fence = pendingItemEventsSettled()!.then(ready);
  itemEventQueued();
  itemEventsSettled(1);
  await Promise.resolve();
  expect(ready).not.toHaveBeenCalled();
  itemEventsSettled(1);
  await fence;
  expect(ready).toHaveBeenCalledOnce();
  expect(pendingItemEventsSettled()).toBeDefined();
  itemEventsSettled(1);
  expect(pendingItemEventsSettled()).toBeUndefined();
});

it('releases a discarded queue without leaking waiters into another lifetime', async () => {
  itemEventQueued();
  const fence = pendingItemEventsSettled();
  resetItemEventSettlement();
  await fence;
  expect(pendingItemEventsSettled()).toBeUndefined();
  itemEventQueued();
  const next = pendingItemEventsSettled();
  expect(next).toBeDefined();
  itemEventsSettled(1);
  await next;
});
