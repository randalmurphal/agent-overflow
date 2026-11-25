/**
 * `load` run once and shared by every later call, for a module loaded on
 * its first use through one stable dynamic import. A load that fails is
 * forgotten, so the next call tries again; the caller says why it failed.
 */
export function retryingLoad<T>(load: () => Promise<T>): () => Promise<T> {
  let pending: Promise<T> | null = null;
  return () => {
    pending ??= load().catch((err: unknown) => {
      pending = null;
      throw err;
    });
    return pending;
  };
}
