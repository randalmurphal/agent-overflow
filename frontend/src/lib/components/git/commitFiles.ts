import type { CommitFile, CommitSelection } from '../../types/git';

// Selection and display rules of the commit dialog's file list. A row is
// keyed by its path; a rename's row stands for both of its paths.

/** The paths a row's diff covers. */
export function rowPaths(file: CommitFile): string[] {
  return file.oldPath ? [file.oldPath, file.path] : [file.path];
}

/**
 * What a commit of the dialog's state covers: everything while git records
 * only whole commits, else the selected rows as they were listed, else
 * nothing (null). The backend refuses a row that is no longer the listed
 * change, so a commit never records content the list did not show.
 */
export function commitSelection(
  files: readonly CommitFile[],
  selected: ReadonlySet<string>,
  wholeOnly: boolean,
): CommitSelection | null {
  if (wholeOnly) return { all: true, files: [] };
  const rows = files.filter((file) => selected.has(file.path));
  return rows.length > 0 ? { all: false, files: rows } : null;
}

/**
 * The selection carried over a refreshed list: rows that are gone drop out,
 * and rows that appeared stay unselected. A selected rename that the new
 * list shows as a deletion and an addition keeps both selected, so the
 * commit still takes the whole move. Returns the same set when it is
 * unchanged.
 */
export function carrySelection(
  selected: ReadonlySet<string>,
  before: readonly CommitFile[],
  after: readonly CommitFile[],
): ReadonlySet<string> {
  if (selected.size === 0) return selected;
  const listed = new Map(after.map((file) => [file.path, file]));
  const next = new Set<string>();
  for (const file of after) {
    if (selected.has(file.path)) next.add(file.path);
  }
  for (const file of before) {
    if (!file.oldPath || !next.has(file.path)) continue;
    if (!listed.get(file.path)?.oldPath && listed.has(file.oldPath)) next.add(file.oldPath);
  }
  if (next.size === selected.size && [...next].every((path) => selected.has(path))) return selected;
  return next;
}

/** Selects every row, or clears the selection when every row is selected. */
export function toggleAll(selected: ReadonlySet<string>, files: readonly CommitFile[]): ReadonlySet<string> {
  const all = files.length > 0 && files.every((file) => selected.has(file.path));
  return all ? new Set() : new Set(files.map((file) => file.path));
}

export function toggleOne(selected: ReadonlySet<string>, path: string): ReadonlySet<string> {
  const next = new Set(selected);
  if (!next.delete(path)) next.add(path);
  return next;
}

/**
 * The row that takes over as active after a refresh: the same path while it
 * is listed, else the row now at its old position, else none.
 */
export function carryActive(active: string | null, before: readonly CommitFile[], after: readonly CommitFile[]): string | null {
  if (after.length === 0) return null;
  if (active === null) return after[0].path;
  if (after.some((file) => file.path === active)) return active;
  const index = before.findIndex((file) => file.path === active);
  return after[Math.min(Math.max(index, 0), after.length - 1)].path;
}

export interface CommitFileStatus {
  label: string;
  class: string;
}

const STATUS: Record<string, CommitFileStatus> = {
  A: { label: 'Added', class: 'text-success' },
  M: { label: 'Modified', class: 'text-warning' },
  D: { label: 'Deleted', class: 'text-error' },
  R: { label: 'Renamed', class: 'text-info' },
  T: { label: 'Type changed', class: 'text-warning' },
};

export function commitFileStatus(status: string): CommitFileStatus {
  return STATUS[status] ?? { label: 'Changed', class: 'text-fg-muted' };
}

/** A path as the row shows it: the file name, and the directory it is in. */
export function splitPath(path: string): { name: string; dir: string } {
  const cut = path.lastIndexOf('/');
  return cut < 0 ? { name: path, dir: '' } : { name: path.slice(cut + 1), dir: path.slice(0, cut) };
}

/** The identity of a row's change: its preview is read again when it moves. */
export function changeKey(file: CommitFile): string {
  return [file.path, file.oldPath ?? '', file.status, file.version].join('\n');
}
