import type { ForgeFailure } from '../../utils/forgeFailure';

// The rows of one of the page's lists as the virtualizer holds them: each
// failed project (its heading over its error), then each group's heading
// over its rows. A row names the heading of its group, which the list
// keeps on screen while the group's rows scroll under it.

/** A project whose list read failed, or (`partial`) whose rows listed
 * while a detail request failed, so their facts read as unknown. */
export interface PullsListProjectError {
  projectId: string;
  name: string;
  message: string;
  failure: ForgeFailure | null;
  forge: string;
  /** The failure behind the message, for Details. */
  cause?: unknown;
  partial?: boolean;
}

export interface PullsListHeading {
  label: string;
  /** The count on the right; null for a heading with nothing to count. */
  count: number | null;
  testid: string;
}

/** Rows under one heading, or under none (`heading` null). */
export interface PullsListGroup<R> {
  id: string;
  heading: PullsListHeading | null;
  rows: readonly R[];
}

export type PullsListItem<R> =
  | { kind: 'error'; key: string; heading: PullsListHeading; error: PullsListProjectError }
  | { kind: 'heading'; key: string; heading: PullsListHeading }
  | { kind: 'row'; key: string; row: R; heading: number };

export function pullsListItems<R extends { key: string }>(
  errors: readonly PullsListProjectError[],
  errorTestid: string,
  groups: readonly PullsListGroup<R>[],
): PullsListItem<R>[] {
  const items: PullsListItem<R>[] = [];
  for (const error of errors) {
    items.push({ kind: 'error', key: `e:${error.projectId}`, heading: { label: error.partial ? `${error.name}: facts unavailable` : error.name, count: null, testid: errorTestid }, error });
  }
  for (const group of groups) {
    let heading = -1;
    if (group.heading) {
      heading = items.length;
      items.push({ kind: 'heading', key: `g:${group.id}`, heading: group.heading });
    }
    for (const row of group.rows) items.push({ kind: 'row', key: `r:${row.key}`, row, heading });
  }
  return items;
}

/** The index of the heading that names the item at `index`, or -1. */
export function headingIndexOf<R>(items: readonly PullsListItem<R>[], index: number): number {
  const item = items[index];
  if (!item) return -1;
  return item.kind === 'row' ? item.heading : index;
}
