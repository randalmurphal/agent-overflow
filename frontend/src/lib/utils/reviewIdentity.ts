// Display identity for forge authors on the review surfaces. The forge
// gives a login always and a display name when it knows one (Go's
// `authorName`); surfaces show the name and keep the login as the
// secondary line, the way GitHub and GitLab present an author.

export interface ReviewAuthor {
  authorLogin: string;
  authorName?: string;
}

/** The name to show in bold: the display name, else the login. */
export function authorDisplayName(author: ReviewAuthor): string {
  const name = author.authorName?.trim() ?? '';
  return name !== '' ? name : author.authorLogin;
}

/** Two letters for the avatar disc: the first letters of the first two
 * words of the display name (`Randy Murphy` → RM, `coderabbit-api-dev`
 * → CA), else the first two characters. */
export function authorInitials(author: ReviewAuthor): string {
  const source = authorDisplayName(author);
  const parts = source.split(/[\s\-_.]+/).filter((part) => part !== '');
  const chars = parts.length >= 2
    ? [Array.from(parts[0])[0], Array.from(parts[1])[0]]
    : Array.from(parts[0] ?? source).slice(0, 2);
  return chars.join('').toUpperCase();
}

export const AUTHOR_TONE_COUNT = 7;

/** A stable tone index for an author's avatar, from the login so the
 * same person keeps one color across PRs. */
export function authorTone(login: string): number {
  let hash = 2166136261;
  for (const char of login) {
    hash ^= char.codePointAt(0) ?? 0;
    hash = Math.imul(hash, 16777619) >>> 0;
  }
  return hash % AUTHOR_TONE_COUNT;
}

/** Whether a login names an automated account: GitHub's `[bot]` suffix,
 * GitLab's `group_<id>_bot_<hash>` project tokens, `-bot` names. */
export function isBotLogin(login: string): boolean {
  return /\[bot\]$|(^|[-_])bot([-_]|$)/i.test(login);
}
