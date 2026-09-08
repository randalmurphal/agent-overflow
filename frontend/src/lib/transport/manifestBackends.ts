// The attached-backend list as the bootstrap manifest published it, the
// descriptor the page's own backend attaches under, and the per-backend
// manifest fetcher, held in a LEAF.
//
// Why a leaf and not a direct call: ./bootstrap.ts is imported by
// ./wsClient.ts, which is imported by ./backends.ts. A `bootstrap →
// backends` import would close that ring, and the ring has module-level
// side effects at both ends (the `wsClient` singleton, the registry's
// module-load sync), so whichever module the bundler happened to enter
// first would decide whether the app booted. That is not a hypothetical:
// `backends.ts` reads `wsClient` during its own evaluation, which is a
// ReferenceError while `wsClient.ts` is still initialising.
//
// So the manifest PUBLISHES here, the same way it publishes grants,
// harness mode, passkey availability and backend identity into their own
// leaves, and ./backends.ts reads. One direction, no ring. The registry's
// source is `defaultBackendDescriptors` below — one function, and the ONE
// place the client class is decided.

import type { Bootstrap } from './bootstrap';
import type { BackendDescriptor } from './backends';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import { getBackendIdentity } from './backendIdentity';
import { endpointHost, storedBackendEndpoint, storedBackendEndpoints } from './homeEndpoint';
import { hasPairedSession, pairedComputerId } from './deviceSession';
import { rememberedIdentity } from './rememberedIdentity';
import { isNativeShell } from '../native/platform';

let descriptors: readonly BackendDescriptor[] = [];
const listeners = new Set<() => void>();

/**
 * Read the `backends` array off a resolved bootstrap manifest.
 *
 * Every field is validated as a string rather than trusted, for the reason
 * ./bootstrap.ts validates `wsUrl`: an entry this build cannot read is
 * dropped, never coerced into a connection to somewhere unintended.
 * Unknown extra fields are ignored — frames evolve additively.
 */
export function readBackendDescriptors(value: unknown): BackendDescriptor[] {
  if (!Array.isArray(value)) return [];
  const out: BackendDescriptor[] = [];
  for (const raw of value) {
    if (!raw || typeof raw !== 'object') continue;
    const row = raw as Partial<BackendDescriptor>;
    // A registry id must be a non-empty string with no SPACE in it: it is
    // the prefix of every path-keyed composite key
    // (`utils/workspaceKey.ts`), which splits on the first space, and an id
    // that broke that split would silently key one machine's checkout under
    // another's. Rejected at the door rather than escaped at each use.
    if (typeof row.id !== 'string' || row.id === '' || row.id.includes(' ')) continue;
    if (typeof row.wsUrl !== 'string' || typeof row.bootstrapUrl !== 'string') continue;
    out.push({
      id: row.id,
      backendId: typeof row.backendId === 'string' ? row.backendId : '',
      name: typeof row.name === 'string' ? row.name : '',
      ...(typeof row.nickname === 'string' ? { nickname: row.nickname } : {}),
      wsUrl: row.wsUrl,
      bootstrapUrl: row.bootstrapUrl,
    });
  }
  return out;
}

/** What the last resolved manifest named. Empty on every boot that has
 *  one backend, which is every boot today. */
export function manifestBackendDescriptors(): readonly BackendDescriptor[] {
  return descriptors;
}

/**
 * Publish the list a manifest just named. Called on every manifest
 * resolution — the first fetch and every reconnect refetch — so a backend
 * added or removed from Settings takes effect without a reload.
 *
 * Notifies only when the list actually MOVED: a reconnect repeats the same
 * array, and re-running the attach sweep on every reconnect would be pure
 * work for an answer that did not change.
 */
export function publishManifestBackends(next: readonly BackendDescriptor[]): void {
  if (sameDescriptors(descriptors, next)) return;
  descriptors = next;
  for (const listener of listeners) listener();
}

function sameDescriptors(
  a: readonly BackendDescriptor[],
  b: readonly BackendDescriptor[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i += 1) {
    if (
      a[i].id !== b[i].id ||
      a[i].backendId !== b[i].backendId ||
      a[i].name !== b[i].name ||
      a[i].wsUrl !== b[i].wsUrl ||
      a[i].bootstrapUrl !== b[i].bootstrapUrl
    ) {
      return false;
    }
  }
  return true;
}

export function onManifestBackendsChanged(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * How an attached backend's manifest is fetched. Installed by
 * ./bootstrap.ts, which owns the exchange and its validation, and read by
 * ./backends.ts when it constructs a client — the same one-direction rule
 * as the list above.
 */
export type BackendManifestFetcher = (descriptor: BackendDescriptor) => Promise<Bootstrap>;

let fetcher: BackendManifestFetcher | null = null;

export function setBackendManifestFetcher(next: BackendManifestFetcher): void {
  fetcher = next;
}

export function fetchBackendManifest(descriptor: BackendDescriptor): Promise<Bootstrap> {
  if (fetcher === null) {
    return Promise.reject(new Error('backend manifest fetcher not installed'));
  }
  return fetcher(descriptor);
}

/** Test seam: forget the published list. The fetcher is module wiring and
 *  is deliberately kept. */
export function __resetManifestBackendsForTest(): void {
  descriptors = [];
}

// ---------------------------------------------------------------------------
// The page's own backend
// ---------------------------------------------------------------------------

/**
 * The page's own backend, as a desktop attaches it.
 *
 * An ordinary descriptor under the registry id `HOME_BACKEND`: the source
 * below names it first, `backends.attachBackend` hands it the `wsClient`
 * singleton as its client, and nothing else about it is special. Its
 * socket and manifest addresses are the home endpoint's (./homeEndpoint.ts),
 * which that singleton's own default bootstrap resolves — so the
 * descriptor carries the manifest path a paired browser's route refresh
 * fetches (`bootstrap.refreshComputerRoutes`) and no socket address of its
 * own; nothing dials from it. A shell's home is built by
 * `storedBackendDescriptors` from its stored endpoint instead, the way
 * every other machine it holds is.
 */
export const HOME_DESCRIPTOR: BackendDescriptor = {
  id: HOME_BACKEND,
  backendId: '',
  name: '',
  wsUrl: '',
  bootstrapUrl: '/bootstrap.json',
};

/**
 * What the registry attaches: the one source, and the one place the client
 * class is decided.
 *
 * A desktop answers the page's own backend first and then whatever the
 * manifest published — the machines its local process proxies. A shell
 * answers what its endpoint map holds: its legacy home slot first, when one
 * is stored and is not a proven duplicate of a UUID pairing, then every
 * machine it paired with itself. Asked per sync rather than captured, so a
 * native bridge that reports late (the registry evaluates before
 * `prepareNativeShell` runs) is answered correctly by the next sync.
 */
export function defaultBackendDescriptors(): readonly BackendDescriptor[] {
  if (isNativeShell()) return storedBackendDescriptors();
  return [HOME_DESCRIPTOR, ...descriptors];
}

/** A newer UUID pairing supersedes only a proven duplicate legacy phone
 * slot. Leave its credentials/endpoints untouched until explicit removal. */
export function duplicateLegacyHomeBackend(): BackendKey | null {
  if (!isNativeShell() || !storedBackendEndpoint()) return null;
  const id = pairedComputerId() || getBackendIdentity().backendId || rememberedIdentity(HOME_BACKEND)?.backendId;
  return id && storedBackendEndpoint(id) && pairedComputerId(id) === id ? id : null;
}

// ---------------------------------------------------------------------------
// A backend attached after boot
// ---------------------------------------------------------------------------

// The proxy paths the local backend serves an attached profile at. Mirrors
// internal/transport/attachedroutes.go (`AttachedWSPrefix`,
// `AttachedBootstrapPrefix`, `attachedBootstrapSuffix`); the manifest names
// them on every boot, and this is only for the one attach the page itself
// just performed, so it does not wait for the next manifest fetch to learn
// the door it asked for.
const ATTACHED_WS_PREFIX = '/ws/backend/';
const ATTACHED_BOOTSTRAP_PREFIX = '/bootstrap/';
const ATTACHED_BOOTSTRAP_SUFFIX = '.json';

/**
 * The descriptor for one attached profile id, built now.
 *
 * Two realizations of one shape (spec §10, "One seam, two realizations"),
 * and `endpoint` is which:
 *
 *   - **Empty (the desktop).** The local process holds the profile and
 *     proxies it at the same-origin pair the manifest will name anyway,
 *     `/ws/backend/<id>` + `/bootstrap/<id>.json`. Credentials never
 *     enter page script and the CSP stays `'self'`.
 *   - **An origin (a phone).** There is no local process and no proxy:
 *     the client opens the socket itself to THAT backend's endpoint, with
 *     that backend's session in its own credential slot. So both URLs are
 *     absolute and neither carries the attached prefixes — a phone's
 *     second machine is reached at its own `/ws` and `/bootstrap.json`,
 *     the same routes home is reached at, because from the phone's side
 *     there is nothing structurally different about it. Its legacy home
 *     slot is built by the same call, under `HOME_BACKEND`.
 */
export function descriptorForAttachedId(
  id: string,
  name: string,
  endpoint = '',
  nickname?: string,
): BackendDescriptor {
  if (endpoint !== '') {
    const base = new URL(endpoint);
    const scheme = base.protocol === 'https:' ? 'wss://' : 'ws://';
    return {
      id,
      backendId: id,
      name,
      nickname: nickname ?? '',
      wsUrl: scheme + base.host + '/ws',
      bootstrapUrl: base.origin + '/bootstrap.json',
    };
  }
  const scheme = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
  return {
    id,
    backendId: '',
    name,
    ...(nickname !== undefined ? { nickname } : {}),
    wsUrl: scheme + window.location.host + ATTACHED_WS_PREFIX + id,
    bootstrapUrl: ATTACHED_BOOTSTRAP_PREFIX + id + ATTACHED_BOOTSTRAP_SUFFIX,
  };
}

/**
 * Every backend a shell client holds, rebuilt from the endpoint map it
 * persisted (./homeEndpoint.ts). What `defaultBackendDescriptors` answers
 * on a shell: same shape as the manifest's list, different backing store,
 * which is the whole reason the registry's source is one function rather
 * than a client-class branch inside the registry.
 *
 * Home comes FIRST, and only while its legacy slot is stored and is still a
 * computer of its own: a slot proven to duplicate a UUID pairing stays
 * dormant — credentials, endpoint and trust untouched — until explicit
 * removal, and a new phone has no such slot at all. Its name is empty, as
 * a desktop's is, so the same "This machine" placeholder applies.
 *
 * Every other machine's name is the ENDPOINT HOST, which is what
 * `attachBackendFromLink` already writes for a payload that published no
 * `backendName`. An empty one here would leave every surface that labels a
 * machine — the machine picker, Settings → Systems, the sidebar's row —
 * showing a blank until that backend's manifest resolves, which on an
 * unreachable machine is never. An address is the one thing this client is
 * sure of.
 */
export function storedBackendDescriptors(): BackendDescriptor[] {
  const out: BackendDescriptor[] = [];
  const endpoints = storedBackendEndpoints();
  if (endpoints[HOME_BACKEND] && duplicateLegacyHomeBackend() === null) {
    out.push(descriptorForAttachedId(HOME_BACKEND, '', endpoints[HOME_BACKEND]));
  }
  const homeId = endpoints[HOME_BACKEND] && hasPairedSession(HOME_BACKEND)
    ? pairedComputerId(HOME_BACKEND) || rememberedIdentity(HOME_BACKEND)?.backendId : undefined;
  for (const [id, endpoint] of Object.entries(endpoints)) {
    if (id === HOME_BACKEND) continue;
    // A failed redemption can leave an endpoint without a credential. It
    // must not create a second catalog owner beside its valid legacy slot.
    if (id === homeId && pairedComputerId(id) !== id) continue;
    out.push(descriptorForAttachedId(id, endpointHost(endpoint), endpoint));
  }
  return out;
}

/** Publish the current list plus one just-attached profile. */
export function publishAttachedBackend(descriptor: BackendDescriptor): void {
  publishManifestBackends([...descriptors.filter((d) => d.id !== descriptor.id), descriptor]);
}

/** Publish the current list without one just-removed profile. */
export function publishDetachedBackend(id: string): void {
  publishManifestBackends(descriptors.filter((d) => d.id !== id));
}
