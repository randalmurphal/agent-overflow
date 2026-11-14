// Wire refs. Every field that carries a branch name may carry a tag: a
// branch is its short name, a tag is `refs/tags/<name>`, and a branch whose
// own name starts with `refs/` is spelled `refs/heads/<name>` so it cannot
// read as anything else. This module is the only frontend code that spells
// either prefix.

import type { GitStatus } from '../types/git';

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

/**
 * The ref a checkout is on: its branch, else the tag at its detached HEAD
 * (`GitStatus.tag`), else '' (a detached HEAD at no tag, which the backend
 * reads as that HEAD).
 */
export function checkoutRef(
  branch: string | null | undefined,
  tag: string | null | undefined,
): string {
  if (branch) return branchRef(branch);
  return tag ? tagRef(tag) : '';
}

/** What a checkout is on: a branch, else a tag or commit at a detached HEAD. */
export interface CurrentCheckout {
  branch: string;
  tag: string;
  /** Full commit id of a detached HEAD. */
  headCommit: string;
}

/**
 * The one rule for what a thread's checkout is on. Once the workspace's live
 * status has loaded it is the answer, so a branch switch or detach made
 * outside this client shows at once, draft placeholders included (they have
 * no row for the backend to re-branch). Before that, the thread's cached
 * branch stands in.
 */
export function currentCheckout(
  threadBranch: string | null | undefined,
  status: Pick<GitStatus, 'isRepo' | 'branch' | 'tag' | 'headCommit'> | null | undefined,
): CurrentCheckout {
  if (status?.isRepo) {
    const branch = status.branch ?? '';
    return {
      branch,
      tag: branch ? '' : (status.tag ?? ''),
      headCommit: branch ? '' : (status.headCommit ?? ''),
    };
  }
  return { branch: threadBranch ?? '', tag: '', headCommit: '' };
}

/** `currentCheckout` as a wire ref (see `checkoutRef`). */
export function currentCheckoutRef(checkout: CurrentCheckout): string {
  return checkoutRef(checkout.branch, checkout.tag);
}
