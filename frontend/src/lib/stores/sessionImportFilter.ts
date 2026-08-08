// Pure projections over the session-import catalog. Everything the import
// modal renders that isn't a Svelte concern lives here so it stays
// table-testable: the reactive store (sessionImport.svelte.ts) holds the
// filter state, the components only render these outputs.
//
// No runes — same contract as workflowData.ts.

import type {
  ImportProjectGroup,
  ImportProviderFilter,
  ImportProviderStatus,
  ImportableSession,
} from '../types/sessionImport';
import type { SessionImportStatus } from './sessionImport.svelte';

/**
 * Which of the modal body's mutually-exclusive states applies.
 *
 * `'rows'` is the only one that renders the catalogue. The toolbar renders
 * for all three catalogue-shaped states, because `'no-matches'` is escapable
 * only by changing a filter — hiding the filters with it would strand the
 * user in a state with no way out but Cancel.
 */
export type ImportSurface =
  | 'loading'
  | 'error'
  | 'unavailable'
  | 'empty'
  | 'no-matches'
  | 'rows';

/** True for the states that render the toolbar and the progress strip. */
export function surfaceHasCatalog(surface: ImportSurface): boolean {
  return surface === 'rows' || surface === 'empty' || surface === 'no-matches';
}

/**
 * Classify the modal body. One decision in one place: the branch that picks
 * the empty state and the branch that decides whether the toolbar renders
 * would otherwise be two conditions to keep in agreement.
 */
export function importSurface(input: {
  status: SessionImportStatus;
  providers: readonly ImportProviderStatus[];
  rowCount: number;
  filteredCount: number;
}): ImportSurface {
  const { status, providers, rowCount, filteredCount } = input;
  if (status === 'idle' || status === 'loading') return 'loading';
  if (status === 'error') return 'error';
  // The contract guarantees one status entry per provider, so an empty array
  // means the scan reported nothing — not that every provider is broken.
  if (providers.length > 0 && providers.every((p) => !p.available)) return 'unavailable';
  if (rowCount === 0) return 'empty';
  return filteredCount === 0 ? 'no-matches' : 'rows';
}

/** The three filters the toolbar drives, applied together. */
export interface ImportRowFilters {
  providerFilter: ImportProviderFilter;
  /** `ImportableSession.projectPath`, or null for "every project". */
  projectFilter: string | null;
  query: string;
}

/** Selection tri-state of the toolbar's select-all checkbox. */
export type ImportSelectAllState = 'none' | 'some' | 'all';

/**
 * Footer roll-up for the current selection.
 *
 * Deliberately no "threads" figure. `ImportableSession.branchCount` is 0 for
 * every Claude row — not zero threads, NOT DETERMINED: counting a
 * transcript's branches costs a full read and a real Claude home is
 * gigabytes. Anything derived from it would be a lower bound that equals the
 * session count on the provider that dominates the list, i.e. a second
 * number saying what "12 selected" already said, and wrong the moment a
 * session actually branched. The true count is known exactly once the run
 * reports its `threadIds`, and that is where it is shown (the completion
 * toast).
 */
export interface ImportSelectionSummary {
  count: number;
  bytes: number;
}

function matchesProvider(row: ImportableSession, filter: ImportProviderFilter): boolean {
  return filter === 'all' || row.provider === filter;
}

/**
 * Search matches title, project label, and git branch — the three things a
 * row actually shows. Case-insensitive substring; an all-whitespace query
 * matches everything.
 */
function matchesQuery(row: ImportableSession, needle: string): boolean {
  if (needle === '') return true;
  return (
    row.title.toLowerCase().includes(needle) ||
    row.projectLabel.toLowerCase().includes(needle) ||
    (row.gitBranch ?? '').toLowerCase().includes(needle)
  );
}

function normalizeQuery(query: string): string {
  return query.trim().toLowerCase();
}

/** Rows surviving provider + project + query, in catalog order. */
export function filterImportRows(
  rows: readonly ImportableSession[],
  filters: ImportRowFilters,
): ImportableSession[] {
  const needle = normalizeQuery(filters.query);
  const { providerFilter, projectFilter } = filters;
  return rows.filter(
    (row) =>
      matchesProvider(row, providerFilter) &&
      (projectFilter === null || row.projectPath === projectFilter) &&
      matchesQuery(row, needle),
  );
}

/**
 * Project dropdown entries. Membership comes from ALL rows — picking a
 * project must not make the other projects disappear from the menu it was
 * picked in — while each `count` respects the provider and query filters
 * only, so the number next to a project is what choosing it would show.
 *
 * Sorted by label (case-insensitive), then path, so the order is stable
 * across scans regardless of row order.
 */
export function buildProjectGroups(
  rows: readonly ImportableSession[],
  filters: Pick<ImportRowFilters, 'providerFilter' | 'query'>,
): ImportProjectGroup[] {
  const needle = normalizeQuery(filters.query);
  const groups = new Map<string, ImportProjectGroup>();
  for (const row of rows) {
    let group = groups.get(row.projectPath);
    if (!group) {
      group = { path: row.projectPath, label: row.projectLabel, count: 0 };
      groups.set(row.projectPath, group);
    }
    if (matchesProvider(row, filters.providerFilter) && matchesQuery(row, needle)) {
      group.count += 1;
    }
  }
  return [...groups.values()].sort((a, b) => {
    const byLabel = a.label.toLowerCase().localeCompare(b.label.toLowerCase());
    return byLabel !== 0 ? byLabel : a.path.localeCompare(b.path);
  });
}

/**
 * Footer roll-up over the WHOLE catalog, not the filtered view: a selection
 * survives filter changes, so hiding a selected row must not quietly drop it
 * from the count the primary button acts on.
 */
export function selectionSummary(
  rows: readonly ImportableSession[],
  selected: ReadonlySet<string>,
): ImportSelectionSummary {
  let count = 0;
  let bytes = 0;
  for (const row of rows) {
    if (!selected.has(row.id)) continue;
    count += 1;
    bytes += row.sizeBytes;
  }
  return { count, bytes };
}

/** Tri-state of the select-all checkbox over the currently visible rows. */
export function selectAllState(
  filtered: readonly ImportableSession[],
  selected: ReadonlySet<string>,
): ImportSelectAllState {
  if (filtered.length === 0) return 'none';
  let hits = 0;
  for (const row of filtered) {
    if (selected.has(row.id)) hits += 1;
  }
  if (hits === 0) return 'none';
  return hits === filtered.length ? 'all' : 'some';
}
