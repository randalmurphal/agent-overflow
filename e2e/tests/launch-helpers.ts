// Independent processes a spec needs before it starts, launched together so
// their boots overlap. A failed start closes the ones that did start.
interface Closable { close(): Promise<void> }

export async function startTogether<T extends readonly Closable[]>(
  ...starts: { [K in keyof T]: Promise<T[K]> }
): Promise<T> {
  const settled = await Promise.allSettled(starts);
  const failures: unknown[] = settled.flatMap((result) => result.status === 'rejected' ? [result.reason] : []);
  if (failures.length === 0) {
    return settled.map((result) => (result as PromiseFulfilledResult<Closable>).value) as unknown as T;
  }
  const started = settled.flatMap((result) => result.status === 'fulfilled' ? [result.value] : []);
  for (const cleanup of await Promise.allSettled(started.map((process) => process.close()))) {
    if (cleanup.status === 'rejected') failures.push(cleanup.reason);
  }
  throw failures.length === 1 ? failures[0] : new AggregateError(failures, 'Starting processes together failed');
}
