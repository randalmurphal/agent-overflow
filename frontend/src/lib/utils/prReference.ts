// PR/MR references: the identity the frontend keys a PR by, and its wire
// shape.

export type Forge = 'github' | 'gitlab';

export interface PRRef {
  forge: Forge;
  // The forge host the PR lives on, from its URL (`URL.host`: lowercase,
  // a non-default port kept). The backend refuses a reference without one.
  host: string;
  namespace: string;
  repo: string;
  number: number;
}

/**
 * The reference for a PR the forge reported by URL (a workspace's open PR).
 * The host is the URL's, so a self-hosted GitLab MR keeps the host it
 * lives on.
 */
export function prRefFromUrl(forge: string, url: string, number: number): PRRef | null {
  if (forge !== 'github' && forge !== 'gitlab') return null;
  if (!Number.isFinite(number) || number <= 0) return null;
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return null;
  }
  const host = parsed.host;
  if (host === '') return null;
  const parts = parsed.pathname.split('/').filter(Boolean);
  if (forge === 'github') {
    if (parts.length < 4 || parts[2] !== 'pull') return null;
    const n = Number.parseInt(parts[3] ?? '', 10);
    if (n !== number) return null;
    return { forge, host, namespace: parts[0], repo: parts[1], number };
  }
  const sep = parts.indexOf('-');
  if (sep < 2 || parts[sep + 1] !== 'merge_requests') return null;
  const n = Number.parseInt(parts[sep + 2] ?? '', 10);
  if (n !== number) return null;
  const project = parts.slice(0, sep);
  return {
    forge,
    host,
    namespace: project.slice(0, -1).join('/'),
    repo: project[project.length - 1] ?? '',
    number,
  };
}

export function prScopeLabel(ref: PRRef): string {
  return ref.forge === 'gitlab' ? `MR !${ref.number}` : `PR #${ref.number}`;
}

// The host a PR key leaves implicit, per forge.
const PUBLIC_FORGE_HOST: Record<Forge, string> = { github: 'github.com', gitlab: 'gitlab.com' };

/**
 * The entity key for a pull/merge request: `<forge>:<namespace>/<repo>:<n>`
 * on the forge's public host, `<forge>@<host>:<namespace>/<repo>:<n>` on any
 * other.
 *
 * One spelling of "which PR" everywhere it is needed: the PR-review store's
 * key, the `pr:updated` wire address (`PRReference.Key` in
 * internal/git/forge.go builds the identical string), and, with the `pr:`
 * prefix below, the review pane's comment sourceKey. Keys are compared,
 * never parsed.
 */
export function prKey(ref: PRRef): string {
  return `${repoKey(ref)}:${ref.number}`;
}

/** A repository on its forge host: a PR reference without its number. */
export type RepoRef = Omit<PRRef, 'number'>;

/**
 * The entity key for a repository: the PR key without its number
 * (`RepoReference.Key` in internal/git/repo_reference.go builds the
 * identical string). Compared, never parsed.
 */
export function repoKey(ref: RepoRef): string {
  const project = `${ref.namespace}/${ref.repo}`;
  if (ref.host === PUBLIC_FORGE_HOST[ref.forge]) return `${ref.forge}:${project}`;
  return `${ref.forge}@${ref.host}:${project}`;
}

/**
 * Comment sourceKey for pr scope. Stable across PR head movement: drafts
 * must survive pushes, and each draft's commitSha records the head it was
 * anchored to.
 */
export function prSourceKey(ref: PRRef): string {
  return `pr:${prKey(ref)}`;
}

/** The wire shape the Go `git.PRReference` parameter expects. */
export interface PRReferenceWire {
  Forge: string;
  Host: string;
  Namespace: string;
  Repo: string;
  Number: number;
}

/** The wire shape the Go `git.RepoReference` parameter expects. */
export interface RepoReferenceWire {
  Forge: string;
  Host: string;
  Namespace: string;
  Repo: string;
}

export function repoReferenceWire(ref: RepoRef): RepoReferenceWire {
  return { Forge: ref.forge, Host: ref.host, Namespace: ref.namespace, Repo: ref.repo };
}

/** A wire repository (a listed project's, a list row's) as a RepoRef; null
 * for a forge this app does not speak. */
export function repoRefFromWire(wire: { Forge: string; Host: string; Namespace: string; Repo: string }): RepoRef | null {
  if (wire.Forge !== 'github' && wire.Forge !== 'gitlab') return null;
  return { forge: wire.Forge, host: wire.Host, namespace: wire.Namespace, repo: wire.Repo };
}

/** A wire PR reference as a PRRef; null for a forge this app does not speak. */
export function prRefFromWire(wire: PRReferenceWire): PRRef | null {
  const repo = repoRefFromWire(wire);
  return repo ? { ...repo, number: wire.Number } : null;
}

export function prReferenceWire(ref: PRRef): PRReferenceWire {
  return {
    Forge: ref.forge,
    Host: ref.host,
    Namespace: ref.namespace,
    Repo: ref.repo,
    Number: ref.number,
  };
}
