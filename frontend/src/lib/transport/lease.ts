// The one door for the client's foreground lifecycle
// (docs/specs/remote-access.md § "The phone client", "Lifecycle").
//
// **The signal is a NATIVE one** — the Capacitor app plugin's
// pause/resume, wired in `native/lifecycle.ts` and nowhere else. No
// browser or desktop client calls the setter at all, which is why
// `'active'` is both the resting state and what this module answers
// before anything has told it otherwise.
//
// **One door, whole client.** Backgrounding is one OS pausing one app, so it
// is stated to every attached backend at once and there is no per-connection,
// per-pane or per-machine spelling of it. `./backends.ts` owns "every
// attached backend, and every one attached afterwards"; this is the name the
// shell calls.
//
// **It is not document visibility.** A hidden tab, a minimised window, an
// off-screen pane and a blurred document all stay ACTIVE. Off-view work
// shedding is a rejected design in this codebase — a surface that stopped
// receiving renders wrongly the moment it is looked at again — and the one
// case this frame exists for is the one where the platform has stopped
// running the app at all.

import { setLeaseEverywhere } from './backends';
import type { LeaseState } from './frames';

export type { LeaseState } from './frames';

/**
 * Tell every attached backend whether this client is in the foreground.
 *
 * Idempotent per connection, and `'active'` is the resting state: calling
 * nothing is the same as calling `'active'`, which is why a desktop or
 * browser client never puts a lease byte on the wire.
 *
 * Safe to call while disconnected — each connection retains the state and
 * restates it after its next hello, beside its watch set.
 */
export function setClientLease(state: LeaseState): void {
  setLeaseEverywhere(state);
  if (state === current) return;
  current = state;
  for (const listener of [...listeners]) {
    try {
      listener(state);
    } catch (err) {
      console.warn('transport: a lease listener threw', err);
    }
  }
}

// What was last stated, so a client that has to KNOW rather than merely
// tell can. `'active'` is the resting state, so this reads correctly on
// every client that never calls the setter.
let current: LeaseState = 'active';

const listeners = new Set<(state: LeaseState) => void>();

/** Whether this client is in the foreground right now. */
export function clientLease(): LeaseState {
  return current;
}

/**
 * Watch the foreground state. Fires once immediately, then on each
 * change.
 *
 * The consumer is the shell's bundle sync: a multi-megabyte download is
 * work worth deferring while the OS has the app paused, which is the one
 * case where deferring is not off-view work shedding — the app is not
 * being looked at because it is not running.
 */
export function onClientLeaseChange(listener: (state: LeaseState) => void): () => void {
  listener(current);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Test seam: forget the state this module was told. */
export function __resetClientLeaseForTest(): void {
  current = 'active';
  listeners.clear();
}
