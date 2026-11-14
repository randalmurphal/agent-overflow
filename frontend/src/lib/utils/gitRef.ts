// Wire refs. Every field that carries a branch name may carry a tag: a
// branch is its short name, a tag is `refs/tags/<name>`, and a branch whose
// own name starts with `refs/` is spelled `refs/heads/<name>` so it cannot
// read as anything else. This module is the only frontend code that spells
// either prefix.

const TAG_PREFIX = 'refs/tags/';
const BRANCH_PREFIX = 'refs/heads/';

export function tagRef(name: string): string {
  return `${TAG_PREFIX}${name}`;
}

/** A branch name as a wire ref. */
export function branchRef(name: string): string {
  return name.startsWith('refs/') ? `${BRANCH_PREFIX}${name}` : name;
}

export function isTagRef(ref: string | null | undefined): boolean {
  return !!ref && ref.startsWith(TAG_PREFIX) && ref.length > TAG_PREFIX.length;
}

/** The name a person reads: a branch or tag without its wire prefix. */
export function refLabel(ref: string): string {
  if (isTagRef(ref)) return ref.slice(TAG_PREFIX.length);
  return ref.startsWith(BRANCH_PREFIX) ? ref.slice(BRANCH_PREFIX.length) : ref;
}
