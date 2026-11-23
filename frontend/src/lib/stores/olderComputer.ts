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
 * computer through `same`; unpinned, a fallback takes its own route. An
 * `older` that cannot do the action throws `refusal`, with `withNote` for
 * anything the feature adds, so the person still reads which computer
 * refused and what to do, and a copied report keeps its code and cause.
 */
export async function orOlder<T>(
  call: () => PromiseLike<T>,
  older: (same: SameComputer, refusal: Error) => T | PromiseLike<T>,
): Promise<T> {
  // Read before `call` dispatches, which drains the pin.
  const pinned = getPinnedBackend();
  const same: SameComputer = (issue) => (pinned === null ? issue() : withBackendTarget(pinned, issue));
  try {
    return await call();
  } catch (err) {
    if (!(err instanceof Error) || !isMethodUnavailableError(err)) throw err;
    return older(same, err);
  }
}

/** `refusal`, its message followed by `note`. */
export function withNote(refusal: Error, note: string): Error {
  refusal.message = `${refusal.message} ${note}`;
  return refusal;
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
