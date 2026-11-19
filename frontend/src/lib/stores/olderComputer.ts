// A computer on an older release answers a method it does not have with
// `method_not_found`. Against it the window behaves as that release does:
// an action the release has goes through the method it exposes, and a
// surface for a feature it lacks reads empty. bindings.ts decides it once
// per call with `orOlder`; the call's answer is the signal, so there is no
// version handshake.

import { getPinnedBackend, withBackendTarget } from '../transport/backends';
import { isMethodUnavailableError } from './transportStatus.svelte';

/** Issues a fallback call on the computer the first call went to. */
export type SameComputer = <T>(issue: () => T) => T;

/**
 * `call`'s answer, or `older`'s when the computer it reached does not have
 * the method. Called inside a `withBackendTarget`, `older` reaches the same
 * computer through `same`; unpinned, a fallback takes its own route.
 */
export async function orOlder<T>(
  call: () => PromiseLike<T>,
  older: (same: SameComputer) => T | PromiseLike<T>,
): Promise<T> {
  // Read before `call` dispatches, which drains the pin.
  const pinned = getPinnedBackend();
  const same: SameComputer = (issue) => (pinned === null ? issue() : withBackendTarget(pinned, issue));
  try {
    return await call();
  } catch (err) {
    if (!isMethodUnavailableError(err)) throw err;
    return older(same);
  }
}

/**
 * A delete of several threads, made one thread at a time on a computer
 * without DeleteThreads, that deleted some of them. `deleted` are gone;
 * each of `failures` stays, with the reason.
 */
export class ThreadsPartlyDeleted extends Error {
  constructor(
    readonly deleted: readonly string[],
    readonly failures: readonly { threadId: string; error: unknown }[],
  ) {
    super(`${failures.length} of ${deleted.length + failures.length} threads were not deleted`);
    this.name = 'ThreadsPartlyDeleted';
  }
}
