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

/** '' when the project carries no identity to merge on. */
export function repoKey(project: Pick<Project, 'remoteURL' | 'rootCommit'>): string {
  const remote = normalizeRemoteURL(project.remoteURL ?? '');
  if (remote !== '') return `remote:${remote}`;
  const root = (project.rootCommit ?? '').trim().toLowerCase();
  return root === '' ? '' : `commit:${root}`;
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
