// What a pairing dialog holds about the one link it follows, and the one
// rule that folds each host answer into it. Both pairing dialogs
// (ComputerPairingWindow, PairDeviceModal) fold every answer through
// foldPairing, from a poll, an approval's follow-up read, or the approval's
// own result, and derive what they show from the held value. Answers can
// arrive in any order; the held value never moves backward in the host's
// own order.

import { TransportError } from '../../transport/wsClient';
import { pairingAlreadyAllowed } from './pairingTurns';

/**
 * The phases a link moves through, as tiers in the host's own order, lowest
 * first. An answer of a higher tier replaces the held one; a lower one
 * changes nothing.
 *
 * - waiting, verifying, ready: the link has not been decided.
 * - lapsed: the dialog's own deadline fallback, when it saw the deadline
 *   pass without a host answer saying how the link ended.
 * - ended: the host said the link ended without access (`ending` says why).
 *   It outranks the local fallback, which only stands in for it.
 * - confirmed: the device was allowed. It outranks a host-stated ending,
 *   which is safe because a canceled link can never be confirmed (the store
 *   settles a link once), and the host's status reads the link's durable
 *   row before the window's ending, so a `confirmed` answer is never older
 *   than a true cancel. An approval admitted before the deadline whose
 *   write lands after it does follow a deadline ending.
 * - accessEnded, deviceRemoved: the device was allowed and has since lost
 *   that access, its session ended or expired, or the owner removed the
 *   device. A revoked session never becomes usable again, so both outrank
 *   confirmed. They share a tier because the host moves between them in
 *   either order: Restore turns a removed device's pairing into an ended
 *   one (its session stays revoked), and removing a device whose session
 *   already ended turns an ended one into a removed one. Within the tier
 *   the first held stays; the dialog has stopped polling by then.
 */
export const PAIRING_TIERS = [
  ['waiting'],
  ['verifying'],
  ['ready'],
  ['lapsed'],
  ['ended'],
  ['confirmed'],
  ['accessEnded', 'deviceRemoved'],
] as const;

export type PairingPhase = (typeof PAIRING_TIERS)[number][number];

/** The first tier that ends the dialog's following of the link. */
const FIRST_END_TIER = 3;

/** One host answer, or the dialog's own deadline fallback (`lapsed`). */
export interface PairingAnswer {
  phase: PairingPhase;
  /** Why an `ended` link ended, in the host's words; '' when it names none. */
  ending?: string;
  linkId?: string;
  number?: string;
  label?: string;
  discoveryError?: string;
}

/** What a dialog holds for its link: the latest answer it accepted. */
export interface PairingHeld extends PairingAnswer {
  /** Whether a device ever redeemed the link (a `ready` answer was held). */
  redeemed: boolean;
}

/** A pairing that was allowed and has since lost its access. */
export type AccessLost = Extract<PairingPhase, 'accessEnded' | 'deviceRemoved'>;

/**
 * What a surface tells a person about a pairing that lost its access, with
 * the remedy that works: a removed device is refused every new pairing
 * until the owner restores or forgets it, in the Devices screen's words.
 * `subject` is the device, `retry` the control that pairs again.
 */
export function accessLostText(phase: AccessLost, subject: string, retry: string): string {
  switch (phase) {
    case 'accessEnded': return `${subject} was paired, and its access has since ended or expired. Choose ${retry} to pair it again.`;
    case 'deviceRemoved': return `${subject} was paired, and its access was removed in Devices. Choose Restore or Forget on it in Devices, then choose ${retry}.`;
  }
}

/**
 * The link's state an approval's result names (ConfirmDevicePairing):
 * already allowed is confirmed, and a refusal that names how an earlier
 * approval's access was lost is that phase. Null for a result that names no
 * state (a transport failure, or any other refusal), which only the host's
 * status can settle.
 */
export function approvalAnswer(err: unknown): PairingAnswer | null {
  if (pairingAlreadyAllowed(err)) return { phase: 'confirmed' };
  if (!(err instanceof TransportError)) return null;
  switch (err.code) {
    case 'pairing_access_ended': return { phase: 'accessEnded' };
    case 'pairing_device_removed': return { phase: 'deviceRemoved' };
    default: return null;
  }
}

/**
 * Whether a dialog shows an approval's named refusal itself, once the
 * refusal was folded: only while the dialog still follows the link and
 * holds the refusal's own state. A dismissed dialog shows nothing, and one
 * that holds another ending of the same tier shows that ending's remedy, so
 * the caller shows the refusal's message, with its remedy, as a toast.
 */
export function refusalShownBy(current: boolean, held: PairingHeld | null, refusal: PairingAnswer): boolean {
  return current && held?.phase === refusal.phase;
}

export function pairingTier(phase: PairingPhase): number {
  return PAIRING_TIERS.findIndex((tier) => (tier as readonly PairingPhase[]).includes(phase));
}

/** Whether the held phase ends the dialog's following of the link. */
export function pairingSettled(held: PairingHeld | null): boolean {
  return held !== null && pairingTier(held.phase) >= FIRST_END_TIER;
}

/**
 * Folds one answer into what the dialog holds. A higher tier replaces the
 * held answer, keeping the held fields the answer does not carry. Within
 * one tier, an undecided phase takes the newer fields, and an end keeps the
 * first one held. A lower tier changes nothing. Returns `held` itself when
 * nothing changed, so a caller can tell an accepted answer by identity.
 */
export function foldPairing(held: PairingHeld | null, answer: PairingAnswer): PairingHeld {
  if (held !== null) {
    const was = pairingTier(held.phase);
    const now = pairingTier(answer.phase);
    if (now < was || (now === was && now >= FIRST_END_TIER)) return held;
  }
  const next: PairingHeld = { ...(held ?? { redeemed: false }), phase: answer.phase };
  for (const key of ['ending', 'linkId', 'number', 'label', 'discoveryError'] as const) {
    if (answer[key] !== undefined) next[key] = answer[key];
  }
  next.redeemed = (held?.redeemed ?? false) || answer.phase === 'ready';
  return next;
}
