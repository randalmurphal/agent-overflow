import { TransportError } from './wsClient';

/**
 * Await an ArchiveThread or DeleteThread call.
 *
 * `thread_cleanup_failed` (threadtools.CodeCleanupFailed) is the backend's
 * answer when the thread was archived or deleted and the cleanup after it did
 * not finish: something the thread started may still be running or open. The
 * thread is gone from every list all the same, so the caller drops it as it
 * would on success and shows the error, whose message says what happened.
 *
 * Resolves to null on success and to that error on a cleanup failure;
 * rejects when the archive or delete did not happen.
 */
export async function threadCleanupFailure(call: Promise<unknown>): Promise<TransportError | null> {
  return call.then(() => null, (err: unknown) => {
    if (err instanceof TransportError && err.code === 'thread_cleanup_failed') return err;
    throw err;
  });
}
