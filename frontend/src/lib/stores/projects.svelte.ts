import { invalidateReplicaCatalog } from '../replica/session';
import { computerCatalogWriter } from './computerCatalogWriter';
import { computerCatalog, retainUnavailableComputerRows, type CatalogPart, type ComputerRows } from './computerRows';
import { settleCatalogAnswers } from './catalogLoad.svelte';
// Sidebar-facing projects store. Mirrors the pattern of threads.svelte.ts:
// a single reactive $state array driven by an explicit read, with
// optimistic local mutations so the sidebar can reflect a create/rename/
// delete without round-tripping the server.
//
// The rows are read only as part of the sidebar's tree
// (./sidebarCatalogs.ts, projectCatalogPart below); callers call the
// addProjectLocal / updateProjectLocal / removeProjectLocal helpers after a
// successful RPC so the list stays in sync with the backend.

import type { Project, ProjectWithCounts } from '../types/models';
import { ListProjects, UpdateProjectSortPositions } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import {
  disambiguatedPathLabels,
  formatPathLabel,
} from '../utils/pathDisplay';
import { groupRepositories } from '../utils/repoKey';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { projectBackend, noteProject } from '../transport/entityIndex';
import { onBackendDetached } from '../transport/backends';
import { hasMultipleBackends } from './attachedBackends.svelte';

let projects: ProjectWithCounts[] = $state([]);
let loaded = $state(false);

// Per-project live-activity bumps (streaming beats). Kept out of the
// `projects` array signal for the same reason as the threads store's
// liveActivityAt box: a bump is a field patch, and rewriting the array
// re-sorted and re-rendered the whole sidebar on every streamed item.
const liveActivityAt = createKeyedSignalRegistry<number>(0);

/** Read-only view of the current project list for consumers. */
export function getProjects(): readonly ProjectWithCounts[] {
  return projects;
}

// Indexed, not scanned: a project row is looked up per rendered row (its
// entry's members) and per entry of several projections, so a scan here made
// the cost of showing the list quadratic in its length.
const rowById = $derived.by(() => new Map(projects.map((row) => [row.project.id, row])));

/** Lookup helper. Returns undefined when the id isn't in the store. */
export function getProject(id: string): ProjectWithCounts | undefined {
  return rowById.get(id);
}

// ---------------------------------------------------------------------------
// Merged entries (remote-access §10, wave 7d)
// ---------------------------------------------------------------------------
//
// A project is a repository, and the same repository checked out on two
// attached machines is ONE sidebar entry with two targets. The rows stay as
// the backends sent them (the entity index still answers which machine
// owns each id); what merges is the VIEW. Grouping is `groupRepositories`:
// at most one member per machine, so a second clone on one machine stays
// its own entry. An entry is represented by its live home member when there
// is one, else its first live member, so a person's own machine is the one
// whose name, colour and sort position the entry wears.
//
// Computed only while more than one backend is attached: a single-backend
// app returns the list itself, same array identity, and pays nothing.

interface MergedProjects {
  entries: readonly ProjectWithCounts[];
  /** member project id → the entry (representative) id it belongs to. */
  entryOf: Map<string, string>;
  /** representative id → every member, home first. */
  members: Map<string, ProjectWithCounts[]>;
  /** member project id → the repository key its entry merged on. */
  repoKeyOf: Map<string, string>;
}

const NO_MERGE: Pick<MergedProjects, 'entryOf' | 'members' | 'repoKeyOf'> = {
  entryOf: new Map(), members: new Map(), repoKeyOf: new Map(),
};

function computerOf(projectId: string): BackendKey {
  return projectBackend(projectId) ?? HOME_BACKEND;
}

/** Groups rows by repository, `computers` overriding the entity index. */
function groupRows<T extends { project: Project }>(rows: readonly T[], computers?: Map<T, BackendKey>) {
  const wrapped = rows.map((row) => ({ row, project: row.project, computer: computers?.get(row) ?? computerOf(row.project.id) }));
  const groups = groupRepositories(wrapped);
  const keyOf = new Map<string, string>();
  for (const item of wrapped) keyOf.set(item.project.id, groups.keyOf.get(item) ?? '');
  const members = [...groups.members.values()].map((items) => items.map((item) => item.row));
  return { keyOf, members };
}

/**
 * The member of an entry that wears it: its home member when it has one,
 * else the first in the order given, and never an archived member while a
 * live one is there (the sidebar hides an entry by its representative's
 * archived flag). Home first and then the given order is what keeps an
 * entry from swapping representative when a computer reconnects; the
 * sidebar elects over the members whose tree is listed, so for it the
 * representative can change only at a listing
 * (components/sidebar/ProjectsSection.svelte).
 */
export function representativeOf(members: readonly ProjectWithCounts[]): ProjectWithCounts {
  const home = members.filter((row) => computerOf(row.project.id) === HOME_BACKEND);
  const ordered = home.length === 0 || home.length === members.length
    ? members
    : [...home, ...members.filter((row) => computerOf(row.project.id) !== HOME_BACKEND)];
  return ordered.find((row) => !row.project.archived) ?? ordered[0];
}

// The key a sidebar entry keeps while which of its members represents it
// may change: the each key, so a representative change neither remounts the
// row nor loses the animation's identity.
//
// `groupRepositories` keeps one checkout per computer in a verified
// repository, so every member of an entry shares its repository key while a
// second checkout on the same computer is an entry of its own under that
// same key. The chosen checkout of a computer therefore wears the
// repository key and every other row wears its project id, which is unique
// whatever is attached: the choosing is per computer, so a key cannot
// change when a computer attaches, detaches or lists. Computed with one
// computer exactly as with several, because what it keys is persisted and
// rendered state, not the merge.
const entryKeys = $derived.by(() => {
  const wrapped = projects.map((row) => ({ project: row.project, computer: computerOf(row.project.id) }));
  const keys = new Map(wrapped.map((item) => [item.project.id, item.project.id]));
  for (const [key, chosen] of groupRepositories(wrapped).members) {
    for (const item of chosen) keys.set(item.project.id, key);
  }
  return keys;
});

/** The key the sidebar's each block gives the entry `projectId` renders as. */
export function projectEntryKey(projectId: string): string {
  return entryKeys.get(projectId) ?? projectId;
}

/**
 * Group `rows` into one entry per repository: at most one member per
 * computer, the entry worn by its home member when there is one, and the
 * thread count and activity summed over its members.
 *
 * Every row the client holds, so an entry's identity and representative do
 * not depend on what a surface shows. A surface that shows only some members
 * aggregates those itself over these entries; the sidebar does
 * (components/sidebar/ProjectsSection.svelte).
 */
function mergeProjectRows(rows: readonly ProjectWithCounts[]): MergedProjects {
  if (!hasMultipleBackends()) return { entries: rows, ...NO_MERGE };
  const { keyOf, members: groups } = groupRows(rows);
  const position = new Map(rows.map((row, index) => [row, index]));
  const repoKeyOf = new Map<string, string>();
  const groupOf = new Map<ProjectWithCounts, ProjectWithCounts[]>();
  for (const group of groups) {
    if (group.length < 2) continue;
    group.sort((a, b) => Number(computerOf(b.project.id) === HOME_BACKEND) - Number(computerOf(a.project.id) === HOME_BACKEND)
      || position.get(a)! - position.get(b)!);
    for (const row of group) {
      groupOf.set(row, group);
      repoKeyOf.set(row.project.id, keyOf.get(row.project.id)!);
    }
  }
  const entryOf = new Map<string, string>();
  const members = new Map<string, ProjectWithCounts[]>();
  const entries: ProjectWithCounts[] = [];
  for (const row of rows) {
    const group = groupOf.get(row);
    if (!group) {
      entries.push(row);
      continue;
    }
    if (entryOf.has(row.project.id)) continue;
    const rep = representativeOf(group);
    let threadCount = 0;
    let lastActive = 0;
    for (const member of group) {
      threadCount += member.threadCount;
      lastActive = Math.max(lastActive, member.lastActive ?? 0);
      entryOf.set(member.project.id, rep.project.id);
    }
    members.set(rep.project.id, group);
    entries.push(
      threadCount === rep.threadCount && lastActive === (rep.lastActive ?? 0)
        ? rep
        : { ...rep, threadCount, lastActive },
    );
  }
  return { entries, entryOf, members, repoKeyOf };
}

const merged = $derived.by(() => mergeProjectRows(projects));

/** The repository key of the merged entry `projectId` is a member of; ''
 *  when it is in no merged entry. */
export function projectRepoKey(projectId: string): string {
  return merged.repoKeyOf.get(projectId) ?? '';
}

/**
 * Whether a checkout with `identity` on `computer` is the same repository as
 * `projectId`, by the rule entries merge on. False when `projectId` has no
 * identity: nothing proves a folder is "the same" plain directory.
 */
export function checkoutMatchesProject(
  projectId: string,
  computer: BackendKey,
  identity: Pick<Project, 'repositoryID'>,
): boolean {
  const candidate: ProjectWithCounts = {
    project: {
      id: '\u0000checkout', path: '', name: '', sortPosition: 0,
      createdAt: Number.MAX_SAFE_INTEGER, updatedAt: 0, archived: false, ...identity,
    },
    threadCount: 0,
  };
  const { keyOf } = groupRows([...projects, candidate], new Map([[candidate, computer]]));
  const key = keyOf.get(projectId) ?? '';
  return key !== '' && keyOf.get(candidate.project.id) === key;
}

/** The sidebar's list: one row per repository across attached machines. */
export function projectEntries(): readonly ProjectWithCounts[] {
  return merged.entries;
}

/** The entry a project id renders under: itself unless merged into another. */
export function entryIdFor(projectId: string): string {
  return merged.entryOf.get(projectId) ?? projectId;
}

/** Every project row merged into the entry `projectId` belongs to (1 when unmerged). */
export function projectMembers(projectId: string): readonly ProjectWithCounts[] {
  const rows = merged.members.get(entryIdFor(projectId));
  if (rows) return rows;
  const own = getProject(projectId);
  return own ? [own] : [];
}

/** Whether the entry holding `projectId` has members on more than one machine. */
export function projectSpansBackends(projectId: string): boolean {
  return merged.members.has(entryIdFor(projectId));
}

/** The member of `projectId`'s entry that lives on `backend`, if any. */
export function projectSiblingOn(projectId: string, backend: BackendKey): ProjectWithCounts | undefined {
  for (const row of projectMembers(projectId)) {
    if ((projectBackend(row.project.id) ?? HOME_BACKEND) === backend) return row;
  }
  return undefined;
}

// Labels for unfiltered project surfaces. Filtered lists derive their own
// labels with disambiguatedPathLabels so hidden rows cannot force prefixes.
// Unique names label as-is; duplicates gain distinguishing parent dirs.
// One shared $derived so the map is computed once per list change. Over
// the ENTRIES, not the rows: two members of one repo share a name by
// definition and are not a collision. A member that is not the
// representative labels as its representative.
const projectLabels = $derived.by(() => {
  const active: Project[] = [];
  const archived: Project[] = [];
  for (const { project } of merged.entries) (project.archived ? archived : active).push(project);
  // Active pickers and the archive list are separate surfaces.
  return new Map([...disambiguatedPathLabels(active), ...disambiguatedPathLabels(archived)]);
});

/** Flat display-label string (`prefix/name` when disambiguated). Falls
 *  back to the empty string for unknown ids — callers with a "(deleted)"
 *  style placeholder should check getProjectLabel themselves. */
export function getProjectLabelText(id: string): string {
  const label = projectLabels.get(entryIdFor(id));
  return label ? formatPathLabel(label) : '';
}

// Manual reorder.
//
// The order the user dragged is an overlay, not a write to the rows: the
// rows carry the last order a computer accepted and the overlay carries the
// one still out. A refusal drops the overlay, so there is nothing to undo,
// nothing to undo onto a row something else changed meanwhile, and no
// sequence of refusals can leave an order no write accepted.
//
// One write at a time and the newest order wins: a request made while a
// write is out replaces whatever was waiting, which was never sent, and is
// sent when the write settles.
interface OrderRequest {
  ids: readonly string[];
  order: ReadonlyMap<string, number>;
  resolve: () => void;
  reject: (err: unknown) => void;
}

let pendingOrder: ReadonlyMap<string, number> | null = $state.raw(null);
let orderInFlight = false;
let orderWaiting: OrderRequest | null = null;

/**
 * The position the sidebar sorts `row` by: the order still out while a
 * reorder has not settled, else the position the row carries.
 */
export function projectOrderPosition(row: ProjectWithCounts): number {
  return pendingOrder?.get(row.project.id) ?? row.project.sortPosition;
}

/**
 * Put the projects `orderedIds` names at those positions: at once for the
 * sidebar, then on this computer, which holds no other computer's rows and
 * skips an id it does not hold.
 *
 * Rejects with what the write threw. A request made while another is out
 * supersedes any other that was waiting, and such a superseded request
 * resolves without being sent, because the order that replaced it is the
 * one the user is waiting to see.
 */
export function reorderProjects(orderedIds: readonly string[]): Promise<void> {
  const ids = [...orderedIds];
  const order = new Map(ids.map((id, index) => [id, index]));
  pendingOrder = order;
  return new Promise<void>((resolve, reject) => {
    const request: OrderRequest = { ids, order, resolve, reject };
    if (orderInFlight) {
      orderWaiting?.resolve();
      orderWaiting = request;
      return;
    }
    void driveOrderWrites(request);
  });
}

async function driveOrderWrites(first: OrderRequest): Promise<void> {
  orderInFlight = true;
  try {
    let request: OrderRequest | null = first;
    while (request !== null) {
      let error: unknown = null;
      try {
        await UpdateProjectSortPositions([...request.ids]);
        applyAcceptedOrder(request.ids);
      } catch (err) {
        error = err;
      }
      // Only when the overlay is still the order this write carried: a
      // newer request has replaced it otherwise, and that order is what
      // the sidebar must keep showing until its own write settles.
      if (pendingOrder === request.order) pendingOrder = null;
      if (error === null) request.resolve();
      else request.reject(error);
      request = orderWaiting;
      orderWaiting = null;
    }
  } finally {
    orderInFlight = false;
  }
}

/** Fold an accepted order into the rows, one pass over the catalog. */
function applyAcceptedOrder(ids: readonly string[]): void {
  const positions = new Map<string, number>();
  const backends = new Set<BackendKey | undefined>();
  for (const [index, id] of ids.entries()) {
    if (getProject(id) === undefined) continue;
    positions.set(id, index);
    backends.add(projectBackend(id));
  }
  if (positions.size === 0) return;
  catalogWriter.mutate(backends, (rows) => rows.map((row) => {
    const position = positions.get(row.project.id);
    return position === undefined || row.project.sortPosition === position
      ? row
      : { ...row, project: { ...row.project, sortPosition: position } };
  }));
}

/** True once a round has committed at least one computer's project rows. */
export function isLoaded(): boolean {
  return loaded;
}

function listProjectRows(): Promise<ProjectWithCounts[]> {
  return ListProjects();
}

function noteProjectRow(row: ProjectWithCounts, backend: BackendKey): void {
  noteProject(row.project.id, backend);
}

function projectCatalog() {
  return computerCatalog('projects', () => projects, (row) => projectBackend(row.project.id));
}

function commitProjectRows(result: ComputerRows<ProjectWithCounts>): void {
  projects = retainUnavailableComputerRows(projects, result, (row) => projectBackend(row.project.id));
  loaded = true;
  settleCatalogAnswers('projects', result.answered);
}

/**
 * How the project catalog is read from each computer and committed. The
 * sidebar's project headers are one unit with the threads under them and
 * the groups that gather those threads, so this catalog is read only beside
 * the other two, as one round (./sidebarCatalogs.ts). Nothing else reads it:
 * a second reader would supersede the round's and leave its tree held.
 */
export function projectCatalogPart(): CatalogPart<ProjectWithCounts> {
  return {
    read: listProjectRows,
    note: noteProjectRow,
    commit: commitProjectRows,
    catalog: projectCatalog(),
  };
}

/**
 * Insert a freshly-created project at the head of the list. Accepts a
 * bare Project (the CreateProject binding returns one without counts) and
 * wraps it as ProjectWithCounts with zero counts — the next refresh will
 * reconcile actual totals.
 */
const catalogWriter = computerCatalogWriter('projects', () => projects, (rows) => { projects = rows; }, (row) => projectBackend(row.project.id));

export function addProjectLocal(p: Project): void {
  const wrapped: ProjectWithCounts = {
    project: p,
    threadCount: 0,
    lastActive: 0,
  };
  // Prevent duplicate inserts if the caller fires this and a refresh
  // races. Refresh wins because it carries thread counts.
  catalogWriter.mutate(projectBackend(p.id), (rows) =>
    rows.some((existing) => existing.project.id === p.id) ? rows : [wrapped, ...rows]);
}

/**
 * The project belongs in the sidebar (a `listed` frame): its row replaced
 * where it is, keeping the counts as `updateProjectLocal` does, and added
 * when absent. One transform for both, because a catalog read in flight
 * replays it over an answer that may lack the row.
 */
export function listProjectLocal(p: Project): void {
  catalogWriter.mutate(projectBackend(p.id), (rows) => rows.some((existing) => existing.project.id === p.id)
    ? rows.map((existing) => existing.project.id === p.id ? { ...existing, project: p } : existing)
    : [{ project: p, threadCount: 0, lastActive: 0 }, ...rows]);
}

/**
 * Replace a project's row after rename / archive / unarchive. Preserves
 * the existing threadCount + lastActive so the sidebar doesn't flicker
 * back to 0 threads until the next refresh.
 */
export function updateProjectLocal(p: Project): void {
  catalogWriter.mutate(projectBackend(p.id), (rows) => rows.map((existing) =>
    existing.project.id === p.id
      ? { ...existing, project: p }
      : existing,
  ));
}

/**
 * Bump a project's activity projection when one of its threads receives
 * newer live activity. Mirrors the backend's ListProjects lastActive value
 * without refetching the whole projects list for every streamed item.
 */
export function touchProjectActivity(projectId: string | undefined, updatedAt: number): void {
  if (!projectId || !Number.isFinite(updatedAt)) return;
  const existing = getProject(projectId);
  if (existing === undefined) return;
  if (getProjectLiveActivityAt(existing) >= updatedAt) return;
  liveActivityAt.set(projectId, updatedAt);
}

/**
 * The project's newest activity timestamp: the backend's lastActive or
 * the live streaming bump, whichever is ahead. Reactive on the
 * per-project box — see the threads store's getThreadLiveActivityAt for
 * why bumps stay out of the array signal.
 */
export function getProjectLiveActivityAt(p: ProjectWithCounts): number {
  return Math.max(p.lastActive ?? 0, liveActivityAt.get(p.project.id));
}

/** Drop a project row and any related thread counts. */
export function removeProjectLocal(id: string): void {
  invalidateReplicaCatalog(projectBackend(id) ?? '', 'projects');
  catalogWriter.mutate(projectBackend(id), (rows) => rows.filter((p) => p.project.id !== id));
  liveActivityAt.drop(id);
}

/** Test helper — clears state between tests. */
/**
 * Drop every project row a detached backend owned, for the reason
 * `threads.svelte.ts` states about its own: the entity index has already
 * forgotten the machine, so a row left here would route its next call to
 * the page's own backend.
 */
export function dropProjectsForDetachedBackend(ids: readonly string[]): void {
  if (ids.length === 0) return;
  const gone = new Set(ids);
  const kept = projects.filter((p) => !gone.has(p.project.id));
  if (kept.length === projects.length) return;
  projects = kept;
}

onBackendDetached(({ projectIds }) => dropProjectsForDetachedBackend(projectIds));

export function resetProjectsForTest(): void {
  catalogWriter.reset();
  projects = [];
  loaded = false;
  liveActivityAt.reset();
  pendingOrder = null;
  orderInFlight = false;
  orderWaiting = null;
}
