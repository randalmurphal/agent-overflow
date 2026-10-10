// Pure URL/short-form parser for PR/MR references. Kept free of Svelte
// + binding imports so tests can run it head-less.
//
// Accepted shapes (whitespace trimmed):
//   GitHub URL:  https://github.com/OWNER/REPO/pull/N (also http:// and bare host)
//   GitHub short: OWNER/REPO#N
//   GitLab URL:  https://gitlab.com/NAMESPACE/REPO/-/merge_requests/N
//                (NAMESPACE may include subgroups: group/sub/.../repo)
//                Self-hosted GitLab hosts from settings are accepted
//                here too — callers pass the configured allowlist via
//                `opts.gitlabHosts`.
//   GitLab short: NAMESPACE/REPO!N (also group/sub/repo!N)
//
// Anything else yields a structured error that the UI can show verbatim.

export type Forge = 'github' | 'gitlab';

export interface ParsedPRReference {
  forge: Forge;
  // The forge host a URL input matched, lowercase (the URL patterns admit
  // no port); '' for a short form, which names no host.
  host: string;
  // The path-segment chain before the repo. Single segment for github
  // (the owner) or the empty string when no namespace; arbitrary depth
  // for GitLab subgroups.
  namespace: string;
  repo: string;
  number: number;
}

export interface PRRef {
  forge: Forge;
  // The forge host the PR lives on, from its URL (`URL.host`: lowercase,
  // a non-default port kept). The backend refuses a reference without one.
  host: string;
  namespace: string;
  repo: string;
  number: number;
}

export type PRReferenceResult =
  | { ok: true; value: ParsedPRReference }
  | { ok: false; error: string };

export interface ParsePRReferenceOptions {
  /**
   * Additional GitLab hostnames to recognise in URL inputs (bare hosts,
   * lowercase). gitlab.com is always recognised; entries here extend
   * the URL parser to also match `https://<host>/.../-/merge_requests/N`.
   * Short refs (`namespace/repo!N`) are forge-neutral and don't need
   * this list.
   */
  gitlabHosts?: string[];
}

// Anchored patterns mirror the backend parser in
// internal/git/forge.go::ParsePRReference. Keep the two in sync — the
// backend validates again, but the UI should reject obvious garbage
// without a round-trip.
const GITHUB_URL_PATTERN = /^(?:https?:\/\/)?(github\.com)\/([^/]+)\/([^/\s]+)\/pull\/(\d+)(?:[/?#].*)?$/;
const GITHUB_SHORT_PATTERN = /^([^/\s]+)\/([^/\s#]+)#(\d+)$/;
const GITLAB_SHORT_PATTERN = /^((?:[^/\s]+\/)+[^/\s!]+)!(\d+)$/;

// Escape regex metacharacters so a hostname can be embedded into an
// anchored URL pattern without surprises. Hosts are pre-normalised by
// settings.validateBareHostname, so only `.` and `-` show up in
// practice, but keep this defensive.
function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function gitlabUrlPatternFor(hosts: string[]): RegExp {
  // Always include gitlab.com first; allow self-hosted hosts after.
  const all = ['gitlab.com', ...hosts];
  const alternation = all.map(escapeRegex).join('|');
  return new RegExp(
    `^(?:https?:\\/\\/)?(${alternation})\\/((?:[^/\\s]+\\/)+[^/\\s]+)\\/-\\/merge_requests\\/(\\d+)(?:[/?#].*)?$`,
  );
}

/**
 * Parse a PR/MR reference typed or pasted by a user. A URL input carries its
 * host; a short form (`OWNER/REPO#N`, `NAMESPACE/REPO!N`) yields `host: ''`
 * and must be given the host of the repository it names before it becomes a
 * `PRRef` or reaches the wire, where an empty host is refused.
 */
export function parsePRReference(
  input: string,
  opts: ParsePRReferenceOptions = {},
): PRReferenceResult {
  const trimmed = input.trim();
  if (trimmed === '') {
    return { ok: false, error: 'Reference is empty' };
  }

  let match = GITHUB_URL_PATTERN.exec(trimmed);
  if (match) {
    return parseMatch('github', match[1], match[2], match[3], match[4]);
  }

  const gitlabUrlPattern = gitlabUrlPatternFor(opts.gitlabHosts ?? []);
  match = gitlabUrlPattern.exec(trimmed);
  if (match) {
    const { namespace, repo } = splitNamespacePath(match[2]);
    return parseMatch('gitlab', match[1].toLowerCase(), namespace, repo, match[3]);
  }

  match = GITHUB_SHORT_PATTERN.exec(trimmed);
  if (match) {
    return parseMatch('github', '', match[1], match[2], match[3]);
  }

  match = GITLAB_SHORT_PATTERN.exec(trimmed);
  if (match) {
    const { namespace, repo } = splitNamespacePath(match[1]);
    return parseMatch('gitlab', '', namespace, repo, match[2]);
  }

  return {
    ok: false,
    error:
      'Unrecognised PR/MR reference: expected ' +
      'https://github.com/OWNER/REPO/pull/N, ' +
      'https://gitlab.com/NAMESPACE/REPO/-/merge_requests/N, ' +
      'OWNER/REPO#N, or NAMESPACE/REPO!N',
  };
}

/**
 * The reference for a PR the forge reported by URL (a workspace's open PR).
 * The host is the URL's, so a GitHub Enterprise or self-hosted GitLab PR
 * keeps the host it lives on.
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
  const project = `${ref.namespace}/${ref.repo}`;
  if (ref.host === PUBLIC_FORGE_HOST[ref.forge]) {
    return `${ref.forge}:${project}:${ref.number}`;
  }
  return `${ref.forge}@${ref.host}:${project}:${ref.number}`;
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

export function prReferenceWire(ref: PRRef): PRReferenceWire {
  return {
    Forge: ref.forge,
    Host: ref.host,
    Namespace: ref.namespace,
    Repo: ref.repo,
    Number: ref.number,
  };
}

function parseMatch(
  forge: Forge,
  host: string,
  namespace: string,
  repo: string,
  numberStr: string,
): PRReferenceResult {
  const number = Number.parseInt(numberStr, 10);
  if (!Number.isFinite(number) || number <= 0) {
    return { ok: false, error: `PR/MR number must be a positive integer, got "${numberStr}"` };
  }
  return { ok: true, value: { forge, host, namespace, repo, number } };
}

function splitNamespacePath(path: string): { namespace: string; repo: string } {
  const parts = path.split('/');
  if (parts.length < 2) {
    return { namespace: '', repo: path };
  }
  return {
    namespace: parts.slice(0, -1).join('/'),
    repo: parts[parts.length - 1],
  };
}
