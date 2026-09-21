/** Completes explicit remeasurement requests after their geometry has committed. */
export function createMeasurementBarrier<T>() {
  const requests = new Set<{ pending: Set<T>; finish(): void }>();

  function measured(row: T): boolean {
    let changed = false;
    for (const request of requests) {
      if (request.pending.delete(row)) changed = true;
    }
    return changed;
  }

  return {
    get active(): boolean { return requests.size > 0; },
    request(rows: readonly T[], signal: AbortSignal): Promise<void> {
      if (signal.aborted) return Promise.resolve();
      return new Promise(resolve => {
        const request = {
          pending: new Set(rows),
          finish() {
            signal.removeEventListener('abort', request.finish);
            requests.delete(request);
            resolve();
          },
        };
        requests.add(request);
        signal.addEventListener('abort', request.finish, { once: true });
      });
    },
    added(row: T): void {
      for (const request of requests) request.pending.add(row);
    },
    measured,
    removed: measured,
    commit(): void {
      for (const request of [...requests]) {
        if (request.pending.size === 0) request.finish();
      }
    },
    cancel(): void {
      for (const request of [...requests]) request.finish();
    },
  };
}
