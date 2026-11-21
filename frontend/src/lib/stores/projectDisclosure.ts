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
import {
  projectEntryKey,
  projectMembers,
} from './projects.svelte';
import {
  collapseProject,
  collapseThreadList,
  expandProject,
  getThreadListVisibleLimit,
  isProjectExpanded,
  setThreadListVisibleLimit,
} from './sidebar.svelte';

/** The key the entry a project belongs to is disclosed by. */
export function projectDisclosureKey(projectId: string): string {
  return projectEntryKey(projectId);
}

/**
 * Carry what a member id said about disclosure over to its entry's key,
 * once, and consume the ids it read.
 *
 * Disclosure used to be persisted per project id, so a user who collapsed a
 * repository before its second checkout existed has an answer under an id
 * that is no longer a key. Each member id is read, written under the entry's
 * key, and removed; an answer already under the entry's key wins, because it
 * is the newer one.
 *
 * This reads nothing and writes nothing once no project-id key is left in
 * storage, which is the condition under which it is dead code.
 */
export function adoptProjectDisclosure(projectId: string): void {
  const key = projectEntryKey(projectId);
  for (const member of projectMembers(projectId)) {
    const id = member.project.id;
    // The representative of an entry with no repository identity is its own
    // key: there is nothing to carry and nothing to consume.
    if (id === key) continue;
    if (!isProjectExpanded(id)) {
      expandProject(id);
      collapseProject(key);
    }
    const limit = getThreadListVisibleLimit(id);
    collapseThreadList(id);
    if (limit > getThreadListVisibleLimit(key)) setThreadListVisibleLimit(key, limit);
  }
}

/**
 * Unfold a project's rows so a row inside it can mount: what every caller
 * that has just created or moved a thread wants. Takes a project id, since
 * that is what such a caller holds, and carries any answer still under it.
 */
export function revealProject(projectId: string): void {
  adoptProjectDisclosure(projectId);
  expandProject(projectEntryKey(projectId));
}
