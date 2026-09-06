// Updating a SUPERVISED serve host from wherever this page is
// (docs/architecture/serve-mode.md § Updating over the wire;
// docs/specs/remote-access.md, the W8h2 notes).
//
// One entry PER BACKEND, because the question is per machine: a desktop
// attached to two serve hosts can update either, and the page's own
// backend may itself be a serve host reached through a browser. Keyed by
// registry id, the same key everything else per backend uses; a frame's
// origin is translated once through `backendKeyForOrigin`.
//
// This is NOT the in-app updater (`updates.svelte.ts`). That one swaps the
// binary the page is running inside and is `host`-scoped; this one asks a
// machine's supervisor to stage and run a different version, over the
// wire, and needs `access:admin`. A desktop install answers `supervised:
// false` and renders nothing here.
//
// Three things about the flow, all decided by the backend and mirrored
// here rather than re-decided:
//
//   - The status frame is the WHOLE `ServiceUpdateStatus`, on every phase
//     change, and `GetServiceUpdateStatus` answers the same shape. So a
//     late subscriber, a reconnect and a first paint all converge on one
//     read, and this store never merges fields.
//   - After `requested` the backend restarts. Its socket drops, the new
//     process says hello, and that hello is what re-reads the status: the
//     new version reports itself idle with its own `currentVersion`.
//   - `service:update-outcome` is the supervisor's verdict, `committed` or
//     `rolled-back`, carrying the `updateId` the request answered. It is
//     kept beside the status so the person who pressed Update is told how
//     it ended, and cleared by the next request. It names TWO versions,
//     because on a rollback the version that booted is not the version that
//     failed. The backend publishes it at most once per settled update, so
//     a machine that has rebooted since one does not re-announce it.

import {
  GetServiceUpdateStatus,
  ListServiceReleases,
  RequestServiceUpdate,
  CancelServiceUpdate,
  type ReleaseSummary,
  type ServiceUpdateStatus,
} from './bindings';
import { wailsEventOn } from './wailsEvents';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { getAttachedBackends } from './attachedBackends.svelte';
import { isMethodUnavailableError, onBackendHelloChange, onBackendStatusChange, getTransportHelloFor } from './transportStatus.svelte';
import {
  attachedBackends as registryBackends,
  backendKeyForOrigin,
  withBackendTarget,
  onBackendDetached,
} from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { hasScope } from '../transport/scopes';
import { isScopeRefusal } from '../transport/scopeRefusal';
import { userFacingError } from '../utils/userFacingError';

export type { ReleaseSummary, ServiceUpdateStatus };

/** The supervisor's verdict on one update, as `service:update-outcome` spells it. */
export interface ServiceUpdateOutcome {
  updateId: string;
  outcome: 'committed' | 'rolled-back' | string;
  /** What actually booted. On a rollback this is the version that came back. */
  version: string;
  /** What the update was aiming at. On a rollback this is the version that failed. */
  target?: string;
  reason?: string;
}

/** Everything this page knows about updating one machine. */
export interface MachineUpdate {
  /** The last status read or pushed; null until the first read lands. */
  status: ServiceUpdateStatus | null;
  /** A failed read that was not a refusal. Refusals leave `status` null and say nothing. */
  loadError: string;
  /** Request sent, first status frame not yet back. */
  requesting: boolean;
  canceling: boolean;
  requestError: string;
  outcome: ServiceUpdateOutcome | null;
  releases: readonly ReleaseSummary[];
  releasesLoaded: boolean;
  releasesLoading: boolean;
  releasesError: string;
  selectedTag: string;
}

const EMPTY: MachineUpdate = Object.freeze({
  status: null,
  loadError: '',
  requesting: false,
  canceling: false,
  requestError: '',
  outcome: null,
  releases: [],
  releasesLoaded: false,
  releasesLoading: false,
  releasesError: '',
  selectedTag: '',
});

const machines = createKeyedSignalRegistry<MachineUpdate>(EMPTY);
const statusReads = new Map<BackendKey, symbol>();

function patch(key: BackendKey, changes: Partial<MachineUpdate>): void {
  machines.set(key, { ...machines.get(key), ...changes });
}

/** One machine's update state. Reactive on that machine's box alone. */
export function machineUpdate(key: BackendKey): MachineUpdate {
  return machines.get(key);
}

/**
 * The attached machines whose backend reports a supervisor, home first.
 * What Settings → Updates renders a card for; empty on a desktop with no
 * serve host attached, which is the common case and renders nothing.
 */
export function supervisedMachines(): BackendKey[] {
  const keys: BackendKey[] = [];
  for (const entry of getAttachedBackends()) {
    if (machines.get(entry.id).status?.supervised) keys.push(entry.id);
  }
  return keys;
}

/** Whether any supervised machine has a newer release waiting. Drives the badge. */
export function hasPendingServiceUpdate(): boolean {
  for (const entry of getAttachedBackends()) {
    const status = machines.get(entry.id).status;
    if (status?.supervised && status.available && (status.latestVersion ?? '') !== '') return true;
  }
  return false;
}

/** The phases between pressing Update and the supervisor's verdict. */
export function isServiceUpdateInFlight(phase: string): boolean {
  return (
    phase === 'resolving' ||
    phase === 'downloading' ||
    phase === 'verifying' ||
    phase === 'staging' ||
    phase === 'waiting' ||
    phase === 'requested'
  );
}

/**
 * Read one machine's status. A PASSIVE load, so it asks before it fires
 * (stores/AGENTS.md): a session without `access:admin` on that backend
 * issues nothing. A backend older than this bundle refuses the call by
 * name, and that is the same silence as an unsupervised host, not an
 * error to show.
 */
export async function loadMachineUpdate(key: BackendKey): Promise<void> {
  if (!hasScope('access:admin', key)) return;
  const request = Symbol();
  statusReads.set(key, request);
  try {
    const status = await withBackendTarget(key, () => GetServiceUpdateStatus());
    if (statusReads.get(key) !== request) return;
    patch(key, { status, loadError: '', requesting: false });
  } catch (err) {
    if (statusReads.get(key) !== request) return;
    if (isScopeRefusal(err) || isMethodUnavailableError(err)) return;
    patch(key, { loadError: userFacingError(err, 'Could not read the update status.') });
  } finally {
    if (statusReads.get(key) === request) statusReads.delete(key);
  }
}

/** The installable releases for one machine's picker. Lazy; read-only. */
export async function loadServiceReleases(key: BackendKey): Promise<void> {
  const current = machines.get(key);
  if (current.releasesLoading) return;
  patch(key, { releasesLoading: true, releasesError: '' });
  try {
    const releases = await withBackendTarget(key, () => ListServiceReleases());
    const kept = releases.some((r) => r.tag === machines.get(key).selectedTag);
    patch(key, {
      releases,
      releasesLoaded: true,
      releasesLoading: false,
      // Default to the latest stable; keep a deliberate pick that is still listed.
      selectedTag: kept
        ? machines.get(key).selectedTag
        : (releases.find((r) => r.isLatest)?.tag ?? releases[0]?.tag ?? ''),
    });
  } catch (err) {
    patch(key, {
      releasesLoading: false,
      releasesError: userFacingError(err, 'Could not load available versions.'),
    });
  }
}

export function selectServiceRelease(key: BackendKey, tag: string): void {
  patch(key, { selectedTag: tag });
}

/**
 * Whether the picker's Install is valid: a listed, non-current release
 * while nothing is in flight on that machine.
 */
export function canInstallSelectedRelease(key: BackendKey): boolean {
  const m = machines.get(key);
  const pick = m.releases.find((r) => r.tag === m.selectedTag);
  if (!pick || pick.isCurrent) return false;
  return !m.requesting && !isServiceUpdateInFlight(m.status?.phase ?? '');
}

/**
 * Ask one machine's supervisor to install a release. The backend does the
 * whole flow and reports each phase on `service:update-status`; this only
 * sends the request and shows a refusal. Step-up is collected by the
 * dispatch interception when the backend asks for it, never here.
 */
export async function requestServiceUpdate(key: BackendKey, tag: string): Promise<void> {
  const m = machines.get(key);
  if (m.requesting || isServiceUpdateInFlight(m.status?.phase ?? '')) return;
  patch(key, { requesting: true, requestError: '', outcome: null });
  try {
    await withBackendTarget(key, () => RequestServiceUpdate(tag));
  } catch (err) {
    patch(key, {
      requesting: false,
      requestError: userFacingError(err, 'Could not start the update.'),
    });
  }
}

/** The status advertises this operation, so old hosts never receive it. */
export async function cancelServiceUpdate(key: BackendKey): Promise<void> {
  const m = machines.get(key);
  if (!m.status?.cancelable || m.canceling) return;
  patch(key, { canceling: true, requestError: '' });
  try {
    await withBackendTarget(key, () => CancelServiceUpdate());
    await loadMachineUpdate(key);
  } catch (err) {
    patch(key, { requestError: userFacingError(err, 'Could not cancel the update.') });
  } finally {
    patch(key, { canceling: false });
  }
}

let cancel: (() => void) | null = null;

/**
 * Subscribe to both channels and read every attached machine's status on
 * its hello, now and on every reconnect. Idempotent; answers a teardown.
 */
export function initServiceUpdates(): () => void {
  if (cancel !== null) return stopServiceUpdates;
  const scheduled = new Set<BackendKey>();
  let stopped = false;
  const schedule = (key: BackendKey) => {
    if (scheduled.has(key)) return;
    scheduled.add(key);
    queueMicrotask(() => {
      if (stopped || !scheduled.delete(key)) return;
      void loadMachineUpdate(key);
    });
  };
  const cancels = [
    wailsEventOn<ServiceUpdateStatus>('service:update-status', (status, origin) => {
      const key = backendKeyForOrigin(origin.backendId);
      statusReads.delete(key);
      patch(key, { status, requesting: false, loadError: '' });
    }),
    wailsEventOn<ServiceUpdateOutcome>('service:update-outcome', (outcome, origin) => {
      patch(backendKeyForOrigin(origin.backendId), { outcome });
    }),
    onBackendHelloChange((key, hello) => {
      if (hello !== null) {
        schedule(key);
        return;
      }
      // A null hello is a dropped socket OR a detached backend. Only the
      // second forgets: a machine mid-restart keeps its `requested` status
      // so the card can say what it is waiting for.
      statusReads.delete(key);
      scheduled.delete(key);
      if (!registryBackends().some((b) => b.id === key)) machines.drop(key);
    }),
    onBackendStatusChange((key, status) => {
      if (status.status === 'connected' && getTransportHelloFor(key)) schedule(key);
      else { statusReads.delete(key); scheduled.delete(key); }
    }),
    onBackendDetached(({ backendId }) => {
      statusReads.delete(backendId);
      scheduled.delete(backendId);
      machines.drop(backendId);
    }),
  ];
  cancel = () => {
    stopped = true;
    scheduled.clear();
    statusReads.clear();
    for (const c of cancels) c();
  };
  return stopServiceUpdates;
}

export function stopServiceUpdates(): void {
  cancel?.();
  cancel = null;
}

/** Test-only: drop every machine and the subscriptions. */
export function resetServiceUpdatesForTest(): void {
  stopServiceUpdates();
  machines.reset();
  statusReads.clear();
}
