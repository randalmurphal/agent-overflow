// Pure URL/short-form parser for GitHub PR references. Kept free of Svelte
// + binding imports so tests can run it head-less.
//
// Accepted shapes (whitespace trimmed):
//   https://github.com/OWNER/REPO/pull/N
//   http://github.com/OWNER/REPO/pull/N
//   github.com/OWNER/REPO/pull/N
//   OWNER/REPO#N
//
// Anything else yields a structured error that the UI can show verbatim.

export interface ParsedPRReference {
  owner: string;
  repo: string;
  number: number;
}

export type PRReferenceResult =
  | { ok: true; value: ParsedPRReference }
  | { ok: false; error: string };

// Anchored patterns match the backend parser in app_thread_from_pr.go. Keep
// the two in sync — the backend validates again, but the UI should reject
// obvious garbage without a round-trip.
const URL_PATTERN = /^(?:https?:\/\/)?github\.com\/([^/]+)\/([^/]+)\/pull\/(\d+)(?:[/?#].*)?$/;
const SHORT_PATTERN = /^([^/\s]+)\/([^/\s#]+)#(\d+)$/;

export function parsePRReference(input: string): PRReferenceResult {
  const trimmed = input.trim();
  if (trimmed === '') {
    return { ok: false, error: 'PR reference is empty' };
  }

  const urlMatch = URL_PATTERN.exec(trimmed);
  if (urlMatch) {
    return parseMatch(urlMatch[1], urlMatch[2], urlMatch[3]);
  }

  const shortMatch = SHORT_PATTERN.exec(trimmed);
  if (shortMatch) {
    return parseMatch(shortMatch[1], shortMatch[2], shortMatch[3]);
  }

  return {
    ok: false,
    error:
      `Unrecognised PR reference: expected https://github.com/OWNER/REPO/pull/N or OWNER/REPO#N`,
  };
}

function parseMatch(owner: string, repo: string, numberStr: string): PRReferenceResult {
  const number = Number.parseInt(numberStr, 10);
  if (!Number.isFinite(number) || number <= 0) {
    return { ok: false, error: `PR number must be a positive integer, got "${numberStr}"` };
  }
  return { ok: true, value: { owner, repo, number } };
}
