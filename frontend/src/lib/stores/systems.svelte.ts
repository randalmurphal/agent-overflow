// The other machines this installation is attached to, as Settings →
// Remote access → Connect to a computer manages them.
//
// One store owns the three profile RPCs (`ListBackends`, `AddBackend`,
// `RemoveBackend`) and the `backend:attach` reaction,
// because they share one fact: the list the local backend holds. A section
// calling `AddBackend` itself would show a verification number the
// confirmation event has no way to retire, and a removal made anywhere
// else would leave the transport registry holding a socket to a profile
// that no longer exists.
//
// THE REGISTRY IS THE LIST. This store keeps no mirror of the rows: every
// `ListBackends` answer is published wholesale into
// `publishManifestBackends`, which is the transport registry's source, and
// the section renders the registry's entries — the same list the machine
// picker and the sidebar read, on both realizations. What the descriptor
// has no field for (`lastReachedMs` and the two sync errors) lives in the
// side map below, keyed by the same id.
//
// All three RPCs are `host`-scoped and `home`-routed
// (internal/app/app_backends.go): they act on THIS machine's profile
// directory, never on an attached one. A standalone frontend owns these
// operations locally. A legacy relay or paired browser cannot administer
// its upstream profiles; the passive load asks `hasScope('host')` before
// it fires (stores/AGENTS.md, the passive-load rule).
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
  type AttachedBackend,
} from './bindings';
import { hasScope } from '../transport/scopes';
import { backendById, detachBackend } from '../transport/backends';
import { purgeClientState } from '../transport/clientPurge';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import {
  descriptorForAttachedId,
  publishAttachedBackend,
  publishDetachedBackend,
  publishManifestBackends,
  manifestBackendDescriptors,
} from '../transport/manifestBackends';

/** What the `backend:attach` channel carries (internal/app BackendAttachOutcome). */
export interface BackendAttachEvent {
  id: string;
  attached: boolean;
  error?: string;
}

/**
 * What the `backend:set-changed` channel carries
 * (internal/attachedbackends SetChange, emitted by both the desktop with
 * a backend and the frontend-only one): every mutation of the set that is
 * not a pairing ceremony ending. The action union is pinned against the
 * Go constants by internal/app's backend_set_change_vocabulary_test.go.
 */
export interface BackendSetChangeEvent {
  action: 'removed' | 'renamed' | 'device-name-sync' | 'membership';
  id: string;
  nickname?: string;
  /**
   * Who ended a `removed` machine's pairing. Absent when this installation
   * forgot it — the page asked, so there is nothing to explain — and
   * `ended-by-computer` when the far machine refused this device's renewal
   * with a verdict that ends the session, which is the one removal nobody
   * here asked for and the one that gets a toast.
   */
  reason?: 'ended-by-computer';
}

/** A pairing this page started and is waiting on. */
export interface PendingAttachment {
  id: string;
  name: string;
  endpoint: string;
  verificationNumber: string;
}

/** The per-machine facts `ListBackends` carries that the registry's
 *  descriptor has no field for. */
export interface SystemStatus {
  lastReachedMs: number;
  deviceNameSyncError: string;
  ownDeviceSyncError: string;
}

let statuses = $state.raw<ReadonlyMap<string, SystemStatus>>(new Map());
let loaded = $state(false);
let pending = $state.raw<readonly PendingAttachment[]>([]);
let loadInFlight: Promise<void> | null = null;
let revision = 0;

/** The status fields the last load recorded for one machine. */
export function systemStatus(id: string): SystemStatus | undefined {
  return statuses.get(id);
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
      // A response started before a removal/rename cannot bring its old
      // profile back: the guard covers the PUBLISH, which is the list's
      // one owner. Re-read the authoritative set when a mutation raced it.
      for (;;) {
        const before = revision;
        const rows = await ListBackends();
        if (before !== revision) continue;
        publishSystems(rows);
        break;
      }
      loaded = true;
    } finally {
      loadInFlight = null;
    }
  })();
  return loadInFlight;
}

/**
 * Publish one authoritative answer: the descriptors into the registry's
 * source, the rest into the side map. Synchronous on purpose — the
 * revision guard above covers everything here, so an older in-flight read
 * can never erase a repaired set.
 */
function publishSystems(rows: readonly AttachedBackend[]): void {
  const ids = new Set(rows.map((row) => row.id));
  for (const removed of manifestBackendDescriptors()) {
    if (!ids.has(removed.id)) { detachBackend(removed.id); purgeClientState(removed.id); }
  }
  publishManifestBackends(rows.map((row) =>
    descriptorForAttachedId(row.id, systemLabel(row), '', row.nickname ?? '', row.backendId)));
  statuses = new Map(rows.map((row) => [row.id, {
    lastReachedMs: row.lastReachedMs ?? 0,
    deviceNameSyncError: row.deviceNameSyncError ?? '',
    ownDeviceSyncError: row.ownDeviceSyncError ?? '',
  }]));
}

/**
 * Start a pairing. Resolves with the verification number to show; the
 * outcome arrives later on `backend:attach`.
 */
export async function addSystem(pairingLink: string): Promise<PendingAttachment> {
  const attachment = await AddBackend(pairingLink);
  revision++;
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
  forgetSystem(id);
}

/**
 * Everything a removal does to THIS page, with no RPC. The chokepoint the
 * acting page and every other page on this host both go through, so a
 * removal made in one window cannot leave another one holding a socket to a
 * profile that no longer exists.
 *
 * Idempotent: the acting page runs it and then receives its own echo. The
 * filters no-op, the transport calls are already-detached no-ops, and
 * `purgeClientState` is a deletion of state that is by then gone.
 */
function forgetSystem(id: string): void {
  revision++;
  pending = pending.filter((p) => p.id !== id);
  if (statuses.has(id)) {
    const next = new Map(statuses);
    next.delete(id);
    statuses = next;
  }
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

/**
 * `backend:set-changed` — a removal or a rename, made by any page on this
 * host. Called by events.ts.
 *
 * Its own channel rather than a second meaning on `backend:attach`: that one
 * answers "how did the pairing I started end" and retires a pending row,
 * which a rename must not do.
 *
 * Origin is checked for the same reason `applyBackendAttach` checks it — the
 * event hub subscribes every attached backend, the channel is loopback-only
 * rather than home-only, and these RPCs act on THIS machine's profile
 * directory. Another backend's frame names an id in its own profile
 * directory, which would drop the wrong row here.
 */
export function applyBackendSetChange(
  evt: BackendSetChangeEvent,
  origin: BackendKey = HOME_BACKEND,
): void {
  if (origin !== HOME_BACKEND || !evt) return;
  if (evt.action === 'membership') {
    revision++;
    void loadSystems().catch((err) => addToast('error', `Could not refresh device connections: ${errString(err)}`));
    return;
  }
  if (!evt.id) return;
  if (evt.action === 'removed') {
    // The label is read BEFORE the row goes: after forgetSystem there is
    // nothing left on this side that knows what the machine was called.
    if (evt.reason === 'ended-by-computer') {
      addToast('warning', `${removedSystemLabel(evt.id)} ended this computer's access. Pair again from Connect to a computer.`);
    }
    forgetSystem(evt.id);
    return;
  }
  // A rename and a device-name-sync change both land in fields only the
  // authoritative list holds — the folded label, the sync errors — so both
  // re-read it. The bumped revision keeps an older in-flight read from
  // publishing the superseded answer over the fresh one.
  if (evt.action === 'renamed' || evt.action === 'device-name-sync') {
    revision++;
    void loadSystems().catch((err) => addToast('error', `Could not refresh device connections: ${errString(err)}`));
  }
}

/** The name a person sees for a system: their nickname, else the machine's own. */
export function systemLabel(system: Pick<AttachedBackend, 'name' | 'nickname' | 'id'>): string {
  return system.nickname || system.name || system.id;
}

/**
 * The label for a machine that is about to be forgotten: the transport
 * registry's entry (every page on this host carries every attached door,
 * whether or not Settings ever opened), else the id. The entry's `name`
 * already folds the profile nickname in, because `publishSystems` writes
 * it through `systemLabel`.
 */
function removedSystemLabel(id: string): string {
  const entry = backendById(id);
  return entry?.nickname || entry?.name || id;
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
 * directory these RPCs act on. The Go channel is loopback-only and
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
    // Another window's result (or a delayed result after removal) is only
    // an invitation to refresh. The current profile set decides membership.
    // The profile id IS the machine's UUID (attachedbackends.Attachment.ID
    // is the link's backend id), so the entry answers to it at once.
    if (row) publishAttachedBackend(descriptorForAttachedId(evt.id, name, '', '', evt.id));
    if (hasScope('host')) void loadSystems().catch((err) => addToast('error', errString(err)));
    return { name, error: '' };
  }
  // A failure for a row this page no longer holds is not this page's to
  // report: it cancelled the pairing (RemoveBackend ends the wait, which
  // arrives here as a refusal), or another window started it and will hear
  // the answer itself.
  if (!row) return null;
  return { name, error: evt.error || 'the pairing was not confirmed' };
}

/** Test seam. */
export function __resetSystemsForTest(): void {
  revision++;
  statuses = new Map();
  loaded = false;
  pending = [];
  loadInFlight = null;
}
