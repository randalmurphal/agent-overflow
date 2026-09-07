// Sidebar visibility belongs to this frontend. Exclusions use computer UUIDs,
// never mutable route slots; newly paired computers are visible by default.
import { onBackendIdentity } from '../transport/backendIdentity';
import type { BackendKey } from '../transport/backendKey';
import { backendDisplayName, getAttachedBackends } from './attachedBackends.svelte';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

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
    return [{ key, id, name: backendDisplayName(entry), visible: !id || !hidden.has(id) }];
  });
});

export function sidebarDevices() { return devices; }
export function sidebarDeviceFilterActive(): boolean { return hiddenRoutes.size > 0; }
export function sidebarBackendVisible(key: BackendKey | undefined): boolean {
  return key === undefined || !hiddenRoutes.has(key);
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
