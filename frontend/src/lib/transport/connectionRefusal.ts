import type { TerminalTransportStatus } from './wsClient';

/**
 * The third module in the refusal vocabulary, and the one whose refusals
 * are the CONNECTION's rather than a call's. ./authReason.ts phrases a
 * credential refusal that came off the wire with a code; ./scopeRefusal.ts
 * phrases an authorization refusal that came off the wire with a scope.
 * A terminal transport state carries neither: the `/ws` upgrade and the
 * manifest both answer an unfingerprintable 404 by design, so the client
 * decides which condition it is in and this module says what to do about
 * it.
 *
 * Kept apart from the other two for the reason they are kept apart from
 * each other: the remedies differ in kind. One asks for a fresh page
 * load, the other asks for a pairing link, and a surface branching on
 * both would offer the wrong one half the time.
 *
 * Exhaustive over `TerminalTransportStatus` by construction — a record,
 * not a switch — so adding a terminal state fails the type check here
 * rather than silently rendering nothing on the surface that shows it.
 */

/**
 * One sentence per terminal state: what happened, then what to do. No
 * mechanism (`frontend/AGENTS.md`: no visible in-app explanatory text for
 * internal mechanics) — the person cannot act on peer locality or on
 * which credential the upgrade reads, only on reopening a link or pairing
 * this device.
 */
const TERMINAL_MESSAGES: Record<TerminalTransportStatus, string> = {
  unauthorized: 'The backend restarted. Reopen the share link to reconnect.',
  'pairing-required': 'Pair this device to use this backend. Open a pairing link on this device.',
};

/** What to show while the transport sits in a terminal state. */
export function connectionRefusalMessage(status: TerminalTransportStatus): string {
  return TERMINAL_MESSAGES[status];
}

/**
 * Whether a status is one the automatic ladder has stopped on, narrowing
 * to the type `connectionRefusalMessage` takes. A surface reads this
 * rather than listing the members itself, which is what keeps the set in
 * one place.
 */
export function isTerminalConnectionStatus(status: string): status is TerminalTransportStatus {
  return Object.hasOwn(TERMINAL_MESSAGES, status);
}
