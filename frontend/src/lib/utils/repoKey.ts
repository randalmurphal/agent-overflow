// Cross-computer repository grouping uses only forge-verified IDs.
import type { Project } from '../types/models';

type Identity = Pick<Project, 'repositoryID'>;

export function hasRepoIdentity(project: Identity): boolean {
  return Boolean(project.repositoryID);
}

/** One project row as the grouping reads it. */
export interface RepoGroupRow {
  project: Identity & Pick<Project, 'id' | 'createdAt' | 'archived'>;
  /** The computer the row lives on. */
  computer: string;
}

export interface RepoGroups<T> {
  /** Row → its repository key ('' for a row with no identity). */
  keyOf: Map<T, string>;
  /** Key → the rows that merge under it: at most one per computer. */
  members: Map<string, T[]>;
}

/** Keep the oldest live checkout per computer in each verified repository. */
export function groupRepositories<T extends RepoGroupRow>(rows: readonly T[]): RepoGroups<T> {
  const keyOf = new Map<T, string>();
  for (const row of rows) {
    const id = row.project.repositoryID;
    keyOf.set(row, id ? `forge:${id}` : '');
  }

  const chosen = new Map<string, Map<string, T>>();
  for (const row of rows) {
    const key = keyOf.get(row)!;
    if (key === '') continue;
    const byComputer = chosen.get(key) ?? new Map<string, T>();
    const current = byComputer.get(row.computer);
    if (current === undefined || precedes(row, current)) byComputer.set(row.computer, row);
    chosen.set(key, byComputer);
  }
  const members = new Map<string, T[]>();
  for (const [key, byComputer] of chosen) members.set(key, [...byComputer.values()]);
  return { keyOf, members };
}

/** Live before archived, then oldest, then id: stable across reloads. */
function precedes(a: RepoGroupRow, b: RepoGroupRow): boolean {
  if (a.project.archived !== b.project.archived) return !a.project.archived;
  if (a.project.createdAt !== b.project.createdAt) return a.project.createdAt < b.project.createdAt;
  return a.project.id < b.project.id;
}
