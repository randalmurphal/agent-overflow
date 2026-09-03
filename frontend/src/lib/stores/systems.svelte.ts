// The other machines this installation is attached to, as Settings →
// Systems manages them.
//
// One store owns the four profile RPCs (`ListBackends`, `AddBackend`,
// `RemoveBackend`, `RenameBackend`) and the `backend:attach` reaction,
// because they share one fact: the list the local backend holds. A section
// calling `AddBackend` itself would show a verification number the
// confirmation event has no way to retire, and a removal made anywhere
// else would leave the transport registry holding a socket to a profile
// that no longer exists.
//
// All four are `host`-scoped and `home`-routed (internal/app/app_backends.go):
// they act on THIS machine's profile directory, never on an attached one.
// A `--connect` window and every paired device are told so rather than
// shown a control that cannot work, and the passive list load asks
// `hasScope('host')` before it fires (stores/AGENTS.md, the passive-load
// rule).
//
// Pairing is two RPCs apart in time: `AddBackend` returns the verification
// number at once and the owner of the far machine confirms it minutes
// later, which arrives as one `backend:attach` frame. On success the
// registry learns the new door immediately (`publishAttachedBackend`)
// rather than at the next manifest fetch, so the machine picker and the
// unified sidebar fill without a reload.

import {
  AddBackend,
  ListBackends,
  RemoveBackend,
  RenameBackend,
  type AttachedBackend,
} from './bindings';
import { hasScope } from '../transport/scopes';
import { detachBackend } from '../transport/backends';
import { purgeClientState } from '../transport/clientPurge';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import {
  descriptorForAttachedId,
  publishAttachedBackend,
  publishDetachedBackend,
} from '../transport/manifestBackends';

/** What the `backend:attach` channel carries (internal/app BackendAttachOutcome). */
export interface BackendAttachEvent {
  id: string;
  attached: boolean;
  error?: string;
}

/** A pairing this page started and is waiting on. */
export interface PendingAttachment {
  id: string;
  name: string;
  endpoint: string;
  verificationNumber: string;
}

let systems = $state.raw<readonly AttachedBackend[]>([]);
let loaded = $state(false);
let pending = $state.raw<readonly PendingAttachment[]>([]);
let loadInFlight: Promise<void> | null = null;

/** Every attached machine, as the last load answered. */
export function getSystems(): readonly AttachedBackend[] {
  return systems;
}

/** Whether a load has completed since boot (or the last reset). */
export function systemsLoaded(): boolean {
  return loaded;
}

/** Pairings started from this page that the far owner has not confirmed. */
export function getPendingAttachments(): readonly PendingAttachment[] {
  return pending;
}

/**
 * Load the list. A passive caller (a section mount) gets its empty answer
 * without an RPC when this session cannot hold `host`.
 */
export function loadSystems(): Promise<void> {
  if (!hasScope('host')) return Promise.resolve();
  if (loadInFlight) return loadInFlight;
  loadInFlight = (async () => {
    try {
      systems = await ListBackends();
      loaded = true;
    } finally {
      loadInFlight = null;
    }
  })();
  return loadInFlight;
}

/**
 * Start a pairing. Resolves with the verification number to show; the
 * outcome arrives later on `backend:attach`.
 */
export async function addSystem(pairingLink: string): Promise<PendingAttachment> {
  const attachment = await AddBackend(pairingLink);
  const row: PendingAttachment = {
    id: attachment.id,
    name: attachment.name,
    endpoint: attachment.endpoint,
    verificationNumber: attachment.verificationNumber,
  };
  pending = [...pending.filter((p) => p.id !== row.id), row];
  return row;
}

export async function removeSystem(id: string): Promise<void> {
  await RemoveBackend(id);
  systems = systems.filter((s) => s.id !== id);
  pending = pending.filter((p) => p.id !== id);
  // Both: the manifest list forgets it so the next sync does not re-open
  // it, and the socket closes now rather than at that sync.
  publishDetachedBackend(id);
  detachBackend(id);
  // And what that machine stored on this device. The desktop's removal
  // door is this one rather than `detachAttachedBackend` (the profile
  // lives in the local Go process, not in a client session slot), so the
  // purge has to be stated here too or a detach from Settings leaves the
  // replica a detach from a phone removes.
  purgeClientState(id);
}

export async function renameSystem(id: string, nickname: string): Promise<void> {
  await RenameBackend(id, nickname);
  systems = systems.map((s) => (s.id === id ? { ...s, nickname } : s));
}

/** The name a person sees for a system: their nickname, else the machine's own. */
export function systemLabel(system: Pick<AttachedBackend, 'name' | 'nickname' | 'id'>): string {
  return system.nickname || system.name || system.id;
}

/**
 * `backend:attach` — how one pairing ended. Called by events.ts. Retires
 * the pending row either way; on success the transport learns the door now
 * and the list is re-read so the new row carries what pairing wrote.
 *
 * **Only home's frame counts, and `origin` is how that is decided here
 * rather than trusted upstream.** The event hub subscribes every attached
 * backend (`transport/backends.ts`'s `subscribeEveryBackend`), so this
 * handler is reachable from a machine that is not the one whose profile
 * directory these four RPCs act on. The Go channel is loopback-only and
 * host-scoped, which excludes a network peer but not a backend that is
 * itself on this box, and the descriptor built below names THIS machine's
 * proxy path (`/ws/backend/<id>`), so another backend's frame would
 * register a door home does not serve and leave a socket that can never
 * open. Answers null when the frame is not home's, which the caller reads
 * as "say nothing".
 */
export function applyBackendAttach(
  evt: BackendAttachEvent,
  origin: BackendKey = HOME_BACKEND,
): { name: string; error: string } | null {
  if (origin !== HOME_BACKEND) return null;
  const row = pending.find((p) => p.id === evt.id);
  pending = pending.filter((p) => p.id !== evt.id);
  const name = row?.name ?? evt.id;
  if (evt.attached) {
    publishAttachedBackend(descriptorForAttachedId(evt.id, name));
    if (hasScope('host')) void loadSystems();
    return { name, error: '' };
  }
  return { name, error: evt.error || 'the pairing was not confirmed' };
}

/** Test seam. */
export function __resetSystemsForTest(): void {
  systems = [];
  loaded = false;
  pending = [];
  loadInFlight = null;
}
