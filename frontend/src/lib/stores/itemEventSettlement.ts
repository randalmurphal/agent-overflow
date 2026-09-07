// Replay delivery enqueues mutations; its wire completion does not mean those
// mutations have reached the panes. A fence waits only for work already queued,
// so continued live streaming cannot hold reconnect presentation indefinitely.
let queued = 0;
let settled = 0;
const waiters = new Set<{ through: number; resolve: () => void }>();

export function itemEventQueued(): void { queued += 1; }

export function itemEventsSettled(count: number): void {
  settled += count;
  for (const waiter of waiters) {
    if (waiter.through > settled) continue;
    waiters.delete(waiter);
    waiter.resolve();
  }
}

export function pendingItemEventsSettled(): Promise<void> | undefined {
  if (settled === queued) return undefined;
  return new Promise((resolve) => { waiters.add({ through: queued, resolve }); });
}

export function resetItemEventSettlement(): void {
  queued = settled = 0;
  for (const waiter of waiters) waiter.resolve();
  waiters.clear();
}
