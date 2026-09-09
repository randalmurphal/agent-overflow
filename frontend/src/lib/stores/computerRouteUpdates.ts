import { attachedBackends, backendById, backendDescriptor, onBackendsChanged, withBackendTarget, type BackendEntry } from '../transport/backends';
import { refreshComputerRoutes } from '../transport/bootstrap';
import { hasPairedSession, pairedSessionId, observePairedComputerBootstrap } from '../transport/deviceSession';
import { isNativeShell } from '../native/platform';
import { mergeComputerRoutes } from '../transport/computerRoute';
import { GetComputerRoutes, RepairBackendAddress } from './bindings';
import { wailsEventOn } from './wailsEvents';

const CHANNEL = 'computer-routes:changed';

async function refreshRoutes(entry: BackendEntry, backendId: string, current: () => boolean, signal: AbortSignal): Promise<void> {
  const descriptor = backendDescriptor(entry.id);
  if (!descriptor) return;
  const bootstrap = async (): Promise<void> => {
    signal.throwIfAborted();
    const request = new AbortController();
    const abort = () => request.abort(signal.reason);
    signal.addEventListener('abort', abort, { once: true });
    const timeout = setTimeout(() => request.abort(), 20_000);
    try {
      await refreshComputerRoutes(descriptor, backendId, () => current() && !request.signal.aborted, request.signal);
      request.signal.throwIfAborted();
    } finally { clearTimeout(timeout); signal.removeEventListener('abort', abort); }
  };
  try { await bootstrap(); return; }
  catch (error) {
    if (!current()) return;
    // Rebinding can retire HTTP while the authenticated socket remains alive.
    const routes = mergeComputerRoutes([], await withBackendTarget(entry.id, GetComputerRoutes));
    if (!current()) return;
    if (isNativeShell()) {
      await observePairedComputerBootstrap(entry.id, backendId, routes, current);
      return;
    }
    // The local proxy owns desktop credentials. Its existing repair path
    // verifies identity using the saved certificate trust before changing ports.
    for (const route of routes) {
      if (!current()) return;
      try {
        await RepairBackendAddress(entry.id, route.endpoint);
        if (current()) await bootstrap();
        return;
      } catch { /* Another advertised route may still be reachable. */ }
    }
    throw error;
  }
}

/**
 * Refresh authoritative route hints without interrupting a healthy socket.
 * Events, replay gaps, hello changes, and reconnects are coalesced per computer.
 * Each refresh belongs to the captured client, session, and watch generation;
 * replacement, disconnect, or teardown cancels it and prevents a late result
 * from changing routes.
 *
 * Bootstrap is the primary authenticated source. If HTTP became unavailable
 * during a listener move while WebSocket remains connected, `GetComputerRoutes`
 * supplies current candidates. Native clients admit them through the captured
 * pairing; desktop proxies use certificate-verified address repair before
 * refreshing bootstrap. Missing `computer-routes.v1` means the host keeps its
 * existing route behavior.
 */
export function installComputerRouteUpdates(): () => void {
  type Watch = { entry: BackendEntry; stop(): void; dirty: boolean; busy: boolean; scheduled: boolean; epoch: number;
    retry?: ReturnType<typeof setTimeout>; delay: number; abort?: AbortController };
  const watches = new Map<string, Watch>();
  let stopped = false;
  const connected = (watch: Watch) => !stopped && backendById(watch.entry.id)?.client === watch.entry.client
    && watch.entry.client.getStatus().status === 'connected'
    && !!watch.entry.client.getHello()?.capabilities.includes('computer-routes.v1');

  async function refresh(watch: Watch): Promise<void> {
    if (watch.busy || !watch.dirty || !connected(watch)) return;
    watch.busy = true; watch.dirty = false;
    clearTimeout(watch.retry);
    const epoch = watch.epoch;
    const session = pairedSessionId(watch.entry.id);
    const current = () => connected(watch) && watch.epoch === epoch && pairedSessionId(watch.entry.id) === session;
    const controller = new AbortController();
    watch.abort = controller;
    let failed = false;
    try {
      const id = watch.entry.client.getHello()?.backendId;
      if (id) await refreshRoutes(watch.entry, id, () => current() && !watch.dirty && !controller.signal.aborted, controller.signal);
      controller.signal.throwIfAborted();
      watch.delay = 1_000;
    } catch { failed = current(); }
    finally {
      watch.busy = false; watch.abort = undefined;
      if (current()) {
        if (watch.dirty) void refresh(watch);
        else if (failed) {
          watch.retry = setTimeout(() => { watch.dirty = true; void refresh(watch); }, watch.delay);
          watch.delay = Math.min(watch.delay * 2, 30_000);
        }
      } else if (connected(watch) && watch.dirty) void refresh(watch);
    }
  }
  function invalidate(watch: Watch): void {
    watch.dirty = true;
    if (watch.scheduled) return;
    watch.scheduled = true;
    queueMicrotask(() => { watch.scheduled = false; void refresh(watch); });
  }
  function reconcile(): void {
    for (const [id, watch] of watches) {
      if (backendById(id)?.client !== watch.entry.client) { watch.stop(); watches.delete(id); }
    }
    for (const entry of attachedBackends()) {
      // Local desktop HOME owns its listeners rather than a remote route profile.
      if (watches.has(entry.id) || (entry.home && !hasPairedSession(entry.id))) continue;
      const watch: Watch = { entry, stop: () => {}, dirty: true, busy: false, scheduled: false, epoch: 0, delay: 1_000 };
      watches.set(entry.id, watch);
      const status = entry.client.onStatusChange(() => {
        if (!connected(watch)) { watch.epoch++; watch.abort?.abort(); clearTimeout(watch.retry); }
        invalidate(watch);
      });
      const hello = entry.client.onHelloChange(() => invalidate(watch));
      watch.stop = () => { watch.epoch++; watch.abort?.abort(); clearTimeout(watch.retry); status(); hello(); };
    }
  }
  const onOrigin = (id: string) => {
    if (!id) return;
    for (const watch of watches.values()) if (watch.entry.client.getHello()?.backendId === id) invalidate(watch);
  };
  const stopEvents = wailsEventOn(CHANNEL, (_, origin) => onOrigin(origin.backendId));
  const stopGaps = wailsEventOn<{ channel: string }>('transport:gap', (gap, origin) => {
    if (gap.channel === CHANNEL) onOrigin(origin.backendId);
  });
  const stopBackends = onBackendsChanged(reconcile);
  reconcile();
  return () => { stopped = true; stopEvents(); stopGaps(); stopBackends(); for (const watch of watches.values()) watch.stop(); watches.clear(); };
}
