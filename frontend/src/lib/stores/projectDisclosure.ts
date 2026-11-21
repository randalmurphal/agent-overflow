// The key a sidebar project's disclosure is persisted by.
//
// A repository checked out on several computers is one sidebar entry, and
// which checkout represents it changes when a computer lists or a checkout
// is added. Collapse and the show-more limit are therefore keyed by the
// ENTRY (`projectEntryKey`: the repository key of this computer's chosen
// checkout, else the project's own id), which no listing, filter, attach or
// detach changes. `stores/sidebar.svelte.ts` holds the record and knows
// nothing of what the key means; this module is the only place that turns a
// project into one.
import { projectEntryKey } from './projects.svelte';
import { expandProject } from './sidebar.svelte';

/** The key the entry a project belongs to is disclosed by. */
export function projectDisclosureKey(projectId: string): string {
  return projectEntryKey(projectId);
}

/**
 * Unfold a project's rows so a row inside it can mount: what every caller
 * that has just created or moved a thread wants. Takes a project id, since
 * that is what such a caller holds.
 */
export function revealProject(projectId: string): void {
  expandProject(projectEntryKey(projectId));
}
