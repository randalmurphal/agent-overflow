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
import { getBackendIdentity } from '../transport/backendIdentity';
import { projectBackend, threadBackend } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import { getTransportStatusFor } from './transportStatus.svelte';

let list = $state.raw<readonly BackendEntry[]>(registryBackends().slice());

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
 * What a person calls this machine. Home's name arrives on its own hello
 * (`backendName`), an attached one's on the descriptor pairing wrote; a
 * backend that published neither is named by its id rather than by a
 * guess, and home falls back to the one thing that is always true of it.
 */
export function backendDisplayName(entry: BackendEntry): string {
  if (entry.home) return getBackendIdentity().name || 'This machine';
  return list.find((current) => current.id === entry.id)?.name || entry.name || entry.id;
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
