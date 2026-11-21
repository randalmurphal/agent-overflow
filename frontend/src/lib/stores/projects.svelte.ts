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
import { ListProjects } from './bindings';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import {
  disambiguatedPathLabels,
  formatPathLabel,
  type PathLabel,
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

export interface MergedProjects {
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
 * Group `rows` into one entry per repository: at most one member per
 * computer, the entry worn by its home member when there is one, and the
 * thread count and activity summed over the members given here.
 *
 * The caller chooses the rows, so a surface showing only some of them gets a
 * representative and an aggregate of those alone. The sidebar passes the
 * members whose computer lists its tree, which is what keeps a computer that
 * lists later from re-sorting or relabelling an entry already on screen; the
 * store's own `merged` passes every row, for the pickers, settings and
 * routing that answer for the whole repository.
 */
export function mergeProjectRows(rows: readonly ProjectWithCounts[]): MergedProjects {
  if (!hasMultipleBackends()) return { entries: rows, ...NO_MERGE };
  const { keyOf, members: groups } = groupRows(rows);
  const position = new Map(rows.map((row, index) => [row, index]));
  const repoKeyOf = new Map<string, string>();
  const groupOf = new Map<ProjectWithCounts, ProjectWithCounts[]>();
  for (const group of groups) {
    if (group.length < 2) continue;
    // Home first, then list order — stable, so the entry does not swap its
    // representative when a machine reconnects.
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
    // An archived member never represents a live one: the sidebar hides an
    // entry by its representative's archived flag.
    const rep = group.find((member) => !member.project.archived) ?? group[0];
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

/** Structured display label for a project (prefix + name). Undefined when
 *  the id isn't in the store. */
export function getProjectLabel(id: string): PathLabel | undefined {
  return projectLabels.get(entryIdFor(id));
}

/** Flat display-label string (`prefix/name` when disambiguated). Falls
 *  back to the empty string for unknown ids — callers with a "(deleted)"
 *  style placeholder should check getProjectLabel themselves. */
export function getProjectLabelText(id: string): string {
  const label = projectLabels.get(entryIdFor(id));
  return label ? formatPathLabel(label) : '';
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
  const existing = projects.find((p) => p.project.id === projectId);
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
}
