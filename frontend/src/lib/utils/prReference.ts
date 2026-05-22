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
  // The path-segment chain before the repo. Single segment for github
  // (the owner) or the empty string when no namespace; arbitrary depth
  // for GitLab subgroups.
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
const GITHUB_URL_PATTERN = /^(?:https?:\/\/)?github\.com\/([^/]+)\/([^/\s]+)\/pull\/(\d+)(?:[/?#].*)?$/;
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
    `^(?:https?:\\/\\/)?(?:${alternation})\\/((?:[^/\\s]+\\/)+[^/\\s]+)\\/-\\/merge_requests\\/(\\d+)(?:[/?#].*)?$`,
  );
}

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
    return parseMatch('github', match[1], match[2], match[3]);
  }

  const gitlabUrlPattern = gitlabUrlPatternFor(opts.gitlabHosts ?? []);
  match = gitlabUrlPattern.exec(trimmed);
  if (match) {
    const { namespace, repo } = splitNamespacePath(match[1]);
    return parseMatch('gitlab', namespace, repo, match[2]);
  }

  match = GITHUB_SHORT_PATTERN.exec(trimmed);
  if (match) {
    return parseMatch('github', match[1], match[2], match[3]);
  }

  match = GITLAB_SHORT_PATTERN.exec(trimmed);
  if (match) {
    const { namespace, repo } = splitNamespacePath(match[1]);
    return parseMatch('gitlab', namespace, repo, match[2]);
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

function parseMatch(forge: Forge, namespace: string, repo: string, numberStr: string): PRReferenceResult {
  const number = Number.parseInt(numberStr, 10);
  if (!Number.isFinite(number) || number <= 0) {
    return { ok: false, error: `PR/MR number must be a positive integer, got "${numberStr}"` };
  }
  return { ok: true, value: { forge, namespace, repo, number } };
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
