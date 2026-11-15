// Provider CLI updates (Claude Code, Codex) on each attached computer.
//
// One entry per backend, keyed by registry id: the question "is a newer
// CLI available, and is it updating" is per machine. `provider:updates`
// and `GetProviderUpdates` / `CheckProviderUpdates` all answer the whole
// `Status[]` for that machine, so every snapshot replaces the entry and
// this store never merges fields. Everything is `access:admin`.
//
// stores/events.ts subscribes the channel and stores/computerHydration.ts
// reads each computer on every connection edge. A disconnect keeps the last
// snapshot; a detach drops it. A read reply applies only while it is the
// newest read of that backend and no frame has landed since it started.
// Frames lost on `provider:updates` (a transport gap) are recovered by
// reading the computer again.
//
// Announcements: one info toast per provider version per client
// (`${provider}@${latestVersion}`, persisted through frontendStorage so a
// relaunch or a second computer does not repeat it), a success toast when
// a run this client watched goes running -> succeeded, and an error toast
// when one goes running -> failed. With more than one computer attached,
// every toast and the notes modal name the computer they are about.

import {
  CheckProviderUpdates,
  GetProviderUpdateNotes,
  GetProviderUpdates,
  RunProviderUpdate,
  type ProviderUpdateNotes,
  type ProviderUpdateStatus,
} from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { isMethodUnavailableError } from './transportStatus.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import { holdBackendRecovery } from './transportRecovery';
import { attachedBackendEntry, backendDisplayName, hasMultipleBackends } from './attachedBackends.svelte';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';
import { backendKeyForOrigin, onBackendDetached, withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { EventOrigin } from '../transport/handle';
import { hasScope } from '../transport/scopes';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { providerLabel } from '../providers/catalog';
import { userFacingError } from '../utils/userFacingError';

export type { ProviderUpdateNotes, ProviderUpdateStatus };

/** How long the "is available" toast stays up. */
export const PROVIDER_UPDATE_TOAST_MS = 15_000;
/** frontendStorage key of the announced `${provider}@${version}` list. */
export const ANNOUNCED_STORAGE_KEY = 'provider-updates-announced';
/** Announced keys kept; older ones are versions nobody is offered again. */
const ANNOUNCED_LIMIT = 32;

interface ComputerProviderUpdates {
  /** The last snapshot read or pushed, by provider. Empty until one lands. */
  statuses: Readonly<Record<string, ProviderUpdateStatus>>;
  /**
   * The RunProviderUpdate call in flight, by provider. The backend
   * publishes the running frame before it answers, so the frame is applied
   * by the time the call settles. Only the call that still owns its entry
   * writes when it settles: a detach, or a later call, has taken it over.
   */
  starting: Readonly<Record<string, symbol>>;
  /** Why RunProviderUpdate was refused, by provider. Cleared by the next run or check. */
  runErrors: Readonly<Record<string, string>>;
  /** A failed snapshot read that was not a refusal or a dropped connection. */
  loadError: string;
}

const EMPTY: ComputerProviderUpdates = Object.freeze({
  statuses: Object.freeze({}),
  starting: Object.freeze({}),
  runErrors: Object.freeze({}),
  loadError: '',
});

const computers = createKeyedSignalRegistry<ComputerProviderUpdates>(EMPTY);
/** The newest snapshot read per backend. A frame or a detach clears it. */
const snapshotReads = new Map<BackendKey, symbol>();

function patch(key: BackendKey, changes: Partial<ComputerProviderUpdates>): void {
  computers.set(key, { ...computers.get(key), ...changes });
}

function without<V>(record: Readonly<Record<string, V>>, name: string): Readonly<Record<string, V>> {
  if (!(name in record)) return record;
  const next = { ...record };
  delete next[name];
  return next;
}

/** One provider's last known update state on one computer; null before the first snapshot. */
export function providerUpdate(provider: string, backend: BackendKey): ProviderUpdateStatus | null {
  return computers.get(backend).statuses[provider] ?? null;
}

/** Whether a newer CLI is waiting and not already being installed. Drives the rail dot. */
export function hasProviderUpdate(provider: string, backend: BackendKey): boolean {
  if (!hasScope('access:admin', backend)) return false;
  const status = providerUpdate(provider, backend);
  return status !== null && status.available && status.latestVersion !== '' && status.run?.phase !== 'running';
}

/** Whether a run is starting or running: the Update control is busy. */
export function providerUpdateRunning(provider: string, backend: BackendKey): boolean {
  const state = computers.get(backend);
  return provider in state.starting || state.statuses[provider]?.run?.phase === 'running';
}

/**
 * The computer's name where a provider update has to say which computer it
 * is about: '' while only one is attached.
 */
export function providerUpdateComputerName(backend: BackendKey): string {
  const entry = attachedBackendEntry(backend);
  return entry && hasMultipleBackends() ? backendDisplayName(entry) : '';
}

function onComputer(backend: BackendKey): string {
  const name = providerUpdateComputerName(backend);
  return name ? ` on ${name}` : '';
}

export interface ProviderUpdateFailure {
  error: string;
  /** The end of the update command's output; '' for a refused start. */
  output: string;
}

/**
 * Why the provider's update did not happen, for the surface that offers it:
 * a refused start, or the last run's failure. Null while a run is on. A
 * failed run stays until a check that answered finds nothing newer: the
 * version was installed some other way and there is nothing to retry.
 */
export function providerUpdateFailure(provider: string, backend: BackendKey): ProviderUpdateFailure | null {
  if (!hasScope('access:admin', backend) || providerUpdateRunning(provider, backend)) return null;
  const state = computers.get(backend);
  const refused = state.runErrors[provider];
  if (refused) return { error: refused, output: '' };
  const status = state.statuses[provider];
  if (!status || status.run?.phase !== 'failed') return null;
  if (!status.checkError && !status.available) return null;
  return { error: status.run.error || 'The update command failed.', output: status.run.output ?? '' };
}

/** Why the last snapshot read of this computer failed, or ''. */
export function providerUpdatesLoadError(backend: BackendKey): string {
  return computers.get(backend).loadError;
}

function readAnnounced(): string[] {
  const value = readFrontendValue(ANNOUNCED_STORAGE_KEY);
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string') : [];
}

/** Marks the key announced; false when it already was. */
function claimAnnouncement(key: string): boolean {
  const announced = readAnnounced();
  if (announced.includes(key)) return false;
  writeFrontendValue(ANNOUNCED_STORAGE_KEY, [...announced, key].slice(-ANNOUNCED_LIMIT));
  return true;
}

function announce(key: BackendKey, previous: ComputerProviderUpdates, next: ComputerProviderUpdates): void {
  if (!hasScope('access:admin', key)) return;
  for (const status of Object.values(next.statuses)) {
    const label = providerLabel(status.provider);
    const phase = status.run?.phase ?? '';
    const before = previous.statuses[status.provider]?.run?.phase ?? '';
    if (before === 'running' && phase === 'succeeded') {
      addToast('success', `${label} updated to ${status.run.toVersion || status.installedVersion}${onComputer(key)}`);
    } else if (before === 'running' && phase === 'failed') {
      addErrorToast(`${label} did not update${onComputer(key)}.`, new Error(status.run.error || 'The update command failed.'));
    }
    if (!status.available || status.latestVersion === '') continue;
    const announcement = `${status.provider}@${status.latestVersion}`;
    // A run in progress is the person already acting on this version, so it
    // is claimed without a toast.
    if (!claimAnnouncement(announcement) || phase === 'running') continue;
    const provider = status.provider;
    addToast('info', `${label} ${status.latestVersion} is available${onComputer(key)}`, PROVIDER_UPDATE_TOAST_MS, {
      label: "What's new",
      run: () => openProviderUpdateNotes(provider, key),
    });
  }
}

function applySnapshot(key: BackendKey, statuses: readonly ProviderUpdateStatus[] | null): void {
  const previous = computers.get(key);
  const byProvider: Record<string, ProviderUpdateStatus> = {};
  for (const status of statuses ?? []) byProvider[status.provider] = status;
  const next: ComputerProviderUpdates = { ...previous, statuses: byProvider, loadError: '' };
  computers.set(key, next);
  announce(key, previous, next);
}

async function readSnapshot(
  key: BackendKey,
  read: () => Promise<ProviderUpdateStatus[]>,
  fallback: string,
): Promise<void> {
  if (!hasScope('access:admin', key)) return;
  const request = Symbol();
  snapshotReads.set(key, request);
  try {
    const statuses = await withBackendTarget(key, read);
    if (snapshotReads.get(key) !== request) return;
    applySnapshot(key, statuses);
  } catch (err) {
    if (snapshotReads.get(key) !== request) return;
    if (isScopeRefusal(err) || isMethodUnavailableError(err) || isPassiveConnectionFailure(err)) return;
    patch(key, { loadError: userFacingError(err, fallback) });
  } finally {
    if (snapshotReads.get(key) === request) snapshotReads.delete(key);
  }
}

/**
 * Read one computer's last known state (memory only on the backend).
 * Passive: issues nothing without `access:admin` on that backend.
 */
export function loadProviderUpdates(key: BackendKey): Promise<void> {
  return readSnapshot(key, () => GetProviderUpdates(), 'Could not read the provider update status.');
}

/** Read one computer again after frames on `provider:updates` were lost. */
export function resyncProviderUpdatesAfterGap(key: BackendKey): void {
  holdBackendRecovery(key, loadProviderUpdates(key));
}

/**
 * Ask one computer to check both providers' release hosts now. Passive (a
 * provider page calls it on mount): silent on a refusal, an older backend
 * or a dropped connection. Clears the refusal shown beside Update.
 */
export function checkProviderUpdates(key: BackendKey): Promise<void> {
  if (!hasScope('access:admin', key)) return Promise.resolve();
  patch(key, { runErrors: EMPTY.runErrors });
  return readSnapshot(key, () => CheckProviderUpdates(), 'Could not check for provider updates.');
}

/**
 * Start the provider's own update command on that computer. An explicit
 * action: a refusal (step-up, already running, nothing to install) shows
 * beside the control and as an error toast. The outcome arrives on
 * `provider:updates`.
 */
export async function runProviderUpdate(provider: string, key: BackendKey): Promise<void> {
  if (providerUpdateRunning(provider, key)) return;
  const state = computers.get(key);
  const request = Symbol();
  const current = () => computers.get(key).starting[provider] === request;
  patch(key, {
    starting: { ...state.starting, [provider]: request },
    runErrors: without(state.runErrors, provider),
  });
  try {
    await withBackendTarget(key, () => RunProviderUpdate(provider));
  } catch (err) {
    const message = userFacingError(err, `Could not start the ${providerLabel(provider)} update${onComputer(key)}.`);
    if (current()) patch(key, { runErrors: { ...computers.get(key).runErrors, [provider]: message } });
    addErrorToast(message, err);
  } finally {
    if (current()) patch(key, { starting: without(computers.get(key).starting, provider) });
  }
}

// ---------------------------------------------------------------------------
// Release notes: one modal for the whole app, for one provider on one computer.

export interface ProviderUpdateNotesView {
  provider: string;
  backend: BackendKey;
  loading: boolean;
  notes: ProviderUpdateNotes | null;
  error: string;
}

let notesView = $state.raw<ProviderUpdateNotesView | null>(null);
let notesRequest: symbol | null = null;

/** The open notes modal's target and content; null when closed. */
export function providerUpdateNotes(): ProviderUpdateNotesView | null {
  return notesView;
}

async function loadNotes(provider: string, backend: BackendKey): Promise<void> {
  const request = Symbol();
  notesRequest = request;
  notesView = { provider, backend, loading: true, notes: null, error: '' };
  try {
    const notes = await withBackendTarget(backend, () => GetProviderUpdateNotes(provider));
    if (notesRequest !== request) return;
    notesView = { provider, backend, loading: false, notes, error: '' };
  } catch (err) {
    if (notesRequest !== request) return;
    notesView = {
      provider,
      backend,
      loading: false,
      notes: null,
      error: userFacingError(err, `Could not load the ${providerLabel(provider)} release notes.`),
    };
  }
}

/**
 * Open the notes modal for one provider on one computer and load them.
 * Every open loads: the range follows the versions of the latest check.
 */
export function openProviderUpdateNotes(provider: string, backend: BackendKey): void {
  void loadNotes(provider, backend);
}

/** Load the open modal's notes again, after an error. */
export function retryProviderUpdateNotes(): void {
  const open = notesView;
  if (open !== null) void loadNotes(open.provider, open.backend);
}

/** Close the modal; a reply still in flight is dropped. */
export function closeProviderUpdateNotes(): void {
  notesRequest = null;
  notesView = null;
}

// ---------------------------------------------------------------------------

function forget(key: BackendKey): void {
  snapshotReads.delete(key);
  computers.drop(key);
  if (notesView?.backend === key) closeProviderUpdateNotes();
}

onBackendDetached(({ backendId }) => forget(backendId));

/**
 * Apply one `provider:updates` frame (stores/events.ts subscribes). A gap
 * marker reaches the channel with no payload: it is not a snapshot, and the
 * recovery read it started stays current.
 */
export function applyProviderUpdatesEvent(statuses: ProviderUpdateStatus[] | null, origin: EventOrigin): void {
  if (!Array.isArray(statuses)) return;
  const key = backendKeyForOrigin(origin.backendId);
  snapshotReads.delete(key);
  applySnapshot(key, statuses);
}

/** Test-only: drop every computer and the open modal. */
export function resetProviderUpdatesForTest(): void {
  computers.reset();
  snapshotReads.clear();
  closeProviderUpdateNotes();
}
