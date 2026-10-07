// The key two backends' project rows are merged on.
//
// A project is a repository, and the same repository checked out on two
// machines is ONE project with two targets (remote-access §10, wave 7d).
// Paths cannot say that — the same path names a different checkout on
// every machine — so the key is the repo's own identity: its `origin`
// remote, normalised so the spellings git accepts for one remote agree,
// else its root commit, which is a fact about the history and therefore the
// same everywhere the history is. A directory that is neither answers ''
// and is never merged.

import type { Project } from '../types/models';

type Identity = Pick<Project, 'remoteURL' | 'rootCommit'>;

/** Whether the project carries an identity to merge on: a remote or a root. */
export function hasRepoIdentity(project: Identity): boolean {
  return normalizeRemoteURL(project.remoteURL ?? '') !== '' || normalizeRootCommit(project.rootCommit) !== '';
}

function normalizeRootCommit(root: string | undefined): string {
  return (root ?? '').trim().toLowerCase();
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

/**
 * Groups rows into repositories.
 *
 * A row with a remote groups by its normalised remote. A row without one
 * (no `origin`, or a remote that is a local path) joins the remote group
 * holding its root commit when exactly one does, so a remoteless checkout
 * still merges with clones that have an origin; with none, or several (a
 * fork and its upstream share a root), it groups by root commit.
 *
 * Each computer contributes at most one member to a group: its oldest
 * live row, so a second clone on one computer stays its own entry and
 * adding one never moves the clone already merged. Every keyed row,
 * member or not, is in keyOf.
 */
export function groupRepositories<T extends RepoGroupRow>(rows: readonly T[]): RepoGroups<T> {
  const keyOf = new Map<T, string>();
  const remoteKeysByRoot = new Map<string, Set<string>>();
  for (const row of rows) {
    const remote = normalizeRemoteURL(row.project.remoteURL ?? '');
    if (remote === '') continue;
    const key = `remote:${remote}`;
    keyOf.set(row, key);
    const root = normalizeRootCommit(row.project.rootCommit);
    if (root === '') continue;
    const keys = remoteKeysByRoot.get(root) ?? new Set<string>();
    keys.add(key);
    remoteKeysByRoot.set(root, keys);
  }
  for (const row of rows) {
    if (keyOf.has(row)) continue;
    const root = normalizeRootCommit(row.project.rootCommit);
    if (root === '') {
      keyOf.set(row, '');
      continue;
    }
    const keys = remoteKeysByRoot.get(root);
    keyOf.set(row, keys?.size === 1 ? keys.values().next().value! : `commit:${root}`);
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

/**
 * `host/owner/repo` for every shape git accepts for one remote:
 *
 *   https://user@github.com/Owner/Repo.git
 *   ssh://git@github.com:22/Owner/Repo
 *   git@github.com:Owner/Repo.git
 *   git://github.com/Owner/Repo
 *
 * The host is lowercased (DNS is case-insensitive); the path keeps its
 * case, because forges differ on whether it matters and merging two repos
 * that differ only there would be a wrong answer rather than a missed one.
 * Trailing `.git` and `/` are dropped. '' for anything else — a local path
 * remote, say, is a fact about one machine.
 */
export function normalizeRemoteURL(raw: string): string {
  const url = raw.trim();
  if (url === '') return '';
  // scp-like `user@host:path` — the one shape without a scheme. A Windows
  // drive letter (`C:\…`) also has a colon: a one-letter host is a drive,
  // and a backslash anywhere is a local path, never a remote.
  const scp = /^(?:[^@/\\]+@)?([^:/\\]{2,}):(?!\/\/)([^\\]+)$/.exec(url);
  let host = '';
  let path = '';
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(url)) {
    let parsed: URL;
    try {
      parsed = new URL(url);
    } catch {
      return '';
    }
    if (parsed.protocol === 'file:') return '';
    host = parsed.hostname;
    path = parsed.pathname;
  } else if (scp) {
    host = scp[1];
    path = scp[2];
  } else {
    return '';
  }
  host = host.toLowerCase();
  path = path.replace(/^\/+/, '').replace(/\/+$/, '');
  if (path.toLowerCase().endsWith('.git')) path = path.slice(0, -4);
  if (host === '' || path === '') return '';
  return `${host}/${path}`;
}
