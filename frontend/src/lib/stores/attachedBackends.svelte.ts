// The attached-backend list, as the UI reads it.
//
// `transport/backends.ts` holds the registry and keeps it a plain array:
// it is walked on the RPC fan-out path, and a reactive proxy there would
// put a signal read on every call. This module is the mirror the UI
// renders from — the composer's machine picker (wave 7c), Settings →
// Systems, the per-backend reachability the sidebar dims rows on — and it
// wakes only on attach and detach, which happen when somebody adds or
// removes a machine.
//
// Same split as the transport status store beside it, and for the same
// reason: the transport owns the fact, `stores/` owns the rune.

import {
  attachedBackends as registryBackends,
  onBackendsChanged,
  type BackendEntry,
} from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { getBackendIdentity, onBackendIdentity } from '../transport/backendIdentity';
import { rememberedIdentity } from '../transport/rememberedIdentity';
import { endpointHost, storedBackendEndpoint } from '../transport/homeEndpoint';
import { projectBackend, threadBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import { getTransportStatusFor } from './transportStatus.svelte';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

const NICKNAMES_KEY = 'computer-nicknames';
const MAX_NICKNAMES = 128;
function readNicknames(): ReadonlyMap<string, string> {
  const raw = readFrontendValue(NICKNAMES_KEY);
  if (!Array.isArray(raw)) return new Map();
  return new Map(raw.slice(-MAX_NICKNAMES).filter((row): row is [string, string] =>
    Array.isArray(row) && row.length === 2 && typeof row[0] === 'string' && row[0].length > 0 && row[0].length <= 128
    && typeof row[1] === 'string' && row[1].trim().length > 0 && row[1].length <= 80));
}
let nicknames = $state.raw(readNicknames());
onFrontendValueChanged(NICKNAMES_KEY, () => { nicknames = readNicknames(); });
let identityRevision = $state(0);
onBackendIdentity(() => { identityRevision++; });

let list = $state.raw<readonly BackendEntry[]>(registryBackends().slice());
let displayNames = $derived.by(() => {
  void identityRevision;
  return new Map(list.map((entry) => [entry.id, resolveDisplayName(entry)]));
});

onBackendsChanged(() => {
  // A fresh array so the signal moves; the ENTRIES are the registry's own
  // objects, because a backend's identity, scopes and status are getters
  // on them and a copy would freeze all three.
  list = registryBackends().slice();
});

/** Every attached backend, home first. Reactive; safe in a `$derived`. */
export function getAttachedBackends(): readonly BackendEntry[] {
  return list;
}

/**
 * Whether this client is attached to more than one backend.
 *
 * The gate wave 7c's machine picker hides behind: a single-backend app
 * shows no picker at all, so it looks exactly as it does today.
 */
export function hasMultipleBackends(): boolean {
  return list.length > 1;
}

// ---------------------------------------------------------------------------
// What the UI says about a backend
// ---------------------------------------------------------------------------

/** The registry entry for a key, or undefined once it has detached. */
export function attachedBackendEntry(key: BackendKey): BackendEntry | undefined {
  for (const entry of list) if (entry.id === key) return entry;
  return undefined;
}

/**
 * This frontend's nickname for a stable computer identity, if one is saved.
 */
export function backendNickname(key: BackendKey): string {
  void identityRevision;
  const id = attachedBackendEntry(key)?.backendId;
  return id ? nicknames.get(id) ?? '' : '';
}

/** Nicknames belong to this frontend and the stable computer UUID, never an
 * address or the HOME slot. Clearing restores the advertised name. */
export function setBackendNickname(key: BackendKey, value: string): boolean {
  const id = attachedBackendEntry(key)?.backendId;
  if (!id) throw new Error('Connect to this computer once before naming it.');
  const nickname = value.trim();
  if (nickname.length > 80) throw new Error('Use a nickname of 80 characters or fewer.');
  const next = new Map(nicknames);
  next.delete(id);
  if (nickname) next.set(id, nickname);
  while (next.size > MAX_NICKNAMES) next.delete(next.keys().next().value!);
  if (!writeFrontendValue(NICKNAMES_KEY, [...next])) return false;
  nicknames = next;
  return true;
}

function resolveDisplayName(entry: BackendEntry): string {
  const nickname = backendNickname(entry.id);
  if (nickname) return nickname;
  const name = list.find((current) => current.id === entry.id)?.name || entry.name;
  const endpoint = storedBackendEndpoint(entry.id);
  // Phone descriptors are rebuilt from endpoints at boot. That address is
  // a fallback, not a nickname that overrides the host's actual identity.
  if (name && (!endpoint || name !== endpointHost(endpoint))) return name;
  return getBackendIdentity(entry.id).name || rememberedIdentity(entry.id)?.name
    || name || (entry.home ? 'This machine' : entry.id);
}

/** Resolve once per connection/identity/nickname change, not per sidebar row. */
export function backendDisplayName(entry: BackendEntry): string {
  return displayNames.get(entry.id) || entry.name || (entry.home ? 'This machine' : entry.id);
}

/** Test seam: clear both in-memory and persisted frontend nicknames. */
export function __resetBackendNicknamesForTest(): void {
  writeFrontendValue(NICKNAMES_KEY, []);
  nicknames = new Map();
}

/** Whether this backend's socket is open and serving. */
export function backendReachable(key: BackendKey): boolean {
  return getTransportStatusFor(key).status === 'connected';
}

/**
 * The machine a thread lives on, by the row's own id first and its
 * project's second — a draft placeholder has no indexed thread id yet, but
 * its project is on exactly one machine (wave 7d merges entries; until
 * then a project IS a machine choice).
 */
export function threadMachine(threadId: string, projectId: string | null | undefined): BackendKey {
  return threadBackend(threadId) ?? (projectId ? projectBackend(projectId) : undefined) ?? HOME_BACKEND;
}

/**
 * Whether this page can act on a thread's machine ITSELF, rather than
 * through the backend that machine is attached to.
 *
 * True in exactly the ordinary desktop case: the thread runs on the page's
 * own machine and this session holds `host` there. Two questions reduce to
 * this one — whether `localhost` in a thread's output means this reader's
 * `localhost`, and whether the thread's companion browser is an engine
 * this window can show — so they share an answer instead of drifting.
 */
export function threadActsHere(threadId: string): boolean {
  return threadMachine(threadId, null) === HOME_BACKEND && hasScope('host');
}

/**
 * Whether a thread's machine is off-line from this client's point of view.
 *
 * A single-computer outage is already explained by the transport banner.
 * With several computers, every owner is checked, including the first
 * paired host: the other computers can still be used while it is offline.
 */
export function threadMachineUnreachable(threadId: string, projectId: string | null | undefined): boolean {
  if (list.length < 2) return false;
  const key = threadMachine(threadId, projectId);
  return !backendReachable(key);
}
