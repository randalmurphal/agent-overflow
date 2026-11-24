// What a pairing dialog holds about the one link it follows, and the one
// rule that folds each host answer into it. Both pairing dialogs
// (ComputerPairingWindow, PairDeviceModal) fold every answer through
// foldPairing, from a poll, an approval's follow-up read, or the approval's
// own result, and derive what they show from the held value. Answers can
// arrive in any order; the held value never moves backward in the host's
// own order.

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
 * - accessEnded: the device was allowed and has since lost that access. A
 *   revoked session never becomes usable again, so it outranks confirmed.
 */
export const PAIRING_TIERS = [
  ['waiting'],
  ['verifying'],
  ['ready'],
  ['lapsed'],
  ['ended'],
  ['confirmed'],
  ['accessEnded'],
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
