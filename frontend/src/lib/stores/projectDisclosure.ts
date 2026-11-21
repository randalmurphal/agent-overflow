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
  dropDisclosure,
  expandProject,
  getThreadListVisibleLimit,
  isDisclosureAnswered,
  isProjectExpanded,
  setThreadListVisibleLimit,
} from './sidebar.svelte';
import { catalogTreeListed } from './sidebarCatalogs';
import { projectBackend } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';

/** The key the entry a project belongs to is disclosed by. */
export function projectDisclosureKey(projectId: string): string {
  return projectEntryKey(projectId);
}

/**
 * Answer an entry's disclosure from the ids it used to be persisted by,
 * once.
 *
 * Disclosure was per project id, so a user who folded a repository before
 * its second checkout existed has an answer under an id that is no longer a
 * key. This runs only for an entry nothing has answered yet: an entry the
 * user has since folded, unfolded or revealed more of carries its own
 * answer, and an older id must not be able to overrule it. What it reads is
 * the members whose own computer has listed its tree, because a member that
 * arrived before its threads did is not part of what the user is looking at.
 *
 * It answers the entry even when no old id had anything to say, so the
 * entry is answered from the members listed then; an id that turns up later
 * is dropped without effect. Every id it reads is dropped, which is the
 * condition under which this is dead code: no project-id key left.
 */
export function adoptProjectDisclosure(projectId: string): void {
  const key = projectEntryKey(projectId);
  const answered = isDisclosureAnswered(key);
  // Seeded from the key's own state, so an answer already there can only be
  // strengthened: a single-checkout project folded before the upgrade has
  // its answer under what is now the entry's key.
  let collapsed = !isProjectExpanded(key);
  let limit = getThreadListVisibleLimit(key);
  for (const member of projectMembers(projectId)) {
    const id = member.project.id;
    // The representative of an entry with no repository identity is its own
    // key: there is nothing to carry and nothing to drop.
    if (id === key) continue;
    if (!catalogTreeListed(projectBackend(id) ?? HOME_BACKEND)) continue;
    if (!answered) {
      collapsed = collapsed || !isProjectExpanded(id);
      limit = Math.max(limit, getThreadListVisibleLimit(id));
    }
    dropDisclosure(id);
  }
  if (answered) return;
  // Writes the answer and the record that there is one.
  if (collapsed) collapseProject(key);
  else expandProject(key);
  if (limit > THREAD_PREVIEW_LIMIT) setThreadListVisibleLimit(key, limit);
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
