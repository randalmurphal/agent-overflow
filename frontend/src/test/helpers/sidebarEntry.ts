// A sidebar entry for a component test.
//
// Components under components/sidebar take the entry value, never a project
// row (lib/components/sidebar/sidebarEntries.ts). A test that renders one row
// builds its entry from a row with `entryFromRow`; a test with rows in the
// projects store takes the real thing with `storeEntryFor`, so a change to a
// row reaches the component the way it does in the app.
import type { Project, ProjectWithCounts } from '../../lib/types/models';
import { HOME_BACKEND } from '../../lib/transport/backendKey';
import { isProjectExpanded } from '../../lib/stores/sidebar.svelte';
import {
  buildSidebarEntries,
  type SidebarEntry,
} from '../../lib/components/sidebar/sidebarEntries';

function warningsOf(project: Project): SidebarEntry['warnings'] {
  return project.identityError
    ? [{ id: project.id, path: project.path, reason: project.identityError }]
    : [];
}

/** The entry one row renders as, with the disclosure the sidebar store holds. */
export function entryFromRow(row: ProjectWithCounts, overrides: Partial<SidebarEntry> = {}): SidebarEntry {
  const { project } = row;
  return {
    key: project.id,
    id: project.id,
    backend: HOME_BACKEND,
    name: project.name,
    path: project.path,
    createdAt: project.createdAt,
    threadCount: row.threadCount,
    position: project.sortPosition,
    expanded: isProjectExpanded(project.id),
    members: [row],
    warnings: warningsOf(project),
    ...overrides,
  };
}

/**
 * The entries the sidebar would render for the projects in the store, built
 * by the real projection: every computer listed, every computer visible.
 */
export function storeEntries(): readonly SidebarEntry[] {
  return buildSidebarEntries({
    listed: () => true,
    visible: () => true,
    expanded: ({ id }) => isProjectExpanded(id),
    previous: [],
  }).entries;
}

/** The entry of one project among `storeEntries`. */
export function storeEntryFor(projectId: string): SidebarEntry {
  const entries = storeEntries();
  const entry = entries.find((candidate) => candidate.id === projectId
    || candidate.members.some((member) => member.project.id === projectId));
  if (entry === undefined) throw new Error(`no sidebar entry for ${projectId}`);
  return entry;
}
