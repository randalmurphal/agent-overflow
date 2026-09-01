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
