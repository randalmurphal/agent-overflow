// Sidebar visibility belongs to this frontend. Exclusions use computer UUIDs,
// never mutable route slots; newly paired computers are visible by default.
import { onBackendIdentity } from '../transport/backendIdentity';
import type { BackendKey } from '../transport/backendKey';
import type { Thread } from '../types/models';
import { backendDisplayName, backendOfflineLabel, getAttachedBackends, threadMachine } from './attachedBackends.svelte';
import { computerCatalogPending } from './sidebarCatalogs';
import { catalogReadServable } from './computerRows';
import { getTransportStatusFor } from './transportStatus.svelte';
import { startupStatusText } from '../transport/startupProgress';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';
import { remoteAccessAvailable } from '../transport/buildVariant';

const KEY = 'sidebar-hidden-computers';
const MAX_HIDDEN = 128;
function readHidden(): ReadonlySet<string> {
  const value = readFrontendValue(KEY);
  return new Set(Array.isArray(value) ? value.slice(-MAX_HIDDEN).filter(
    (id): id is string => typeof id === 'string' && id.length > 0 && id.length <= 128,
  ) : []);
}
let hidden = $state.raw(readHidden());
let identityRevision = $state(0);
onBackendIdentity(() => { identityRevision++; });
onFrontendValueChanged(KEY, () => { hidden = readHidden(); });

const hiddenRoutes = $derived.by(() => {
  void identityRevision;
  // A build without remote access offers no filter, so a stored exclusion
  // must not hide projects the person has no control to show again.
  if (!remoteAccessAvailable()) return new Set<BackendKey>();
  return new Set(getAttachedBackends().filter((entry) => entry.backendId && hidden.has(entry.backendId)).map((entry) => entry.id));
});
const devices = $derived.by(() => {
  void identityRevision;
  const seen = new Set<string>();
  return getAttachedBackends().flatMap((entry) => {
    const id = entry.backendId;
    const key = id || `unknown:${entry.id}`;
    if (seen.has(key)) return [];
    seen.add(key);
    return [{ key, id, backend: entry.id, name: backendDisplayName(entry), visible: !id || !hidden.has(id) }];
  });
});

export function sidebarDevices() { return devices; }

/** A computer's state as its row in the Projects menu says it. */
export interface SidebarDeviceStatus {
  text: string;
  /**
   * Its lists failed to load while it can serve a read again: the failure
   * is the row's text, and Retry applies.
   */
  failed: boolean;
}

/**
 * What the Projects menu says about one computer: the one place the sidebar
 * names a computer's connection and load state. Plain text, so nothing in
 * the open menu moves. Reactive on that computer alone.
 */
export function sidebarDeviceStatus(backend: BackendKey): SidebarDeviceStatus {
  const pending = computerCatalogPending(backend);
  const status = getTransportStatusFor(backend);
  // The connection comes first: a load error kept from before the
  // computer went offline, or was refused, is not why its lists are
  // missing now, and its Retry would be rejected without a call.
  if (pending !== null && pending.error !== null && catalogReadServable(status.status)) {
    return { text: pending.error, failed: true };
  }
  switch (status.status) {
    case 'connected':
      return { text: pending === null ? 'Connected' : 'Loading…', failed: false };
    case 'starting':
      return { text: status.startup ? startupStatusText(status.startup) : 'Starting…', failed: false };
    case 'disconnected':
      return { text: 'Connecting…', failed: false };
    case 'reconnecting':
      return { text: backendOfflineLabel(backend), failed: false };
    case 'unauthorized':
    case 'pairing-required':
      return { text: 'Not connected', failed: false };
  }
  return status.status satisfies never;
}
export function sidebarDeviceFilterActive(): boolean { return hiddenRoutes.size > 0; }
export function sidebarBackendVisible(key: BackendKey | undefined): boolean {
  return key === undefined || !hiddenRoutes.has(key);
}
/** Whether the device filter shows this thread's computer. */
export function sidebarThreadVisible(thread: Pick<Thread, 'id' | 'projectId'>): boolean {
  return !sidebarDeviceFilterActive() || sidebarBackendVisible(threadMachine(thread.id, thread.projectId));
}
export function setSidebarDeviceVisible(id: string, visible: boolean): void {
  if (!id || !devices.some((device) => device.id === id)) return;
  const next = new Set(hidden);
  next.delete(id);
  if (!visible) next.add(id);
  while (next.size > MAX_HIDDEN) next.delete(next.values().next().value!);
  if (writeFrontendValue(KEY, [...next])) hidden = next;
}
export function showAllSidebarDevices(): void {
  if (writeFrontendValue(KEY, [])) hidden = new Set();
}
