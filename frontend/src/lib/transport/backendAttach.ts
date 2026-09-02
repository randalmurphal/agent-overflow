// Attaching a second machine from a client that IS the client.
//
// On the desktop, attaching is a host RPC: the local Go process redeems
// the pairing link, holds the profile, and proxies that backend to its own
// page at `/ws/backend/<id>` (`stores/systems.svelte.ts`, `AddBackend`).
// A phone has no local process to hold anything, so the redemption is the
// same exchange this client already performs for its OWN backend — one
// more session slot, one more endpoint, one more descriptor — and that is
// the whole difference between the two realizations (spec §10).
//
// Everything below is composed from parts that already exist and nothing
// here is a second copy of any of them: `parsePairingFragment` reads the
// link, `redeemPairing` spends it against a per-backend slot,
// `homeEndpoint` remembers where that backend is, and
// `syncAttachedBackends` opens the socket once the owner confirms.

import { HOME_BACKEND } from './backendKey';
import { syncAttachedBackends } from './backends';
import {
  parsePairingFragment,
  probeActivation,
  redeemPairing,
  type PairingPayload,
} from './deviceSession';
import { storeBackendEndpoint } from './homeEndpoint';

export interface AttachedPairing {
  /** The registry id this machine will be keyed by. */
  id: string;
  /** What to call it until its manifest says otherwise. */
  name: string;
  /** The six digits the owner compares on the machine being attached. */
  verificationNumber: string;
}

/**
 * Read a pairing link, whatever it was carried in.
 *
 * A scanned QR and a pasted URL are the same string, and the payload is
 * in the fragment either way — so this takes the whole thing and finds
 * the fragment rather than making the caller do it. Throws the sentence
 * `parsePairingFragment` already writes for a link this build cannot
 * redeem, because that sentence is meant for a person.
 */
export function payloadFromLink(link: string): PairingPayload {
  const trimmed = link.trim();
  const at = trimmed.indexOf('#pair=');
  const payload = at === -1 ? null : parsePairingFragment(trimmed.slice(at));
  if (payload === null) throw new Error('That is not an Agent Overflow pairing link.');
  return payload;
}

/**
 * Redeem a pairing link into a NEW session slot on this client.
 *
 * The registry id is the payload's `backendId` — the machine's own UUID,
 * which is stable, is what its events are stamped with, and is what a
 * second link from the same machine would name again, so re-attaching
 * adopts the existing slot rather than opening a second one.
 *
 * The socket is NOT opened here. A redeemed pairing is not an admitted
 * one until the owner confirms it on the machine being attached, and a
 * client that dialed first would spend one doomed upgrade per backoff
 * step waiting for a human. `awaitAttachedActivation` is the other half.
 */
export async function attachBackendFromLink(link: string): Promise<AttachedPairing> {
  const payload = payloadFromLink(link);
  const id = payload.backendId;
  if (id === '' || id === HOME_BACKEND || id.includes(' ')) {
    // A registry id is the prefix of every path-keyed composite key, and
    // the empty string is the page's own backend. Refused at the door,
    // the same rule `manifestBackends.readBackendDescriptors` applies.
    throw new Error('That pairing link does not name a machine this app can attach.');
  }
  const name = payload.backendName || new URL(payload.endpoint).host;
  // Stored before the credential, so a session can never outlive the
  // knowledge of where to present it.
  storeBackendEndpoint(id, payload.endpoint);
  const outcome = await redeemPairing(payload, name, fetch, id);
  return { id, name, verificationNumber: outcome.verificationNumber };
}

/**
 * Poll until the owner confirms the pairing on that machine, then attach.
 * Answers false when the window lapsed without a confirmation, which is
 * the caller's cue to say so rather than to keep asking.
 */
export async function awaitAttachedActivation(
  id: string,
  intervalMs = 3_000,
  deadlineMs = 10 * 60_000,
): Promise<boolean> {
  const deadline = Date.now() + deadlineMs;
  for (;;) {
    if (await probeActivation(fetch, id)) {
      // The descriptor is rebuilt from the stored endpoint map, so this
      // is the same sync a shell boot performs — one code path for "these
      // are the machines I am attached to".
      syncAttachedBackends();
      return true;
    }
    if (Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}
