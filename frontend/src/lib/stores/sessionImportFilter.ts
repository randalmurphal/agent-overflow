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
  | 'hidden-only'
  | 'rows';

/** True for the states that render the toolbar and the progress strip. */
export function surfaceHasCatalog(surface: ImportSurface): boolean {
  return (
    surface === 'rows' ||
    surface === 'empty' ||
    surface === 'no-matches' ||
    surface === 'hidden-only'
  );
}

/**
 * Classify the modal body. One decision in one place: the branch that picks
 * the empty state and the branch that decides whether the toolbar renders
 * would otherwise be two conditions to keep in agreement.
 *
 * `hidden-only` is split out of `no-matches` because the two have different
 * escapes. `hiddenCount` counts rows that pass every filter and are held back
 * ONLY by the already-ran toggle, so a zero view with a non-zero hiddenCount
 * means showing them is guaranteed to render rows — "No sessions match these
 * filters" would be a lie there, and its Clear-filters button would do
 * nothing.
 */
export function importSurface(input: {
  status: SessionImportStatus;
  providers: readonly ImportProviderStatus[];
  rowCount: number;
  filteredCount: number;
  /** Rows the already-ran toggle is currently withholding; see above. */
  hiddenCount: number;
}): ImportSurface {
  const { status, providers, rowCount, filteredCount, hiddenCount } = input;
  if (status === 'idle' || status === 'loading') return 'loading';
  if (status === 'error') return 'error';
  // The contract guarantees one status entry per provider, so an empty array
  // means the scan reported nothing — not that every provider is broken.
  if (providers.length > 0 && providers.every((p) => !p.available)) return 'unavailable';
  if (rowCount === 0) return 'empty';
  if (filteredCount > 0) return 'rows';
  return hiddenCount > 0 ? 'hidden-only' : 'no-matches';
}

/** The filters the toolbar drives, applied together. */
export interface ImportRowFilters {
  providerFilter: ImportProviderFilter;
  /** An `ImportProjectGroup.key`, or null for "every project". */
  projectFilter: string | null;
  query: string;
  /**
   * Include rows Agent Overflow itself produced. Off by default: importing
   * one duplicates work that is already here. This is EXCLUSION, not a view
   * filter — while it is off those rows are outside the offer entirely, so
   * nothing that counts, selects or imports may see them.
   */
  showAlreadyRan: boolean;
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

/**
 * The project a row belongs to, as the dropdown and the project filter both
 * see it: the resolved project id when AO already has one, else the cwd.
 *
 * Several cwds legitimately resolve to ONE project — a repo root and every
 * subdirectory an agent happened to run in (`/repo`, `/repo/frontend`) — and
 * the backend has already done that resolution. Keying on the raw cwd listed
 * that project once per cwd, as several identically-labelled entries whose
 * counts split the rows between them.
 *
 * Rows with no project keep their path as the key: there is nothing yet to
 * merge them onto, and importing one is what creates the project.
 */
export function projectGroupKey(row: ImportableSession): string {
  return hasKnownProject(row) ? row.projectId : row.projectPath;
}

/**
 * Whether the row resolves to a project AO already has. One definition, so
 * `projectGroupKey` and the group's `known` flag cannot disagree about what
 * the key it produced actually is.
 */
function hasKnownProject(row: ImportableSession): boolean {
  return row.projectId !== '';
}

/**
 * True for a row the already-ran toggle currently withholds. Exported because
 * this is EXCLUSION rather than a view filter (see `showAlreadyRan`), so the
 * store's selection and retry paths have to apply the same test the
 * projections do.
 */
export function isHiddenRow(
  row: ImportableSession,
  filters: Pick<ImportRowFilters, 'showAlreadyRan'>,
): boolean {
  return !filters.showAlreadyRan && row.ranInAgentOverflow;
}

/** Provider + project + query. Says nothing about the already-ran toggle. */
function matchesFilters(
  row: ImportableSession,
  filters: ImportRowFilters,
  needle: string,
): boolean {
  return (
    matchesProvider(row, filters.providerFilter) &&
    (filters.projectFilter === null || projectGroupKey(row) === filters.projectFilter) &&
    matchesQuery(row, needle)
  );
}

/** Rows surviving every filter, in catalog order. */
export function filterImportRows(
  rows: readonly ImportableSession[],
  filters: ImportRowFilters,
): ImportableSession[] {
  const needle = normalizeQuery(filters.query);
  return rows.filter((row) => !isHiddenRow(row, filters) && matchesFilters(row, filters, needle));
}

/**
 * How many rows the already-ran toggle is withholding from the CURRENT view.
 *
 * Counted under provider + project + query so the toggle's label answers the
 * question the user is actually asking ("what am I not seeing right now?"),
 * and so a zero view can tell "hidden" apart from "no matches". Independent
 * of the toggle itself: with it on, this is what turning it off would remove.
 */
export function countAlreadyRanRows(
  rows: readonly ImportableSession[],
  filters: ImportRowFilters,
): number {
  const needle = normalizeQuery(filters.query);
  let count = 0;
  for (const row of rows) {
    if (row.ranInAgentOverflow && matchesFilters(row, filters, needle)) count += 1;
  }
  return count;
}

/**
 * Project dropdown entries, one per PROJECT (see `projectGroupKey`).
 *
 * Membership comes from every row the toggle does not withhold — picking a
 * project must not make the other projects disappear from the menu it was
 * picked in, but a project whose every row is excluded from the offer has
 * nothing to pick and does not belong in the menu at all. Each `count`
 * respects the provider and query filters only, so the number next to a
 * project is what choosing it would show.
 *
 * Sorted by count descending, then label (case-insensitive), then path: the
 * projects a real home is dominated by are the ones worth reaching first, and
 * the two tiebreakers keep the order stable across scans regardless of row
 * order.
 */
export function buildProjectGroups(
  rows: readonly ImportableSession[],
  filters: Pick<ImportRowFilters, 'providerFilter' | 'query' | 'showAlreadyRan'>,
): ImportProjectGroup[] {
  const needle = normalizeQuery(filters.query);
  const groups = new Map<string, ImportProjectGroup>();
  for (const row of rows) {
    if (isHiddenRow(row, filters)) continue;
    const key = projectGroupKey(row);
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        path: row.projectPath,
        label: row.projectLabel,
        count: 0,
        known: hasKnownProject(row),
      };
      groups.set(key, group);
    } else if (shorterPath(row.projectPath, group.path)) {
      // The project's own root is not on the row, so the shortest member cwd
      // is the closest stand-in. Only the path varies within a group: every
      // row of a known project carries that project's name, and an unknown
      // group is keyed on the path itself.
      group.path = row.projectPath;
    }
    if (matchesProvider(row, filters.providerFilter) && matchesQuery(row, needle)) {
      group.count += 1;
    }
  }
  return [...groups.values()].sort((a, b) => {
    if (a.count !== b.count) return b.count - a.count;
    const byLabel = a.label.toLowerCase().localeCompare(b.label.toLowerCase());
    return byLabel !== 0 ? byLabel : a.path.localeCompare(b.path);
  });
}

/** Shorter wins; equal lengths break on the path itself, never on row order. */
function shorterPath(candidate: string, current: string): boolean {
  if (candidate.length !== current.length) return candidate.length < current.length;
  return candidate < current;
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
