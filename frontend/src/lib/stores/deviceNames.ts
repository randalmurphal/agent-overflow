import { isFrontendOnly } from '../transport/runMode';
import { GetDeviceName, UpdateClientDeviceName } from './bindings';
import { clientDeviceName, hasClientDeviceName, onClientDeviceNameChanged, setClientDeviceNameStatus } from './clientDeviceName.svelte';
import { devicePlatform } from '../utils/deviceLabel';
import { errString } from '../utils/errors';
import { attachedBackends, backendById, onBackendsChanged, withBackendTarget, type BackendEntry } from '../transport/backends';
import { wailsEventOn } from './wailsEvents';
import { hasPairedSession } from '../transport/deviceSession';
import { getBackendIdentity, observeBackendName } from '../transport/backendIdentity';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';

/** One small subscription per connected computer; no timers or persisted retry queue. */
export function installDeviceNameSync(): () => void {
  type Watch = { entry: BackendEntry; stop: () => void; busy: boolean; attempted: string; issue: string };
  const watches = new Map<BackendKey, Watch>();
  let stopped = false;

  function report(): void {
    const active = [...watches.values()].filter((watch) => hasPairedSession(watch.entry.id));
    if (!hasClientDeviceName() || active.length === 0) { setClientDeviceNameStatus(''); return; }
    if (active.some((watch) => watch.busy)) { setClientDeviceNameStatus('Updating connected computers…'); return; }
    const issue = active.find((watch) => watch.issue)?.issue;
    setClientDeviceNameStatus(issue || 'Name shared with connected computers.');
  }

  async function sync(watch: Watch): Promise<void> {
    const { entry } = watch;
    // Go-held proxy credentials describe the desktop installation. Only the
    // Go name owner may publish those; this path owns JS-held pairings alone.
    if (stopped || !hasClientDeviceName() || !hasPairedSession(entry.id)) return;
    if (entry.client.getStatus().status !== 'connected') {
      watch.attempted = '';
      watch.issue = 'Saved here. Offline computers will receive the name when they reconnect.';
      report();
      return;
    }
    const hello = entry.client.getHello();
    if (!hello) return;
    if (!hello.capabilities.includes('device-name.v1')) {
      watch.issue = 'Saved here. Update older computers to share this device name with them.';
      report();
      return;
    }
    const name = clientDeviceName();
    if (watch.busy || watch.attempted === name) return;
    watch.busy = true;
    watch.attempted = name;
    watch.issue = '';
    report();
    try {
      await withBackendTarget(entry.id, () => UpdateClientDeviceName(name, devicePlatform()));
    } catch (err) {
      watch.issue = entry.client.getStatus().status === 'connected'
        ? `Could not share this device name: ${errString(err)}. Save again to retry.`
        : 'Saved here. Offline computers will receive the name when they reconnect.';
    } finally {
      watch.busy = false;
      if (!stopped && watches.get(entry.id) === watch) {
        report();
        // A rename while this call was in flight must win in wire order.
        if (clientDeviceName() !== name || watch.attempted === '') void sync(watch);
      }
    }
  }

  function reconcile(): void {
    for (const [id, watch] of watches) {
      if (backendById(id)?.client !== watch.entry.client) { watch.stop(); watches.delete(id); }
    }
    for (const entry of attachedBackends()) {
      if (watches.has(entry.id)) continue;
      const watch: Watch = { entry, stop: () => {}, busy: false, attempted: '', issue: '' };
      watches.set(entry.id, watch);
      const status = entry.client.onStatusChange(() => { void sync(watch); });
      const hello = entry.client.onHelloChange(() => { void sync(watch); });
      watch.stop = () => { status(); hello(); };
      void sync(watch);
    }
    report();
  }

  const stopBackends = onBackendsChanged(reconcile);
  const stopName = onClientDeviceNameChanged(() => {
    for (const watch of watches.values()) { watch.attempted = ''; void sync(watch); }
  });
  const stopEvents = wailsEventOn('backend:name-changed', (data, origin) => {
    const entry = origin.backendId && attachedBackends().find((candidate) => candidate.backendId === origin.backendId);
    if (entry && data && typeof data === 'object' && 'name' in data) observeBackendName(data.name, entry.id);
  });
  reconcile();
  return () => {
    stopped = true;
    stopBackends(); stopName(); stopEvents();
    for (const watch of watches.values()) watch.stop();
    watches.clear();
    setClientDeviceNameStatus('');
  };
}

/** Subscribe to the selected installation's name, keeping origin checks here. */
export function onDeviceNameChanged(backend: BackendKey, changed: (name: string) => void, failed: (error: unknown) => void): () => void {
  let active = true;
  const stop = wailsEventOn<{ name: string }>('backend:name-changed', (data, origin) => {
    if (origin.backendId && origin.backendId === getBackendIdentity(backend).backendId && typeof data?.name === 'string') {
      changed(data.name);
    } else if (!origin.backendId && backend === HOME_BACKEND && isFrontendOnly()) {
      // A frontend-only controller has no history identity. Read its own
      // name instead of trusting a payload with an unknown origin.
      void withBackendTarget(HOME_BACKEND, () => GetDeviceName()).then((name) => {
        if (active) changed(name);
      }).catch((error) => { if (active) failed(error); });
    }
  });
  return () => { active = false; stop(); };
}

/** A peer renamed itself on this host; only that host's list is stale. */
export function onPairedDeviceNamesChanged(backend: BackendKey, changed: () => void): () => void {
  return wailsEventOn('access:devices-changed', (_data, origin) => {
    if (origin.backendId && origin.backendId === getBackendIdentity(backend).backendId) changed();
  });
}
