// Which backend a CREATION lands on.
//
// Most RPCs resolve their backend from an entity they already name — a
// thread id, a project id (transport/entityIndex.ts). Creation-shaped
// calls have no such id yet, so somebody has to choose, and the person
// choosing is whoever is looking at the composer. This module holds that
// choice; `transport/methodRoutes.ts`'s `selected` route reads it.
//
// **No UI in this wave.** The machine picker is wave 7c, one more dropdown
// in the composer's existing project / worktree / branch strip, hidden
// until more than one backend is paired (spec §10). Until it exists the
// answer is always the home backend, which is exactly today's behaviour.
//
// Sticky per project is also 7c's ("sticky last-used per project", §10).
// The primitive here is the single current choice plus the per-pane
// override a draft placeholder can carry, because those are what routing
// needs; a per-project memory is a preference layered on top and belongs
// with the picker that writes it.
//
// A choice that names a backend which has since detached answers HOME
// rather than a dead handle: an unreachable target must fail visibly at
// the picker (spec §10, "never silent failover"), and a route resolution
// is not the place that decision gets made.

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { backendById } from '../transport/backends';

let selected = $state<BackendKey>(HOME_BACKEND);
// Per-pane overrides: a draft placeholder staging a thread on another
// machine. Keyed by pane id, dropped when the pane closes. A plain Map,
// not a rune: it is read on the RPC path and written by a picker, and
// nothing renders from it in this wave.
const byPane = new Map<string, BackendKey>();
let activePane: string | null = null;

/**
 * The backend a creation-shaped call goes to.
 *
 * Resolution order: the active pane's own override, then the app-wide
 * choice, then home. Reactive when read from a `$derived`, so 7c's picker
 * renders from the same answer routing uses.
 */
export function selectedBackend(): BackendKey {
  const override = activePane === null ? undefined : byPane.get(activePane);
  const choice = override ?? selected;
  if (choice === HOME_BACKEND) return HOME_BACKEND;
  return backendById(choice) === undefined ? HOME_BACKEND : choice;
}

/** Set the app-wide choice. The picker's write. */
export function setSelectedBackend(backendId: BackendKey): void {
  selected = backendId;
}

/** Stage a pane's own choice — a draft placeholder's machine. */
export function setPaneBackend(paneId: string, backendId: BackendKey | null): void {
  if (backendId === null) byPane.delete(paneId);
  else byPane.set(paneId, backendId);
}

/** Name the pane whose choice `selectedBackend()` should prefer. */
export function setActiveBackendPane(paneId: string | null): void {
  activePane = paneId;
}

/** Test seam: back to the single-backend answer. */
export function __resetSelectedBackendForTest(): void {
  selected = HOME_BACKEND;
  byPane.clear();
  activePane = null;
}
