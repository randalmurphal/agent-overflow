// `vi.waitFor` checks its callback at once and then every `interval` ms, 50
// by default. Most waits in this suite are for a promise chain or a short
// timer that settles a millisecond or two after the first check, so each
// such call took the whole 50 ms: 184 of the 238 tests in
// reviewPane.svelte.test.ts spent 50 ms each doing nothing.
//
// Under real timers, a `vi.waitFor` that names no interval checks again
// after 1, 2, 4, ... ms, up to 50 ms between checks: a condition that holds
// early is seen early, and one that takes long is polled as before. An
// explicit interval, and any call under fake timers (where each check
// advances the fake clock by the interval), keep Vitest's own behavior.
import { vi } from 'vitest';
import { realTimers as uncountedTimers } from './timerWaits';

type WaitFor = typeof vi.waitFor;

const DEFAULT_INTERVAL_MS = 50;
const DEFAULT_TIMEOUT_MS = 1000;
const FIRST_RECHECK_MS = 1;
const INSTALLED = Symbol.for('agent-overflow.test.waitForBackoff');

export interface WaitForTimers {
  setTimeout(callback: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

// Captured at import, before any test fakes them. The rechecks are not
// counted as sleep: what the condition waits for is (timerWaits.ts).
const { setTimeout: realSetTimeout, clearTimeout: realClearTimeout } = uncountedTimers();
const realTimers: WaitForTimers = {
  setTimeout: (callback, ms) => realSetTimeout(callback, ms),
  clearTimeout: (handle) => realClearTimeout(handle as ReturnType<typeof setTimeout>),
};

/** `original` with the backoff above; `timers` are the real ones but in this module's test. */
export function waitForWithBackoff(original: WaitFor, timers: WaitForTimers = realTimers): WaitFor {
  return ((callback, options) => {
    const opts = typeof options === 'number' ? { timeout: options } : (options ?? {});
    if (opts.interval !== undefined || vi.isFakeTimers()) return original(callback, options);
    const timeout = opts.timeout ?? DEFAULT_TIMEOUT_MS;
    const stackSource = new Error('STACK_TRACE_ERROR');
    return new Promise((resolve, reject) => {
      let lastError: unknown;
      let pending = false;
      let done = false;
      let delay = FIRST_RECHECK_MS;
      let recheck: unknown;
      const finish = () => {
        done = true;
        timers.clearTimeout(deadline);
        if (recheck !== undefined) timers.clearTimeout(recheck);
      };
      const check = (): void => {
        if (done) return;
        if (pending) {
          schedule();
          return;
        }
        try {
          const result = callback();
          if (result !== null && typeof result === 'object' && typeof (result as PromiseLike<unknown>).then === 'function') {
            pending = true;
            (result as PromiseLike<unknown>).then(
              (value) => {
                pending = false;
                if (done) return;
                finish();
                resolve(value as Awaited<ReturnType<typeof callback>>);
              },
              (error: unknown) => {
                pending = false;
                lastError = error;
              },
            );
          } else {
            finish();
            resolve(result as Awaited<ReturnType<typeof callback>>);
            return;
          }
        } catch (error) {
          lastError = error;
        }
        schedule();
      };
      const schedule = (): void => {
        if (done) return;
        recheck = timers.setTimeout(check, delay);
        delay = Math.min(delay * 2, DEFAULT_INTERVAL_MS);
      };
      const deadline = timers.setTimeout(() => {
        finish();
        if (lastError !== undefined) {
          reject(lastError);
          return;
        }
        const error = new Error('Timed out in waitFor!');
        if (stackSource.stack !== undefined) error.stack = stackSource.stack.replace(stackSource.message, error.message);
        reject(error);
      }, timeout);
      check();
    });
  }) as WaitFor;
}

/** Replaces `vi.waitFor` once per Vitest instance. */
export function installWaitForBackoff(): void {
  const holder = vi as unknown as Record<symbol, unknown>;
  if (holder[INSTALLED] !== undefined) return;
  holder[INSTALLED] = vi.waitFor;
  vi.waitFor = waitForWithBackoff(vi.waitFor);
}
