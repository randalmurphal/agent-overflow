import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { invalidateReplicaCatalog } from '../replica/session';
import { computerCatalogWriter } from './computerCatalogWriter';
import { computerCatalog, readComputerRows, retainUnavailableComputerRows } from './computerRows';
// Sidebar-facing projects store. Mirrors the pattern of threads.svelte.ts:
// a single reactive $state array driven by an explicit refresh, with
// optimistic local mutations so the sidebar can reflect a create/rename/
// delete without round-tripping the server.
//
// Callers (Sidebar components, AddProjectModal) refresh once on mount
// and call the addProjectLocal / updateProjectLocal / removeProjectLocal
// helpers after a successful RPC so the list stays in sync with the
// backend. `refreshProjects` is still safe to call any time to resync.

import type { Project, ProjectWithCounts } from '../types/models';
import { ListProjects } from './bindings';
import { addToast } from './toast.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import {
  disambiguatedProjectLabels,
  formatProjectLabel,
  type ProjectLabel,
} from '../utils/pathDisplay';
import { repoKey } from '../utils/repoKey';
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

/** Lookup helper. Returns undefined when the id isn't in the store. */
export function getProject(id: string): ProjectWithCounts | undefined {
  return projects.find((p) => p.project.id === id);
}

// ---------------------------------------------------------------------------
// Merged entries (remote-access §10, wave 7d)
// ---------------------------------------------------------------------------
//
// A project is a repository, and the same repository checked out on two
// attached machines is ONE sidebar entry with two targets. The rows stay as
// the backends sent them (the entity index still answers which machine
// owns each id); what merges is the VIEW. An entry is represented by its
// home member when there is one, else its first member, so a person's own
// machine is the one whose name, colour and sort position the entry wears.
//
// Computed only while more than one backend is attached: a single-backend
// app returns the list itself, same array identity, and pays nothing.

interface MergedEntries {
  entries: ProjectWithCounts[];
  /** member project id → the entry (representative) id it belongs to. */
  entryOf: Map<string, string>;
  /** representative id → every member, home first. */
  members: Map<string, ProjectWithCounts[]>;
}

const NO_MERGE: Pick<MergedEntries, 'entryOf' | 'members'> = { entryOf: new Map(), members: new Map() };

const merged = $derived.by((): MergedEntries => {
  if (!hasMultipleBackends()) return { entries: projects, ...NO_MERGE };
  const byKey = new Map<string, ProjectWithCounts[]>();
  const order: ProjectWithCounts[] = [];
  const keyOf = new Map<string, string>();
  for (const row of projects) {
    const key = repoKey(row.project);
    if (key === '') {
      order.push(row);
      continue;
    }
    const bucket = byKey.get(key);
    if (bucket) {
      bucket.push(row);
    } else {
      byKey.set(key, [row]);
      order.push(row);
      keyOf.set(row.project.id, key);
    }
  }
  const entryOf = new Map<string, string>();
  const members = new Map<string, ProjectWithCounts[]>();
  const entries: ProjectWithCounts[] = [];
  for (const first of order) {
    const key = keyOf.get(first.project.id);
    const bucket = key === undefined ? undefined : byKey.get(key);
    if (!bucket || bucket.length < 2) {
      entries.push(first);
      continue;
    }
    // Home first, then attach order — stable, so the entry does not swap
    // its representative when a machine reconnects.
    const home = bucket.find((row) => (projectBackend(row.project.id) ?? HOME_BACKEND) === HOME_BACKEND);
    const rep = home ?? bucket[0];
    const ordered = home ? [home, ...bucket.filter((row) => row !== home)] : bucket;
    let threadCount = 0;
    let lastActive = 0;
    for (const row of ordered) {
      threadCount += row.threadCount;
      lastActive = Math.max(lastActive, row.lastActive ?? 0);
      entryOf.set(row.project.id, rep.project.id);
    }
    members.set(rep.project.id, ordered);
    entries.push(
      threadCount === rep.threadCount && lastActive === (rep.lastActive ?? 0)
        ? rep
        : { ...rep, threadCount, lastActive },
    );
  }
  return { entries, entryOf, members };
});

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

// Duplicate project names are legal (only paths are unique), so every
// name-rendering surface reads its label here: unique names label as-is,
// duplicates gain the minimal parent-dir prefix that tells them apart.
// One shared $derived so the map is computed once per list change. Over
// the ENTRIES, not the rows: two members of one repo share a name by
// definition and are not a collision. A member that is not the
// representative labels as its representative.
const projectLabels = $derived(
  disambiguatedProjectLabels(merged.entries.map((p) => p.project)),
);

/** Structured display label for a project (prefix + name). Undefined when
 *  the id isn't in the store. */
export function getProjectLabel(id: string): ProjectLabel | undefined {
  return projectLabels.get(entryIdFor(id));
}

/** Flat display-label string (`prefix/name` when disambiguated). Falls
 *  back to the empty string for unknown ids — callers with a "(deleted)"
 *  style placeholder should check getProjectLabel themselves. */
export function getProjectLabelText(id: string): string {
  const label = projectLabels.get(entryIdFor(id));
  return label ? formatProjectLabel(label) : '';
}

/** True once refreshProjects has completed at least one successful fetch. */
export function isLoaded(): boolean {
  return loaded;
}

/** Refresh each computer independently, preserving its cached rows on failure.
 * Superseded reads neither publish an empty catalog nor mark initial load done. */
export async function refreshProjects(): Promise<void> {
  try {
    const result = await readComputerRows<ProjectWithCounts>(
      () => ListProjects(), (row, backend) => noteProject(row.project.id, backend), computerCatalog('projects', () => projects, (row) => projectBackend(row.project.id), (late) => {
        projects = retainUnavailableComputerRows(projects, late, (row) => projectBackend(row.project.id));
        loaded = true;
      }));
    if (!result) return;
    projects = retainUnavailableComputerRows(projects, result, (row) => projectBackend(row.project.id));
    loaded = true;
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load projects:', err);
    addToast('error', 'Failed to load projects');
  }
}

/**
 * Insert a freshly-created project at the head of the list. Accepts a
 * bare Project (the CreateProject binding returns one without counts) and
 * wraps it as ProjectWithCounts with zero counts — the next refresh will
 * reconcile actual totals.
 */
const catalogWriter = computerCatalogWriter('projects', () => projects, (row) => projectBackend(row.project.id));

export function addProjectLocal(p: Project): void {
  // Prevent duplicate inserts if the caller fires this and a refresh
  // races. Refresh wins because it carries thread counts.
  if (projects.some((existing) => existing.project.id === p.id)) return;
  const wrapped: ProjectWithCounts = {
    project: p,
    threadCount: 0,
    lastActive: 0,
  };
  projects = [wrapped, ...projects];
  catalogWriter.changed(projectBackend(p.id));
}

/**
 * Replace a project's row after rename / archive / unarchive. Preserves
 * the existing threadCount + lastActive so the sidebar doesn't flicker
 * back to 0 threads until the next refresh.
 */
export function updateProjectLocal(p: Project): void {
  catalogWriter.changed(projectBackend(p.id));
  projects = projects.map((existing) =>
    existing.project.id === p.id
      ? { ...existing, project: p }
      : existing,
  );
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
  catalogWriter.changed(projectBackend(id));
  projects = projects.filter((p) => p.project.id !== id);
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
