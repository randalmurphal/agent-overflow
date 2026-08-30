// Transition tests for stateful APIs.
//
// The leaks the 2026-08 perf session found by hand were all the same
// omission: a test drove an API on, asserted the on-state, and stopped.
// The bugs lived in the SECOND lap — a re-register that duplicated a
// sink, a toggle that kept a stale checkpoint, a cache that carried the
// previous mode. `expectCleanTransitions` drives the laps a stateful
// door has to survive and asserts it returns to the same resting state
// every time:
//
//   1. on -> off
//   2. on -> off -> on -> off        re-engagement is a fresh engagement
//   3. on -> off -> off              teardown is idempotent
//   4. on -> onAgain -> off          a second `on` while engaged
//   5. on -> inFlight -> off         teardown lands mid-flight
//
// `read()` is the whole point: name the state a cycle must not leak and
// the helper compares it after every lap. Laps 4 and 5 run only when the
// subject describes them, because "what a second `on` does" is
// API-specific (reject, share, re-lex) and only the subject knows.

import { expect } from 'vitest';

/** Whatever the API hands back from `on`, if anything. */
export type TransitionTeardown = (() => void) | void;

export interface TransitionSubject<TState> {
  /** Engage the API. Returns its teardown when it hands one back. */
  on(): TransitionTeardown;
  /**
   * Disengage, given whatever `on` returned. Must be a no-op the second
   * time — lap 3 calls it twice.
   */
  off(handle: TransitionTeardown): void;
  /**
   * Everything a full cycle must not leak, read fresh and compared with
   * `toEqual`. Keep it observable: behavioral probes beat private fields.
   */
  read(): TState;
  /**
   * Asserted after every `on`. Without it a subject whose `on` is
   * accidentally a no-op passes every lap trivially.
   */
  whileOn?(): void;
  /** What a SECOND `on` must do while already engaged (lap 4). */
  onAgain?(handle: TransitionTeardown): void;
  /**
   * Work performed while engaged, so lap 5 has something genuinely in
   * flight when teardown lands.
   */
  inFlight?(): void;
}

export function expectCleanTransitions<TState>(
  name: string,
  subject: TransitionSubject<TState>,
): void {
  const rest = subject.read();

  const settled = (lap: string): void => {
    expect(
      subject.read(),
      `${name}: "${lap}" did not return to the resting state`,
    ).toEqual(rest);
  };

  const engage = (): TransitionTeardown => {
    const handle = subject.on();
    subject.whileOn?.();
    return handle;
  };

  subject.off(engage());
  settled('on -> off');

  subject.off(engage());
  subject.off(engage());
  settled('on -> off -> on -> off');

  const idempotent = engage();
  subject.off(idempotent);
  subject.off(idempotent);
  settled('on -> off -> off');

  if (subject.onAgain) {
    const engaged = engage();
    subject.onAgain(engaged);
    subject.off(engaged);
    settled('on -> on -> off');
  }

  if (subject.inFlight) {
    const engaged = engage();
    subject.inFlight();
    subject.off(engaged);
    settled('on -> in flight -> off');
  }
}
