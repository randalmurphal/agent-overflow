// One mounted settings controller, pinned to its originating computer.
// It owns reads, mutations, and the subscription; the component owns its form.
import { onDestroy, untrack } from 'svelte';
import { getAttachedBackends, backendReachable } from './attachedBackends.svelte';
import { withBackendTarget, backendKeyForOrigin } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { getTransportHelloFor, getTransportStatusFor } from './transportStatus.svelte';
import { wailsEventOn } from './wailsEvents';
import { hasScope as computerHasScope } from '../transport/scopes';
import { ListAgentComputers, SetAgentComputerEnabled, PairAgentComputer, MintDevicePairing, DevicePairingStatus, ConfirmDevicePairing, CancelDevicePairing } from './bindings';
import type { AgentComputer } from '../../../bindings/agent-overflow/internal/app/models';
import { errString } from '../utils/errors';

export function createAgentComputers(backend: BackendKey) {
  const call = <T>(issue: () => PromiseLike<T>) => withBackendTarget(backend, issue);
  const hasScope = (scope: Parameters<typeof computerHasScope>[0]) => computerHasScope(scope, backend);
  let rows = $state<AgentComputer[]>([]);
  let error = $state('');
  let busy = $state(false);
  let loaded = $state(false);
  let repair = $state('');
  let revision = 0;
  const capable = () => getTransportHelloFor(backend)?.capabilities?.includes('commands.remote.v1') ?? false;
  const identity = (key: string) => getTransportHelloFor(key)?.backendId || key;
  const destinationAvailable = (entry: ReturnType<typeof getAttachedBackends>[number]) => entry.id !== backend && backendReachable(entry.id) && computerHasScope('access:admin', entry.id);
  let destinations = $derived(getAttachedBackends().filter(destinationAvailable));
  let candidates = $derived(destinations.filter((entry) => !rows.some((row) => row.id === identity(entry.id))));

  async function load(): Promise<void> {
    const request = ++revision;
    try { const next = await call(ListAgentComputers); if (request === revision) { rows = next; loaded = true; error = ''; } }
    catch (err) { if (request === revision) error = errString(err); }
  }
  $effect(() => { if (getTransportStatusFor(backend).status === 'connected' && capable() && hasScope('terminal:operate')) void untrack(load); });
  $effect(() => {
    if (!capable() || !hasScope('terminal:operate')) return;
    return wailsEventOn('agent-computers:changed', (_, origin) => {
      if (backendKeyForOrigin(origin.backendId) === backend) void load();
    });
  });
  onDestroy(() => { revision++; });

  async function toggle(row: AgentComputer): Promise<void> {
    if (busy) return;
    busy = true; error = ''; repair = '';
    try { await call(() => SetAgentComputerEnabled(row.id, !row.enabled)); await load(); }
    catch (err) {
      error = errString(err);
      // A saved profile can still need confirmation or a new pairing. Offer
      // explicit repair on its row's failure, without duplicating every
      // disabled computer in the add selector or silently replacing trust.
      if (!row.enabled) repair = row.id;
    }
    finally { busy = false; }
  }

  async function connect(target: string): Promise<boolean> {
    const destination = destinations.find((entry) => identity(entry.id) === target);
    if (!destination || busy || rows.some((row) => row.id === target && row.enabled)) return false;
    const destinationKey = destination.id;
    const destinationID = identity(destinationKey);
    busy = true; error = ''; repair = '';
    let invitation: string | null = null;
    let confirmed = false;
    try {
      const invite = await withBackendTarget(destinationKey, () => MintDevicePairing('desktop', 'full'));
      invitation = invite.linkId;
      const peer = await call(() => PairAgentComputer(invite.url));
      const status = await withBackendTarget(destinationKey, () => DevicePairingStatus(invite.linkId));
      if (peer.id !== destinationID || !status.verificationNumber || peer.verificationNumber !== status.verificationNumber) throw new Error('The computer pairing could not be verified. Try again.');
      await withBackendTarget(destinationKey, () => ConfirmDevicePairing(invite.linkId));
      confirmed = true;
      await call(() => SetAgentComputerEnabled(peer.id, true));
      await load(); return true;
    } catch (err) { error = errString(err); repair = target; return false; }
    finally {
      if (invitation && !confirmed) {
        try { await withBackendTarget(destinationKey, () => CancelDevicePairing(invitation!)); }
        catch { /* The destination keeps a visible, expiring pending pairing. */ }
      }
      busy = false;
    }
  }

  return {
    get rows() { return rows; },
    get error() { return error; },
    get busy() { return busy; },
    get loaded() { return loaded; },
    get candidates() { return candidates; },
    get repairTarget() { return destinations.some((entry) => identity(entry.id) === repair) ? repair : ''; },
    capable, identity, load, toggle, connect,
  };
}
