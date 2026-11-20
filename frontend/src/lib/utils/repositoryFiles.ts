// Which link or image destinations in a PR/MR body name a file of the PR's
// own repository, and at which commit: the one table both the fact scan
// (forgeRepositoryFiles.ts, from raw text) and the destination hook (from a
// token's href) read, so a key the hook reads is a key the scan asked for.
//
//   docs/a.png, ./docs/a.png, /docs/a.png      the head commit, from the root
//   ../blob/<ref>/<path>, ../raw/<ref>/<path>  GitHub's page-relative form
//   https://<host>/<project>/blob|raw/<ref>/<path>
//   https://<host>/<project>/-/blob|raw/<ref>/<path>   (GitLab)
//   https://raw.githubusercontent.com/<owner>/<repo>/[refs/heads/]<ref>/<path>
//
// `<ref>` is read against what the frame names without asking anything:
// the head branch (its head commit), the base branch (its base commit),
// `HEAD` on GitLab (the head commit), or a full commit id. A branch name
// with slashes is matched as a prefix of the rest, the longest name first.
// Any other ref, host or project is not a repository file. The query and
// fragment are dropped, the path is percent-decoded once, and a path that
// leaves the root, ends in `/` or is not a clean repository path is not
// one.

import { canonicalHref } from '../markdown';
import type { FactRevision } from './forgeFacts';

/** The PR's forge address and project, as the table compares them. */
export interface RepositorySite {
  readonly github: boolean;
  /** The forge's `URL.host`, lowercase. */
  readonly host: string;
  /** `namespace/repo`. */
  readonly project: string;
}

/** A file of the repository at a full commit id. */
export interface RepositoryFile {
  readonly commit: string;
  readonly path: string;
}

const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:/;
const FULL_OID = /^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$/;
const CONTROL = /[\u0000-\u001f\\]/;
const RAW_GITHUB_HOST = 'raw.githubusercontent.com';

/** Observability for tests: how many destinations the table read. */
export const repositoryFileCounters = { reads: 0 };

/** `text` percent-decoded once; null when it is not valid encoding. */
export function decodeOnce(text: string): string | null {
  if (!text.includes('%')) return text;
  try {
    return decodeURIComponent(text);
  } catch {
    return null;
  }
}

/** `path` as a clean repository path, `.` and `..` folded; null when it
 * leaves the root, names a directory or is not one git can name. */
export function cleanRepositoryPath(path: string): string | null {
  if (path === '' || path.endsWith('/') || CONTROL.test(path)) return null;
  const segments: string[] = [];
  for (const segment of path.split('/')) {
    if (segment === '' || segment === '.') continue;
    if (segment === '..') {
      if (segments.length === 0) return null;
      segments.pop();
      continue;
    }
    segments.push(segment);
  }
  if (segments.length === 0 || segments[0].startsWith('-')) return null;
  return segments.join('/');
}

/** `<ref>/<path>` read against the frame: the commit the ref names and the path after it. */
function atRef(rest: string, site: RepositorySite, frame: () => FactRevision): RepositoryFile | null {
  const slash = rest.indexOf('/');
  if (slash <= 0) return null;
  const first = rest.slice(0, slash);
  let commit = '';
  let path = '';
  if (FULL_OID.test(first)) {
    commit = first.toLowerCase();
    path = rest.slice(slash + 1);
  } else {
    const revision = frame();
    let matched = -1;
    const consider = (name: string, at: string) => {
      if (name === '' || at === '' || name.length <= matched || !rest.startsWith(name) || rest.charCodeAt(name.length) !== 47 /* / */) return;
      matched = name.length;
      commit = at;
    };
    consider(revision.headRef, revision.head);
    consider(revision.baseRef, revision.base);
    if (!site.github) consider('HEAD', revision.head);
    if (matched === -1) return null;
    path = rest.slice(matched + 1);
  }
  const clean = cleanRepositoryPath(path);
  return clean === null ? null : { commit, path: clean };
}

/** `path` (decoded, from the host's root) as a project file URL's path. */
function projectPath(path: string, site: RepositorySite, revision: () => FactRevision): RepositoryFile | null {
  const prefix = `/${site.project}/`;
  if (path.length <= prefix.length || path.slice(0, prefix.length).toLowerCase() !== prefix.toLowerCase()) return null;
  let rest = path.slice(prefix.length);
  if (!site.github && rest.startsWith('-/')) rest = rest.slice(2);
  if (rest.startsWith('blob/')) return atRef(rest.slice(5), site, revision);
  if (rest.startsWith('raw/')) return atRef(rest.slice(4), site, revision);
  return null;
}

/**
 * The repository file `href` names, as a destination is read
 * (`canonicalHref`), or null when it names none. Work is linear in the
 * href and none of it runs for a fragment, a query or another scheme.
 * `revision` is asked only for a destination written by branch name or
 * relative to the PR, so a caller that records what it reads records it
 * only for those.
 */
export function repositoryFileOf(href: string, site: RepositorySite, revision: () => FactRevision): RepositoryFile | null {
  repositoryFileCounters.reads++;
  const written = canonicalHref(href).trim();
  const first = written.charCodeAt(0);
  if (written === '' || first === 35 /* # */ || first === 63 /* ? */) return null;
  if (SCHEME.test(written)) {
    if (first !== 104 /* h */ && first !== 72 /* H */) return null;
    let url: URL;
    try {
      url = new URL(written);
    } catch {
      return null;
    }
    if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.username !== '' || url.password !== '') return null;
    const path = decodeOnce(url.pathname);
    if (path === null) return null;
    const host = url.host.toLowerCase();
    if (host === RAW_GITHUB_HOST && site.github && site.host === 'github.com') {
      const prefix = `/${site.project}/`;
      if (path.length <= prefix.length || path.slice(0, prefix.length).toLowerCase() !== prefix.toLowerCase()) return null;
      const rest = path.slice(prefix.length);
      return atRef(rest.startsWith('refs/heads/') ? rest.slice(11) : rest, site, revision);
    }
    return host === site.host ? projectPath(path, site, revision) : null;
  }
  const cut = written.search(/[?#]/);
  const path = decodeOnce(cut === -1 ? written : written.slice(0, cut));
  if (path === null) return null;
  if (site.github && path.startsWith('../')) {
    if (path.startsWith('../blob/')) return atRef(path.slice(8), site, revision);
    if (path.startsWith('../raw/')) return atRef(path.slice(7), site, revision);
  }
  if (path.startsWith('/')) {
    const project = projectPath(path, site, revision);
    if (project) return project;
  }
  const clean = cleanRepositoryPath(path);
  if (clean === null) return null;
  const head = revision().head;
  return head === '' ? null : { commit: head, path: clean };
}
