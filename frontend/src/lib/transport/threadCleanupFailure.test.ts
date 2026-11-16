import { describe, expect, it } from 'vitest';
import { threadCleanupFailure } from './threadCleanupFailure';
import { TransportError } from './wsClient';

describe('threadCleanupFailure', () => {
  it('is null for a call that finished cleanly', async () => {
    expect(await threadCleanupFailure(Promise.resolve())).toBeNull();
  });

  it('returns the error of a removal that happened but did not finish its cleanup', async () => {
    const err = new TransportError('thread_cleanup_failed', 'The thread was archived, but its browser pages could not all be closed.');
    expect(await threadCleanupFailure(Promise.reject(err))).toBe(err);
  });

  it('rejects when the removal did not happen', async () => {
    const err = new TransportError('not_found', 'The requested item no longer exists.');
    await expect(threadCleanupFailure(Promise.reject(err))).rejects.toBe(err);
  });
});
