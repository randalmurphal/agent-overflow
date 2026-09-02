// The one door for the client's foreground lifecycle
// (docs/specs/remote-access.md § "The phone client", "Lifecycle").
//
// **Nothing in the SPA calls this yet, and that is correct.** The signal it
// carries is a NATIVE one — the Capacitor app plugin's pause/resume — and
// the shell that produces it lands in wave 6f-c. This module exists ahead of
// it so the wire, the fan-out and the door all ship together: a transport
// capability whose only caller is added later is a capability somebody wires
// by reaching past the seam.
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
}
