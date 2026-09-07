import { preparePairingTrust } from '../native/networkTrust';
import { canVerifyComputerRoutes, verifyComputerRoute } from '../native/computerRouteProbe';
import { mergeComputerRoutes, type ComputerRoute } from './computerRoute';
import { isNativeShell } from '../native/platform';
import { networkFetch } from './networkFetch';
import { clientDeviceName } from '../stores/clientDeviceName.svelte';
import { ownDeviceConnectionExcluded, setOwnDeviceConnectionExcluded } from './ownDeviceConnections';
// Attaching, listing and detaching a second machine from a client that IS
// the client.
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
//
// **Removing is one door, not three.** A machine this client attached
// itself is held in three places — the socket in the registry, the
// credential in the session slot, the address in the endpoint map — and
// all three belong to this directory. `detachAttachedBackend` is the only
// caller that knows they are three; `SystemsSection.svelte` calls one
// function, the same way it calls one to attach.
//
// **The pending pairing lives here too**, in a plain module map with a
// change listener (the shape ./manifestBackends.ts uses for the same
// reason). It has to outlive the call that started it: the owner confirms
// on the OTHER machine, minutes later, and a section that held its own
// "waiting" flag across that wait would be a form disabled for ten
// minutes and a row that vanished on a re-render.

import { HOME_BACKEND, type BackendKey } from './backendKey';
import { attachedBackends, backendById, detachBackend, duplicateLegacyHomeBackend, syncAttachedBackends } from './backends';
import {
  clearPairedSession,
  hasOwnDeviceSession,
  pairedComputerId,
  pairedSessionId,
  parsePairingFragment,
  probeActivation,
  redeemPairing,
  type PairingPayload,
} from './deviceSession';
import { runBeforeBackendDetach } from './detachSteps';
import { purgeClientState } from './clientPurge';
import {
  endpointHost,
  setHomeEndpoint,
  forgetBackendEndpoint,
  storedBackendEndpoints,
} from './homeEndpoint';

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

/** A repeated invitation repairs its own slot, including a legacy first
 * pairing. New computers never replace the first computer's credential. */
export function pairingBackendKey(payload: PairingPayload): BackendKey {
  if (!payload.backendId || payload.backendId.includes(' ')) {
    throw new Error('That pairing link does not name a machine this app can attach.');
  }
  if (duplicateLegacyHomeBackend() === payload.backendId) return payload.backendId;
  return attachedBackends().find((entry) => entry.backendId === payload.backendId)?.id
    ?? Object.keys(storedBackendEndpoints()).find((key) => pairedComputerId(key) === payload.backendId)
    ?? payload.backendId;
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
 * step waiting for a human. `awaitAttachedActivation` is the other half,
 * and the pending row this records is what the wait is visible as.
 */
export async function attachBackendFromLink(link: string): Promise<AttachedPairing> {
  const payload = payloadFromLink(link);
  setOwnDeviceConnectionExcluded(payload.backendId, false);
  return attachPairing(payload);
}

/** Introductions obey local removal and the sponsoring connection's lifetime. */
export async function attachIntroducedBackend(link: string, current: () => boolean, routes: readonly ComputerRoute[]): Promise<AttachedPairing> {
  const payload = payloadFromLink(link);
  if (payload.purpose !== 'own-introduction') throw new Error('This is not a device introduction.');
  const admitted = () => current() && !ownDeviceConnectionExcluded(payload.backendId);
  const assertCurrent = () => {
    if (!admitted()) throw new DOMException('Pairing was canceled.', 'AbortError');
  };
  assertCurrent();
  if (!await canVerifyComputerRoutes()) throw new Error('Update the phone app to connect your devices automatically.');
  assertCurrent();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 20_000);
  let selected: ComputerRoute;
  try {
    // Probe only authenticated catalog addresses, before spending the token.
    // The shared probe owns native concurrency bounds and verifies TLS + UUID.
    selected = await Promise.any(mergeComputerRoutes([], routes).map(async (route) => {
      await verifyComputerRoute(route, payload.backendId, controller.signal);
      assertCurrent();
      return route;
    }));
  } catch {
    assertCurrent();
    throw new Error('No verified address for this computer is reachable.');
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
  assertCurrent();
  // One redemption only. A lost POST reply is never replayed on another route.
  return attachPairing({ ...payload, endpoint: selected.endpoint, certFingerprint: selected.certFingerprint }, admitted);
}

async function attachPairing(payload: PairingPayload, current: () => boolean = () => true): Promise<AttachedPairing> {
  if (!current()) throw new DOMException('Pairing was canceled.', 'AbortError');
  const id = pairingBackendKey(payload);
  const name = payload.backendName || endpointHost(payload.endpoint);
  const trust = preparePairingTrust(payload);
  const endpoint = trust.endpoint;
  const outcome = await redeemPairing(payload, clientDeviceName(), networkFetch, id, { current, endpoint, trust });
  if (id === HOME_BACKEND && isNativeShell()) setHomeEndpoint(endpoint);
  setPendingAttachment({
    id,
    name,
    endpoint,
    verificationNumber: outcome.verificationNumber,
  });
  return { id, name, verificationNumber: outcome.verificationNumber };
}

/**
 * Poll until the owner confirms the pairing on that machine, then attach.
 * Answers false when the window lapsed without a confirmation, which is
 * the caller's cue to say so rather than to keep asking.
 *
 * The pending row is this loop's liveness: a machine removed while the
 * wait is running stops it on the next tick rather than leaving a timer
 * probing a credential that has been cleared for the rest of the window.
 * Either ending retires the row, so nothing has to remember to.
 */
export async function awaitAttachedActivation(
  id: string,
  intervalMs = 3_000,
  deadlineMs = 10 * 60_000,
): Promise<boolean> {
  const attachment = pending.get(id);
  const session = pairedSessionId(id);
  const current = () => !!attachment && pending.get(id) === attachment && pairedSessionId(id) === session;
  const deadline = Date.now() + deadlineMs;
  for (;;) {
    if (!current()) return false;
    const active = await probeActivation(networkFetch, id);
    if (!current()) return false;
    if (active) {
      forgetPendingAttachment(id);
      // The descriptor is rebuilt from the stored endpoint map, so this
      // is the same sync a shell boot performs — one code path for "these
      // are the machines I am attached to".
      syncAttachedBackends();
      await backendById(id)?.client.redialAfterPairing();
      return pairedSessionId(id) === session;
    }
    if (Date.now() >= deadline) {
      forgetPendingAttachment(id);
      return false;
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}

/**
 * Remove a machine this client attached itself: the socket, then the
 * credential, then the address, then what it stored on this device.
 *
 * That order is the whole reason this is one function. Closing the socket
 * first means nothing is dialing while the credential is being taken
 * away, so no reconnect can present a session that is half gone; the
 * endpoint goes last for the rule the pairing paths keep in the other
 * direction — a stored session never outlives the knowledge of where to
 * present it, so it also never outlives it on the way out.
 *
 * Forgetting the endpoint is what makes the removal survive a relaunch:
 * a shell's attached list is REBUILT from the endpoint map
 * (`manifestBackends.storedBackendDescriptors`), so a machine still in the
 * map would be re-attached by the next boot's sync.
 *
 * A pending pairing for the same machine is retired FIRST, ahead of the
 * three. It is not one of them — it is this client's own bookkeeping —
 * but it is also the activation poll's liveness, and taking the
 * credential from under a poll that was still running would spend a
 * request per interval for the rest of the confirmation window.
 *
 * The installed detach STEPS run before all of it, and that placement is
 * the point rather than a nicety: the only one today is a phone
 * withdrawing its push registration, which is a call over the very socket
 * about to be closed. A device that has already let go has no way left to
 * say "stop waking me", and the backend would keep sending until the
 * registration died of old age (./detachSteps.ts).
 *
 * A desktop's local HOME is refused. A phone's legacy HOME pairing can be
 * removed independently; its caller reloads after closing that singleton.
 */
export function detachAttachedBackend(id: BackendKey): void {
  if (id === HOME_BACKEND && !isNativeShell()) return;
  setOwnDeviceConnectionExcluded(pairedComputerId(id) || backendById(id)?.backendId || id, true);
  detachConnection(id);
}

/** Membership removals retire only group credentials, without recording a local opt-out. */
export function retireOwnDeviceBackend(id: BackendKey): void {
  if (hasOwnDeviceSession(id)) detachConnection(id);
}

function detachConnection(id: BackendKey): void {
  // Forgetting the canonical computer also retires its dormant old slot;
  // otherwise the next launch would resurrect the removed connection.
  if (id !== HOME_BACKEND && duplicateLegacyHomeBackend() === id) detachConnection(HOME_BACKEND);
  runBeforeBackendDetach(id);
  forgetPendingAttachment(id);
  detachBackend(id);
  clearPairedSession(id);
  forgetBackendEndpoint(id);
  // The fourth thing, and it is not one of the three above: those end the
  // relationship, this removes what the relationship left behind. That
  // machine's replica database and its ui_state bucket are readable by
  // whoever holds this device, and nothing else will ever reclaim them.
  // After the credential, because a purge is not a reason to keep talking.
  purgeClientState(id);
}

// ---------------------------------------------------------------------------
// The machines this client attached, as a list
// ---------------------------------------------------------------------------

/** One attached machine, as a surface shows it. */
export interface AttachedMachine {
  /** Registry id. The `backendId` its pairing payload named. */
  id: string;
  /** Its descriptor's name, else the endpoint host it was paired at. */
  name: string;
  /** The host part of its endpoint, which is the address a person typed. */
  host: string;
}

/**
 * The registry fields this join reads, and no others.
 *
 * A `BackendEntry` parameter would put `status` — a live getter — within
 * reach of a snapshot that has no way to stay current with it. Naming the
 * three fields keeps that mistake from compiling.
 */
type NamedBackend = { id: string; home: boolean; name: string };

/**
 * Every machine this client attached itself, home excluded.
 *
 * The registry is the source rather than an RPC: on a phone there is no
 * local process holding profiles to ask, and the sockets the registry
 * already holds ARE the machines. The stored endpoint map supplies the
 * address the registry has no field for.
 *
 * REACHABILITY IS DELIBERATELY NOT HERE. An entry's `status` is a getter
 * onto its client and moves without the list moving, so a row that read
 * it through this snapshot would show whatever was true when the list was
 * last rebuilt. It is `stores/transportStatus.svelte.ts`'s signal, read
 * per row through `backendReachable(id)`, which is the same answer the
 * composer's machine picker dims on.
 *
 * `entries` defaults to the registry's own array, which is plain on
 * purpose (./backends.ts: the fan-out walks it). A Svelte surface passes
 * the reactive mirror instead, so its list re-derives on attach and
 * detach rather than on a signal read this module smuggled in.
 */
export function attachedMachines(
  entries: readonly NamedBackend[] = attachedBackends(),
): AttachedMachine[] {
  const endpoints = storedBackendEndpoints();
  const out: AttachedMachine[] = [];
  for (const entry of entries) {
    if (entry.home) continue;
    const host = endpointHost(endpoints[entry.id] ?? '');
    out.push({ id: entry.id, name: entry.name || host || entry.id, host });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Pairings this client started and is waiting on
// ---------------------------------------------------------------------------

/** A redeemed pairing whose owner has not confirmed it yet. */
export interface PendingAttachedBackend {
  id: string;
  /** What to call the machine while it is still being confirmed. */
  name: string;
  /** Where it is, for a row that has nothing else to show yet. */
  endpoint: string;
  /** The six digits the owner compares on that machine. */
  verificationNumber: string;
}

const pending = new Map<string, PendingAttachedBackend>();
const pendingListeners = new Set<() => void>();

/** Every pairing this client is waiting on, in the order they started. */
export function pendingAttachments(): readonly PendingAttachedBackend[] {
  return [...pending.values()];
}

/** Subscribe to the pending list moving. Does NOT fire immediately. */
export function onPendingAttachmentsChanged(listener: () => void): () => void {
  pendingListeners.add(listener);
  return () => {
    pendingListeners.delete(listener);
  };
}

function setPendingAttachment(row: PendingAttachedBackend): void {
  pending.set(row.id, row);
  notifyPending();
}

function forgetPendingAttachment(id: string): void {
  if (!pending.delete(id)) return;
  notifyPending();
}

function notifyPending(): void {
  for (const listener of pendingListeners) listener();
}

/** Test seam: forget every pairing this module is waiting on. */
export function __resetPendingAttachmentsForTest(): void {
  pending.clear();
}
