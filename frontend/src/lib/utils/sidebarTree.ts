// Sidebar thread tree: the BUILD half — node shapes, the sort comparator,
// status bubbling, and buildSidebarThreadTree itself. The VIEW half (flatten,
// the preview cut and the identity cutoffs) lives in `sidebarTreeView.ts` and
// imports from here; nothing imports back.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.
//
// Sort order (highest first):
//   0. Drafts, newest first.
//   1. Groups and standing agents, a section of their own at the top level.
//      Neither is pinnable; within the section they take the normal
//      comparator below.
//   2. Pinned front-burner block, then pinned back-burner block.
//      Within each block: the normal status/activity/id comparator below.
//      A group's members pin the same way inside their group.
//   3. needs-attention (error / pending-approval / awaiting-input / plan-ready / interrupted).
//   4. running (any mode — Working / Planning) and completed (idle + unread).
//   5. paused (reserved for future; no current source emits it).
//   6. idle + read (no pill).
// Within each tier (after pinned): latestActivityAt desc, then thread.id
// localeCompare for stability.
//
// Every thread renders flat: at the top level, as a direct member of a
// group, or under the standing agent that spawned it. Members bubble:
//   - displayStatus: the group row shows its most important member's status;
//     an agent row shows the most important of its own and its children's.
//   - latestActivityAt: a group's activity is its newest member's, so a
//     group with a freshly-active member surfaces in the sort. An agent's
//     is the newest of its own and its children's.
//
// A standing agent (docs/specs/standing-agents.md, "Sidebar"): an
// assistant's current thread (`agentId`) is an agent node, and its children
// are the live threads whose `spawnedByThreadId` names it, unless they are
// assistants themselves, and its past threads that are not archived. The backend keeps an agent and its children out of groups; a
// `groupId` or pin left on an agent's row is ignored here. An agent whose
// row is not in the list (another project, archived) nests nothing: its
// spawned threads render as plain rows.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  resolveEffectiveThreadStatus,
  lastKnownThreadStatusPill,
  resolveThreadStatusPill,
  type ThreadStatusPill,
} from './threadStatusPill';
import { isHiddenThreadMode } from './threadModes';

export type ThreadStatusSortGroup =
  | 'pinned'
  | 'needs-attention'
  | 'running'
  | 'paused'
  | 'completed'
  | 'idle';

export type NormalThreadStatusSortGroup = Exclude<ThreadStatusSortGroup, 'pinned'>;
export type SidebarPinGroup = 'front' | 'back' | null;

// Running and completed rows share the non-blocking activity tier so
// their relative order stays activity-driven. Plain idle/read rows sit
// below them because they have no status indicator worth surfacing.
export const SORT_GROUP_PRIORITY: Record<ThreadStatusSortGroup, number> = {
  pinned: 3,
  'needs-attention': 2,
  running: 1,
  paused: 1,
  completed: 1,
  idle: 0,
};

// Within-group ordering of live statuses. Used by displayStatus bubbling
// to pick the most important member status, NOT by the tree compare
// (which collapses all needs-attention rows into one tier and sorts by
// activity timestamp). Higher number = higher priority.
const STATUS_PRIORITY: Record<ThreadLiveStatus, number> = {
  error: 100,
  'pending-approval': 90,
  'awaiting-input': 80,
  // A worktree whose setup did not finish is a concrete repair the user
  // has to run, so it outranks the two informational fallbacks below it —
  // the same ordering resolveEffectiveThreadStatus applies.
  'setup-failed': 70,
  'plan-ready': 60,
  running: 50,
  interrupted: 40,
  idle: 0,
};

/**
 * Fields every sidebar row carries, whatever it is a row FOR. A group has
 * no status of its own — `ownLiveStatus` is always 'idle' and `ownStatus`
 * null — but it does carry a bubbled display status, a sort tier and an
 * activity timestamp, because those are exactly what the sort reads
 * inside the group section.
 */
export interface SidebarTreeNodeBase {
  /** 0 at the top level, 1 for a group member. */
  depth: number;
  /** Live status for this row alone; always 'idle' for a group. */
  ownLiveStatus: ThreadLiveStatus;
  /** Pill for ownLiveStatus. Null when the row would render no pill. */
  ownStatus: ThreadStatusPill | null;
  /**
   * The status the row shows: a thread's own, or a group's most important
   * member's.
   */
  displayLiveStatus: ThreadLiveStatus;
  /**
   * Pill for displayLiveStatus. A group reuses the contributing member's
   * pill verbatim so mode-derived labels (Planning) read accurately.
   */
  displayStatus: ThreadStatusPill | null;
  sortGroup: ThreadStatusSortGroup;
  /** Status tier without the pin override; orders rows inside a pin block. */
  normalSortGroup: NormalThreadStatusSortGroup;
  /**
   * A thread's own activity; a group's newest member activity, or its own
   * updatedAt while it is empty. Unix ms.
   */
  latestActivityAt: number;
}

export interface SidebarThreadTreeNode extends SidebarTreeNodeBase {
  kind: 'thread';
  thread: Thread;
}

export interface SidebarGroupTreeNode extends SidebarTreeNodeBase {
  kind: 'group';
  group: ThreadGroup;
  /** The group's members, sorted. */
  children: SidebarThreadTreeNode[];
}

export interface SidebarAgentTreeNode extends SidebarTreeNodeBase {
  kind: 'agent';
  /** The standing agent's own thread. */
  thread: Thread;
  /** The live threads it spawned, sorted. */
  children: SidebarThreadTreeNode[];
}

/**
 * A sidebar row: a thread, a thread group (a named container whose top row
 * is not a thread), or a standing agent (a thread whose row also holds the
 * threads it spawned). Discriminate on `kind` — never on the presence of a
 * field.
 */
export type SidebarTreeNode = SidebarThreadTreeNode | SidebarGroupTreeNode | SidebarAgentTreeNode;

/** An assistant's current thread: its row is an agent node. */
export function isStandingAgent(thread: Pick<Thread, 'agentId'>): boolean {
  return Boolean(thread.agentId);
}

/**
 * The key of an agent row's sidebar state (its fold, its done footer): the
 * assistant's id, so a handoff to a new thread keeps it.
 */
export function assistantKey(thread: Pick<Thread, 'id' | 'agentId'>): string {
  return thread.agentId || thread.id;
}

export interface BuildSidebarThreadTreeInput {
  threads: readonly Thread[];
  /**
   * The project's groups. A thread whose `groupId` names one of them
   * becomes that group's member; a `groupId` naming a group that is not
   * here (deleted, or filtered out by search) leaves the thread at the top
   * level rather than dropping it. Groups with no members still render —
   * an empty group persists until it is deleted.
   */
  groups?: readonly ThreadGroup[];
  statusOf?: (thread: Thread) => ThreadLiveStatus;
  liveStatusOf?: (threadId: string) => ThreadLiveStatus;
  /** The standing agent a thread's prompts went to, or ''. */
  waitingOnOf?: (threadId: string) => string;
  /**
   * Newest activity timestamp for a thread. Callers pass the threads
   * store's getThreadLiveActivityAt so streaming bumps (which live in a
   * per-thread box, not on the row) still drive the activity sort.
   * Defaults to the row's own updatedAt.
   */
  activityOf?: (thread: Thread) => number;
  /**
   * The name of the thread's computer while it is unreachable, else null.
   * Its status is the last one reported and is shown still and attributed
   * (lastKnownThreadStatusPill), in its own row and wherever it bubbles to.
   */
  unreachableMachineOf?: (thread: Thread) => string | null;
}

/**
 * statusPriority — idle collapses to 0 (lowest) so an idle+read member
 * loses the bubble check against any member that has any pill. Exported for
 * `sidebarTreeView.ts`'s rollup, which ranks the same statuses.
 */
export function statusPriority(status: ThreadLiveStatus): number {
  return STATUS_PRIORITY[status] ?? 0;
}

function getNormalStatusSortGroup(
  liveStatus: ThreadLiveStatus,
  status: ThreadStatusPill | null,
): NormalThreadStatusSortGroup {
  switch (liveStatus) {
    case 'error':
    case 'pending-approval':
    case 'awaiting-input':
    case 'plan-ready':
    case 'setup-failed':
    case 'interrupted':
      return 'needs-attention';
    case 'running':
      return 'running';
    case 'idle':
      return status === null ? 'idle' : 'completed';
    default:
      // Defensive fallback — a future ThreadLiveStatus enum member
      // should not silently land in `undefined`. Tiering as idle keeps
      // it visible without claiming attention it hasn't earned.
      return 'idle';
  }
}

function getStatusSortGroup(
  thread: Thread,
  liveStatus: ThreadLiveStatus,
  status: ThreadStatusPill | null,
): ThreadStatusSortGroup {
  if (thread.pinnedAt != null) return 'pinned';
  return getNormalStatusSortGroup(liveStatus, status);
}

export function sidebarPinGroup(thread: Pick<Thread, 'pinnedAt' | 'pinGroup'>): SidebarPinGroup {
  if (thread.pinnedAt == null) return null;
  return thread.pinGroup === 1 ? 'back' : 'front';
}

/** Stable id for a node of any kind — the each-block key and sort tie-break. */
export function sidebarTreeNodeId(node: SidebarTreeNode): string {
  return node.kind === 'group' ? node.group.id : node.thread.id;
}

/**
 * A group is never pinned, and an agent renders in the groups section
 * whatever pin its thread carries; a thread node's burner is its thread's.
 */
export function sidebarNodePinGroup(node: SidebarTreeNode): SidebarPinGroup {
  return node.kind === 'thread' ? sidebarPinGroup(node.thread) : null;
}

/**
 * The top-level section a node sorts and renders in. Drafts sit above
 * every section and belong to none, so they draw no section divider.
 */
export type SidebarSection = 'groups' | 'front' | 'back' | 'unpinned';

export function sidebarNodeSection(node: SidebarTreeNode): SidebarSection | null {
  if (isDraftNode(node)) return null;
  if (node.kind !== 'thread') return 'groups';
  return sidebarNodePinGroup(node) ?? 'unpinned';
}

/**
 * A thread node the user is actively composing. Groups are never drafts,
 * and an agent has run (it was promoted from a conversation).
 */
export function isDraftNode(node: SidebarTreeNode): boolean {
  return node.kind === 'thread' && node.thread.isDraft === true;
}

/**
 * The status a container row shows: its most important member's, pill
 * included, ranked by status tier and then by live-status priority. The
 * tier ignores pins: a pinned idle member must not mask a running one. An
 * agent passes its own node among the members, so its own status competes.
 */
function groupDisplay(
  members: readonly SidebarThreadTreeNode[],
): { displayLiveStatus: ThreadLiveStatus; displayStatus: ThreadStatusPill | null } {
  let top: SidebarThreadTreeNode | null = null;
  let topGroupPriority = -1;
  let topPriority = -1;
  for (const member of members) {
    const groupPriority = SORT_GROUP_PRIORITY[member.normalSortGroup];
    const priority = statusPriority(member.displayLiveStatus);
    if (groupPriority > topGroupPriority || (groupPriority === topGroupPriority && priority > topPriority)) {
      topGroupPriority = groupPriority;
      topPriority = priority;
      top = member;
    }
  }
  if (!top) return { displayLiveStatus: 'idle', displayStatus: null };
  return { displayLiveStatus: top.displayLiveStatus, displayStatus: top.displayStatus };
}

/**
 * compareTreeNodes — drafts pinned to the very top (newest createdAt
 * first), then groups, then front/back pin blocks (normal comparator
 * within each), then sort group, activity desc, and id for stability.
 * Drafts outrank everything because the user is actively composing them
 * and needs them surfaced regardless of pin state.
 */
function compareTreeNodes(left: SidebarTreeNode, right: SidebarTreeNode): number {
  // Drafts are a THREAD concept: a group is a container the user curates,
  // never something being composed, so it never wins the draft block.
  const leftDraft = isDraftNode(left);
  const rightDraft = isDraftNode(right);
  if (leftDraft !== rightDraft) return leftDraft ? -1 : 1;
  if (leftDraft && rightDraft && left.kind === 'thread' && right.kind === 'thread') {
    if (right.thread.createdAt !== left.thread.createdAt) {
      return right.thread.createdAt > left.thread.createdAt ? 1 : -1;
    }
    if (left.thread.id === right.thread.id) return 0;
    return left.thread.id < right.thread.id ? 1 : -1;
  }

  // Groups and agents form their own section above the pin blocks. Only
  // the top level holds them, so inside one this never decides.
  const leftContainer = left.kind !== 'thread';
  const rightContainer = right.kind !== 'thread';
  if (leftContainer !== rightContainer) return leftContainer ? -1 : 1;

  const leftPinGroup = sidebarNodePinGroup(left);
  const rightPinGroup = sidebarNodePinGroup(right);
  if (leftPinGroup !== rightPinGroup) {
    if (leftPinGroup === null) return 1;
    if (rightPinGroup === null) return -1;
    return leftPinGroup === 'front' ? -1 : 1;
  }

  const leftSortGroup = leftPinGroup === null ? left.sortGroup : left.normalSortGroup;
  const rightSortGroup = rightPinGroup === null ? right.sortGroup : right.normalSortGroup;
  const groupCmp = SORT_GROUP_PRIORITY[rightSortGroup] - SORT_GROUP_PRIORITY[leftSortGroup];
  if (groupCmp !== 0) return groupCmp;

  if (right.latestActivityAt !== left.latestActivityAt) {
    return right.latestActivityAt > left.latestActivityAt ? 1 : -1;
  }

  // Lexicographic tie-break for stability. Plain `<` / `>` is faster
  // than localeCompare for ASCII UUIDs (the only id source) and the
  // ordering only needs to be stable, not locale-correct.
  const leftId = sidebarTreeNodeId(left);
  const rightId = sidebarTreeNodeId(right);
  if (leftId === rightId) return 0;
  return leftId < rightId ? 1 : -1;
}

/**
 * buildSidebarThreadTree — build one node per visible thread, file each
 * into its group or under its agent, and sort each level.
 *
 * Groups: a thread whose `groupId` names one of `input.groups` is built at
 * depth 1 under that group's node instead of at the top level. Group nodes
 * sort above every top-level thread and among themselves by the normal
 * comparator, so a group with a running member rises within the group
 * section. Members sort by the same comparator, their own pins included.
 *
 * Agents: an assistant's current thread is an agent node in the same
 * section, and a thread it spawned or a past thread of the assistant is
 * built at depth 1 under it, before any group its `groupId` could name. An
 * assistant never nests under the assistant that spawned it: it is an agent
 * row of its own.
 */
export function buildSidebarThreadTree(input: BuildSidebarThreadTreeInput): SidebarTreeNode[] {
  const visibleThreads = input.threads.filter((thread) => !isHiddenThreadMode(thread.mode));

  const buildThreadNode = (thread: Thread, depth: number): SidebarThreadTreeNode => {
    const ownLiveStatus = input.statusOf
      ? input.statusOf(thread)
      : resolveEffectiveThreadStatus(thread, input.liveStatusOf?.(thread.id) ?? 'idle');
    const resolvedPill = resolveThreadStatusPill(thread, ownLiveStatus, input.waitingOnOf?.(thread.id));
    const unreachableMachine = input.unreachableMachineOf?.(thread) ?? null;
    const ownPill = unreachableMachine !== null ? lastKnownThreadStatusPill(resolvedPill, unreachableMachine) : resolvedPill;
    return {
      kind: 'thread',
      thread,
      depth,
      ownLiveStatus,
      ownStatus: ownPill,
      displayLiveStatus: ownLiveStatus,
      displayStatus: ownPill,
      sortGroup: getStatusSortGroup(thread, ownLiveStatus, ownPill),
      normalSortGroup: getNormalStatusSortGroup(ownLiveStatus, ownPill),
      latestActivityAt: input.activityOf ? input.activityOf(thread) : (thread.updatedAt ?? 0),
    };
  };

  const buildGroupNode = (group: ThreadGroup, members: SidebarThreadTreeNode[]): SidebarGroupTreeNode => {
    const display = groupDisplay(members);
    let latestActivityAt = 0;
    for (const member of members) {
      if (member.latestActivityAt > latestActivityAt) latestActivityAt = member.latestActivityAt;
    }
    return {
      kind: 'group',
      group,
      depth: 0,
      children: members,
      ownLiveStatus: 'idle',
      ownStatus: null,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      normalSortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      // An empty group has nothing to bubble, so it sorts on its own last
      // write — which is when it was created or renamed.
      latestActivityAt: members.length === 0 ? (group.updatedAt ?? 0) : latestActivityAt,
    };
  };

  const buildAgentNode = (agent: Thread, children: SidebarThreadTreeNode[]): SidebarAgentTreeNode => {
    const own = buildThreadNode(agent, 0);
    const display = groupDisplay([own, ...children]);
    let latestActivityAt = own.latestActivityAt;
    for (const child of children) {
      if (child.latestActivityAt > latestActivityAt) latestActivityAt = child.latestActivityAt;
    }
    return {
      kind: 'agent',
      thread: agent,
      depth: 0,
      children,
      ownLiveStatus: own.ownLiveStatus,
      ownStatus: own.ownStatus,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      normalSortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      latestActivityAt,
    };
  };

  const groups = input.groups ?? [];
  const agents = visibleThreads.filter(isStandingAgent);
  if (groups.length === 0 && agents.length === 0) {
    return visibleThreads.map((t) => buildThreadNode(t, 0)).sort(compareTreeNodes);
  }

  const groupsById = new Map(groups.map((group) => [group.id, group] as const));
  const agentIds = new Set(agents.map((agent) => agent.id));
  const currentThreadOf = new Map(agents.map((agent) => [agent.agentId as string, agent.id] as const));
  const membersByGroup = new Map<string, Thread[]>();
  const childrenByAgent = new Map<string, Thread[]>();
  const nodes: SidebarTreeNode[] = [];
  for (const thread of visibleThreads) {
    if (agentIds.has(thread.id)) continue;
    const spawner = thread.pastAgentId ? currentThreadOf.get(thread.pastAgentId) : thread.spawnedByThreadId;
    if (spawner && agentIds.has(spawner)) {
      const bucket = childrenByAgent.get(spawner);
      if (bucket) bucket.push(thread);
      else childrenByAgent.set(spawner, [thread]);
      continue;
    }
    const groupId = thread.groupId;
    if (groupId && groupsById.has(groupId)) {
      const bucket = membersByGroup.get(groupId);
      if (bucket) bucket.push(thread);
      else membersByGroup.set(groupId, [thread]);
      continue;
    }
    nodes.push(buildThreadNode(thread, 0));
  }
  for (const group of groups) {
    const members = (membersByGroup.get(group.id) ?? [])
      .map((thread) => buildThreadNode(thread, 1))
      .sort(compareTreeNodes);
    nodes.push(buildGroupNode(group, members));
  }
  for (const agent of agents) {
    const children = (childrenByAgent.get(agent.id) ?? [])
      .map((thread) => buildThreadNode(thread, 1))
      .sort(compareTreeNodes);
    nodes.push(buildAgentNode(agent, children));
  }
  return nodes.sort(compareTreeNodes);
}
