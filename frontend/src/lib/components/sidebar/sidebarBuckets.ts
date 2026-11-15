// Threads and groups bucketed for the sidebar's two sections: the
// assistants' rows, and each project ENTRY's rows.
// Pure: ProjectsSection hands in the stores' rows and its filters, so the
// bucketing is table-drivable from unit tests.
import type { Thread, ThreadGroup } from '../../types/models';
import { threadGroupMatchesQuery, threadMatchesQuery } from './threadSearch';

export interface SidebarBucketsInput {
  threads: readonly Thread[];
  groups: readonly ThreadGroup[];
  /** The search query, trimmed and lowercased; '' for none. */
  query: string;
  /** The sidebar entry a project id belongs to ('' when unknown). */
  entryOf: (projectId: string) => string;
  /** False for a thread on a computer the device filter hides. */
  threadVisible: (thread: Thread) => boolean;
  /** Whether the device filter is on: a group then shows only with a visible member. */
  filterDevices: boolean;
}

export interface SidebarBuckets {
  threadsByProject: Map<string, Thread[]>;
  groupsByProject: Map<string, ThreadGroup[]>;
  /** The Assistants section's rows: the assistants' current threads. */
  assistants: Thread[];
}

/**
 * Whether a thread has a sidebar row at all: not archived, not owned by the
 * thread that spawned it (docs/specs/standing-agents.md ruling 36), and not
 * a mode the sidebar never lists. The catalog leaves owned threads out, but
 * a row reaches the list by other doors too (a frame for a thread that just
 * became owned, a thread noted so a pane can show it), so the rule is
 * applied here, where the list is derived, whatever brought the row.
 */
export function sidebarListsThread(thread: Thread): boolean {
  return !thread.archived && thread.placement !== 'owned' && threadMatchesQuery(thread, '');
}

/**
 * Bucket threads and groups for the sidebar, both filtered by the search
 * query in ONE pass because the two filters are coupled: a group whose NAME
 * matches shows all of its members, including the ones the thread filter
 * would have dropped. A group that neither matches by name nor holds a
 * surviving member is dropped, so search never leaves an empty container
 * behind.
 *
 * An assistant's current thread is its row in the Assistants section,
 * never a project row (docs/specs/standing-agents.md ruling 37). Every
 * other listed thread is a row of its own project; a thread outside every
 * project (an assistant's past thread or helper) has no row.
 */
export function bucketSidebarThreads(input: SidebarBucketsInput): SidebarBuckets {
  const { query } = input;
  const nameMatchedGroupIds = new Set<string>();
  if (query) {
    for (const group of input.groups) {
      if (threadGroupMatchesQuery(group, query)) nameMatchedGroupIds.add(group.id);
    }
  }

  const threadsByProject = new Map<string, Thread[]>();
  const assistants: Thread[] = [];
  const populatedGroupIds = new Set<string>();
  for (const thread of input.threads) {
    if (!sidebarListsThread(thread) || !input.threadVisible(thread)) continue;
    if (thread.agentId) {
      if (threadMatchesQuery(thread, query)) assistants.push(thread);
      continue;
    }
    const key = thread.projectId ? input.entryOf(thread.projectId) : '';
    if (!key) continue;
    const groupId = thread.groupId ?? '';
    // A name-matched group brings its whole membership back.
    const matches = threadMatchesQuery(thread, query) || (groupId !== '' && nameMatchedGroupIds.has(groupId));
    if (!matches) continue;
    if (groupId !== '') populatedGroupIds.add(groupId);
    const bucket = threadsByProject.get(key);
    if (bucket) bucket.push(thread);
    else threadsByProject.set(key, [thread]);
  }

  const groupsByProject = new Map<string, ThreadGroup[]>();
  for (const group of input.groups) {
    if (!group.projectId) continue;
    if (input.filterDevices && !populatedGroupIds.has(group.id)) continue;
    if (query && !nameMatchedGroupIds.has(group.id) && !populatedGroupIds.has(group.id)) continue;
    // Keyed by ENTRY, like the thread buckets: ProjectList looks both up
    // under the entry's representative id, so a member project's groups
    // have to land there or they vanish from a merged entry.
    const key = input.entryOf(group.projectId);
    const bucket = groupsByProject.get(key);
    if (bucket) bucket.push(group);
    else groupsByProject.set(key, [group]);
  }
  return { threadsByProject, groupsByProject, assistants };
}
