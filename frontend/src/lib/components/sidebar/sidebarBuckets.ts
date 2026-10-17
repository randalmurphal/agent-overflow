// Threads and groups bucketed by sidebar ENTRY for the project lists. Pure:
// ProjectsSection hands in the stores' rows and its filters, so the
// bucketing is table-drivable from unit tests.
import type { Thread, ThreadGroup } from '../../types/models';
import { nestingAgentThreadId } from '../../utils/sidebarTree';
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
}

/**
 * Bucket threads and groups by sidebar entry, both filtered by the search
 * query in ONE pass because the two filters are coupled: a group whose NAME
 * matches shows all of its members, including the ones the thread filter
 * would have dropped.
 *
 * Project-less threads have no sidebar surface and are skipped. A group
 * that neither matches by name nor holds a surviving member is dropped, so
 * search never leaves an empty container behind.
 *
 * A thread a standing agent spawned into another project (or a past thread
 * of an assistant whose current thread sits in another project) is filed
 * under the agent's entry, so the agent row there nests it and its own
 * project's list does not show it (docs/specs/standing-agents.md ruling 4).
 * The agent must be in the sidebar for that: not archived, and on a
 * computer the device filter shows. Otherwise the thread stays in its own
 * project, as a plain row.
 */
export function bucketSidebarThreads(input: SidebarBucketsInput): SidebarBuckets {
  const { query } = input;
  const nameMatchedGroupIds = new Set<string>();
  if (query) {
    for (const group of input.groups) {
      if (threadGroupMatchesQuery(group, query)) nameMatchedGroupIds.add(group.id);
    }
  }

  const listed = (thread: Thread) => !thread.archived && input.threadVisible(thread);
  // The entry of every agent row the sidebar shows, by its thread id, and
  // each assistant's current thread, for its past threads.
  const agentEntries = new Map<string, string>();
  const currentThreadOf = new Map<string, string>();
  for (const thread of input.threads) {
    if (!thread.agentId || !thread.projectId || !listed(thread) || !threadMatchesQuery(thread, '')) continue;
    const entry = input.entryOf(thread.projectId);
    if (!entry) continue;
    agentEntries.set(thread.id, entry);
    currentThreadOf.set(thread.agentId, thread.id);
  }

  const threadsByProject = new Map<string, Thread[]>();
  const populatedGroupIds = new Set<string>();
  for (const thread of input.threads) {
    if (!listed(thread)) continue;
    let key = thread.projectId ? input.entryOf(thread.projectId) : '';
    if (!key) continue;
    const spawner = nestingAgentThreadId(thread, (agentId) => currentThreadOf.get(agentId));
    const agentEntry = spawner ? agentEntries.get(spawner) : undefined;
    if (agentEntry) key = agentEntry;
    const groupId = thread.groupId ?? '';
    // A name-matched group brings its whole membership back. The
    // empty-query call is the "is this row eligible at all" test: it still
    // excludes workflow-owned modes.
    const matches = threadMatchesQuery(thread, query)
      || (groupId !== '' && nameMatchedGroupIds.has(groupId) && threadMatchesQuery(thread, ''));
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
  return { threadsByProject, groupsByProject };
}
