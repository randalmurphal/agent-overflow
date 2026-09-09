// Native clients hold their own direct sessions. Desktop/headless clients run
// the same introductions through their Go profile manager, even without a UI.
import { isNativeShell } from '../native/platform';
import { attachedBackends, backendById, onBackendsChanged, withBackendTarget, type BackendEntry } from '../transport/backends';
import { attachIntroducedBackend, awaitAttachedActivation, payloadFromLink, retireOwnDeviceBackend } from '../transport/backendAttach';
import { hasPairedSession, hasOwnDeviceSession, pairedSessionId } from '../transport/deviceSession';
import { ownDeviceConnectionExcluded, ownDeviceConnectionPolicy, rememberOwnDeviceMemberships } from '../transport/ownDeviceConnections';
import { backendDisplayName } from './attachedBackends.svelte';
import { ListOwnDevices, SyncOwnDevices, IntroduceOwnDevice, MintOwnDeviceIntroduction, AcceptOwnDeviceIntroduction } from './bindings';
import { wailsEventOn } from './wailsEvents';

let waiting = $state.raw<readonly string[]>([]);
export function ownDeviceConnectionsWaiting(): readonly string[] { return waiting; }

/**
 * Coordinate personal-device membership for the native frontend. Desktop and
 * headless clients run the equivalent coordinator in Go.
 *
 * Only an active personal session may sponsor catalog merges or introductions.
 * Every asynchronous result remains owned by the captured backend client and
 * session; replacement, disconnect, teardown, or local removal prevents it from
 * admitting a connection. Removal records are applied before enrollment, and
 * credentials or conversation data are never copied between computers.
 *
 * Reconciliation is single-flight and coalesces changes that arrive while it is
 * running. Unreachable members retry with bounded backoff. Failures remain in
 * `ownDeviceConnectionsWaiting` for the connection settings UI rather than
 * producing startup notifications.
 */
export function installOwnDeviceSync(): () => void {
  if (!isNativeShell()) return () => {};
  type Watch = { entry: BackendEntry; stop: () => void };
  const watches = new Map<string, Watch>();
  let stopped = false;
  let scheduled = false;
  let running = false;
  let again = false;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let retryDelay = 30_000;

  function current(watch: Watch): boolean {
    const { entry } = watch;
    return !stopped && backendById(entry.id)?.client === entry.client
      && entry.client.getStatus().status === 'connected'
      && !!entry.client.getHello()?.capabilities.includes('own-devices.v1')
      && hasPairedSession(entry.id);
  }

  function schedule(): void {
    if (stopped) return;
    again = true;
    if (running || scheduled) return;
    scheduled = true;
    queueMicrotask(() => { scheduled = false; if (!stopped) void reconcile(); });
  }

  async function reconcile(): Promise<void> {
    if (running || stopped) return;
    running = true;
    again = false;
    clearTimeout(retry);
    const failed = new Set<string>();
    const attempted = new Set<string>();
    try {
      const groups: { watch: Watch; current: () => boolean; group: Awaited<ReturnType<typeof ListOwnDevices>> }[] = [];
      for (const watch of [...watches.values()]) {
        if (!current(watch) || !hasOwnDeviceSession(watch.entry.id)) continue;
        const sponsorSession = pairedSessionId(watch.entry.id);
        const sponsorCurrent = () => current(watch) && pairedSessionId(watch.entry.id) === sponsorSession;
        try {
          const group = await withBackendTarget(watch.entry.id, ListOwnDevices);
          if (sponsorCurrent() && group.enabled) groups.push({ watch, current: sponsorCurrent, group });
        } catch {
          if (current(watch)) failed.add(backendDisplayName(watch.entry) || 'A computer');
        }
      }
      // Remember every available removal before any introduction. Metadata is
      // bounded by the protocol; no transcripts or credentials are replicated.
      const available = groups.filter((sponsor) => sponsor.current());
      rememberOwnDeviceMemberships(available.flatMap(({ group }) => group.members));
      let policy = ownDeviceConnectionPolicy();
      for (const entry of [...attachedBackends()]) {
        if (policy.removed(entry.backendId)) retireOwnDeviceBackend(entry.id);
      }
      // A phone may be the device joining two existing groups. Let an
      // authenticated host perform the canonical merge, then forward the
      // resulting public catalog. No particular host remains required.
      const merger = available.find((sponsor) => sponsor.current());
      if (merger) {
        for (const other of available) {
          if (!merger.current() || !other.current() || other === merger
            || JSON.stringify(other.group.members) === JSON.stringify(merger.group.members)) continue;
          try {
            const joined = await withBackendTarget(merger.watch.entry.id, () => SyncOwnDevices(other.group.members));
            if (merger.current()) merger.group = joined;
          } catch { failed.add('Device group could not be joined'); }
        }
        for (const other of available) {
          if (!merger.current() || !other.current() || other === merger
            || JSON.stringify(other.group.members) === JSON.stringify(merger.group.members)) continue;
          try {
            const joined = await withBackendTarget(other.watch.entry.id, () => SyncOwnDevices(merger.group.members));
            if (other.current()) other.group = joined;
          } catch { failed.add('Device group could not be joined'); }
        }
      }
      rememberOwnDeviceMemberships(available.filter((sponsor) => sponsor.current()).flatMap(({ group }) => group.members));
      policy = ownDeviceConnectionPolicy();
      for (const entry of [...attachedBackends()]) {
        if (policy.removed(entry.backendId)) retireOwnDeviceBackend(entry.id);
      }
      // When the phone joins two computers that have never reached each
      // other, deliver their first introductions over our existing sessions.
      // Each computer redeems with its own key and connects directly.
      for (const source of available) {
        for (const target of available) {
          const id = target.watch.entry.backendId;
          if (source === target || !source.group.enabled || !target.group.enabled || !source.current() || !target.current() || !id
            || source.group.connectedBackendIds?.includes(id) || source.group.excludedBackendIds?.includes(id)
            || policy.removed(id)) continue;
          try {
            const invite = await withBackendTarget(target.watch.entry.id, () => MintOwnDeviceIntroduction(source.group.selfKeyThumbprint));
            if (!source.current() || !target.current()) continue;
            if (payloadFromLink(invite.url).backendId !== id) throw new Error('The introduction names a different computer.');
            await withBackendTarget(source.watch.entry.id, () => AcceptOwnDeviceIntroduction(invite.url));
          } catch { failed.add('Connections between your computers'); }
        }
      }
      const targets = new Map(attachedBackends().map((entry) => [entry.backendId, entry]));
      for (const sponsor of available) {
        if (!sponsor.group.enabled || !sponsor.current()) continue;
        for (const member of sponsor.group.members) {
          const id = member.backendId;
          if (!id || member.removed || policy.excluded(id)) continue;
          const target = targets.get(id);
          if (target && hasOwnDeviceSession(target.id)) continue;
          if (attempted.has(id)) continue;
          attempted.add(id);
          const targetKey = target?.id ?? id;
          const before = pairedSessionId(targetKey);
          const admission = () => sponsor.current() && pairedSessionId(targetKey) === before;
          try {
            const invite = await withBackendTarget(sponsor.watch.entry.id, () => IntroduceOwnDevice(id));
            if (!admission() || ownDeviceConnectionExcluded(id)) continue;
            if (payloadFromLink(invite.url).backendId !== id) throw new Error('The introduction names a different computer.');
            const paired = await attachIntroducedBackend(invite.url, admission, member.routes);
            if (!stopped && await awaitAttachedActivation(paired.id, 1_000, 10_000) !== 'attached') failed.add((target && backendDisplayName(target)) || member.name || 'A computer');
          } catch {
            if (sponsor.current() && !ownDeviceConnectionExcluded(id)) failed.add((target && backendDisplayName(target)) || member.name || 'A computer');
          }
        }
      }
    } catch {
      // Unreadable local membership must stop automatic enrollment, not erase
      // removal knowledge. Keep the issue in settings instead of startup toasts.
      failed.add('Saved device membership');
    } finally {
      running = false;
      if (!stopped) {
        waiting = [...failed];
        if (failed.size) {
          // Sleeping computers should join when they return, without requiring
          // a settings screen or reconnection to an otherwise healthy sponsor.
          retry = setTimeout(schedule, retryDelay);
          retryDelay = Math.min(retryDelay * 2, 300_000);
        } else retryDelay = 30_000;
        if (again) schedule();
      }
    }
  }

  function watchBackends(): void {
    for (const [id, watch] of watches) {
      if (backendById(id)?.client !== watch.entry.client) { watch.stop(); watches.delete(id); }
    }
    for (const entry of attachedBackends()) {
      if (watches.has(entry.id)) continue;
      const watch: Watch = { entry, stop: () => {} };
      watches.set(entry.id, watch);
      const stopStatus = entry.client.onStatusChange(schedule);
      const stopHello = entry.client.onHelloChange(schedule);
      watch.stop = () => { stopStatus(); stopHello(); };
    }
    schedule();
  }

  const stopBackends = onBackendsChanged(watchBackends);
  const stopEvents = wailsEventOn('own-devices:changed', schedule);
  watchBackends();
  return () => {
    stopped = true;
    clearTimeout(retry);
    stopBackends(); stopEvents();
    for (const watch of watches.values()) watch.stop();
    watches.clear();
    waiting = [];
  };
}
