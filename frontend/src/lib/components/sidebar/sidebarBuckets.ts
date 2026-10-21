// Threads and groups bucketed for the sidebar's two sections: the
// assistants' cards and what they hold, and each project ENTRY's rows.
// Pure: ProjectsSection hands in the stores' rows and its filters, so the
// bucketing is table-drivable from unit tests.
import type { Thread, ThreadGroup } from '../../types/models';
import { cardOwnerAgentId, nestingAgentThreadId } from '../../utils/sidebarTree';
import { archivedCountsByContainer } from '../../utils/sidebarTreeView';
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
  /** By entry: the assistants whose row in that project holds a thread there. */
  agentsByProject: Map<string, Thread[]>;
  /** The assistants' cards in the Assistants section: their current threads. */
  assistants: Thread[];
  /** What the cards hold: the helpers and past threads of `assistants`. */
  assistantChildren: Thread[];
}

/**
 * Whether an assistant's card is in the sidebar, so it and its rows in
 * projects can nest what lives under it: its current thread, listed (not
 * archived), on a computer the device filter shows (threadVisible), and
 * not a mode the sidebar never lists.
 */
export function sidebarAgentRowShown(agent: Thread, threadVisible: (thread: Thread) => boolean): boolean {
  return Boolean(agent.agentId) && !agent.archived
    && threadVisible(agent) && threadMatchesQuery(agent, '');
}

/**
 * The agent row in the sidebar that nests this thread, if any: its
 * assistant's card for a helper or past thread, its spawner's row in its
 * project for project work (nestingAgentThreadId), when that assistant is
 * shown (sidebarAgentRowShown).
 */
export function sidebarNestingAgent(
  thread: Thread,
  lookup: {
    threadById: (id: string) => Thread | undefined;
    currentThreadIdOf: (agentId: string) => string | undefined;
    threadVisible: (thread: Thread) => boolean;
  },
): Thread | undefined {
  const agentId = nestingAgentThreadId(thread, lookup.currentThreadIdOf);
  const agent = agentId ? lookup.threadById(agentId) : undefined;
  return agent && sidebarAgentRowShown(agent, lookup.threadVisible) ? agent : undefined;
}

/**
 * Bucket threads and groups for the sidebar, both filtered by the search
 * query in ONE pass because the two filters are coupled: a group whose NAME
 * matches shows all of its members, including the ones the thread filter
 * would have dropped. A group that neither matches by name nor holds a
 * surviving member is dropped, so search never leaves an empty container
 * behind.
 *
 * An assistant's current thread is its card in the Assistants section,
 * never a project row (docs/specs/standing-agents.md ruling 37); the card
 * shows while it matches the query or holds a thread that does. Its
 * helpers and past threads list on the card (cardOwnerAgentId). Project
 * work stays in its own project, under the assistant's row there when it
 * is nested; the row also shows for archived children alone. While the
 * assistant is not shown, its project work and its past threads in a
 * project list as plain rows of their project, and a thread outside every
 * project is not shown.
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
  // The assistants' rows by thread id, and each assistant's current
  // thread, for sidebarNestingAgent.
  const agents = new Map<string, Thread>();
  const currentThreadOf = new Map<string, string>();
  for (const thread of input.threads) {
    if (!thread.agentId) continue;
    agents.set(thread.id, thread);
    if (!currentThreadOf.has(thread.agentId)) currentThreadOf.set(thread.agentId, thread.id);
  }
  const nesting = {
    threadById: (id: string) => agents.get(id),
    currentThreadIdOf: (agentId: string) => currentThreadOf.get(agentId),
    threadVisible: input.threadVisible,
  };

  const threadsByProject = new Map<string, Thread[]>();
  const agentsByProject = new Map<string, Thread[]>();
  const holdAgent = (key: string, agent: Thread) => {
    const held = agentsByProject.get(key);
    if (!held) agentsByProject.set(key, [agent]);
    else if (!held.includes(agent)) held.push(agent);
  };
  const assistantChildren: Thread[] = [];
  // The shown assistants, and those a matching card child keeps on screen.
  const shownAgents: Thread[] = [];
  const cardsKept = new Set<string>();
  const populatedGroupIds = new Set<string>();
  for (const thread of input.threads) {
    if (!listed(thread)) continue;
    if (thread.agentId) {
      if (sidebarAgentRowShown(thread, input.threadVisible)) shownAgents.push(thread);
      continue;
    }
    const agent = sidebarNestingAgent(thread, nesting);
    if (agent && cardOwnerAgentId(thread)) {
      if (!threadMatchesQuery(thread, query)) continue;
      assistantChildren.push(thread);
      cardsKept.add(agent.id);
      continue;
    }
    const key = thread.projectId ? input.entryOf(thread.projectId) : '';
    if (!key) continue;
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
    if (agent) holdAgent(key, agent);
  }
  // A row in a project shows while it has a child there, live or done, so
  // its archived children alone keep it: its done footer lists them. A
  // search shows only the rows holding a matching thread.
  if (!query) {
    for (const agent of shownAgents) {
      for (const scope of archivedCountsByContainer(agent, input.entryOf).keys()) {
        if (scope !== '') holdAgent(scope, agent);
      }
    }
  }
  const assistants = shownAgents.filter((agent) => cardsKept.has(agent.id) || threadMatchesQuery(agent, query));

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
  return { threadsByProject, groupsByProject, agentsByProject, assistants, assistantChildren };
}
