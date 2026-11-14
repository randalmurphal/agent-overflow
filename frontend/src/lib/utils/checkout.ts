// What a checkout is on, and how the app names it. A checkout is its
// branch, else the tag at its detached HEAD, else the detached commit, else
// nothing known ("No branch": status not loaded, an unborn repository, or a
// backend that reports none of these). This module is the only place that
// rule is written.

import GitBranchIcon from '@lucide/svelte/icons/git-branch';
import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
import TagIcon from '@lucide/svelte/icons/tag';
import type { LucideProps } from '@lucide/svelte';
import type { Component } from 'svelte';
import type { GitStatus } from '../types/git';
import type { Thread } from '../types/models';
import { branchRef, isTagRef, refLabel, tagRef } from './gitRef';

/** What a checkout is on. `tag` and `headCommit` are '' on a branch. */
export interface Checkout {
  branch: string;
  tag: string;
  /** Full commit id of a detached HEAD. */
  headCommit: string;
}

/** The facts a status, worktree or mutation answer reports. */
export interface CheckoutFacts {
  branch?: string | null;
  tag?: string | null;
  headCommit?: string | null;
}

export function checkoutOf(facts: CheckoutFacts): Checkout {
  const branch = facts.branch ?? '';
  if (branch) return { branch, tag: '', headCommit: '' };
  return { branch: '', tag: facts.tag ?? '', headCommit: facts.headCommit ?? '' };
}

/**
 * What a thread's checkout is on. Once the workspace's live status has
 * loaded it is the answer, so a branch switch or detach made outside this
 * client shows at once, draft placeholders included (they have no row for
 * the backend to re-branch). Before that, the thread's cached branch stands
 * in.
 */
export function currentCheckout(
  threadBranch: string | null | undefined,
  status: Pick<GitStatus, 'isRepo' | 'branch' | 'tag' | 'headCommit'> | null | undefined,
): Checkout {
  return status?.isRepo ? checkoutOf(status) : checkoutOf({ branch: threadBranch });
}

/** What a pane reads its checkout from: the thread row and the live status. */
export interface PaneCheckoutSource {
  readonly thread: Thread | null;
  readonly gitStatus: { readonly status: GitStatus | null };
}

/** The pane's current checkout. Reactive. */
export function paneCheckout(pane: PaneCheckoutSource): Checkout {
  return currentCheckout(pane.thread?.branch, pane.gitStatus.status);
}

/**
 * The checkout as a wire ref (utils/gitRef.ts): its branch, else its tag,
 * else '' for a detached HEAD at no tag.
 */
export function checkoutRef(checkout: Checkout): string {
  if (checkout.branch) return branchRef(checkout.branch);
  return checkout.tag ? tagRef(checkout.tag) : '';
}

/** A detached HEAD: on a tag or a commit, not a branch. */
export function isDetached(checkout: Checkout): boolean {
  return !checkout.branch && !!(checkout.tag || checkout.headCommit);
}

export type CheckoutKind = 'branch' | 'tag' | 'commit' | 'none';

export interface CheckoutDisplay {
  kind: CheckoutKind;
  /** The bare name: branch, tag or short sha; '' for none. */
  name: string;
  /** What the label reads. */
  label: string;
  /** The label's tooltip, naming the kind. */
  title: string;
}

const SHORT_SHA_LENGTH = 7;

/** A commit id as the app shows it. */
export function shortSha(commit: string): string {
  return commit.slice(0, SHORT_SHA_LENGTH);
}

export function checkoutDisplay(checkout: Checkout): CheckoutDisplay {
  if (checkout.branch) {
    return { kind: 'branch', name: checkout.branch, label: checkout.branch, title: `Branch: ${checkout.branch}` };
  }
  if (checkout.tag) {
    return { kind: 'tag', name: checkout.tag, label: checkout.tag, title: `Tag: ${checkout.tag}` };
  }
  if (checkout.headCommit) {
    const sha = shortSha(checkout.headCommit);
    return { kind: 'commit', name: sha, label: `detached at ${sha}`, title: `Commit: ${sha}` };
  }
  return { kind: 'none', name: '', label: 'No branch', title: 'No branch' };
}

/** A staged wire ref (a branch or a tag) as it would be displayed checked out. */
export function refCheckout(ref: string): Checkout {
  return isTagRef(ref) ? checkoutOf({ tag: refLabel(ref) }) : checkoutOf({ branch: refLabel(ref) });
}

const ICONS: Record<CheckoutKind, Component<LucideProps>> = {
  branch: GitBranchIcon,
  tag: TagIcon,
  commit: GitCommitHorizontal,
  none: GitBranchIcon,
};

export function checkoutIcon(kind: CheckoutKind): Component<LucideProps> {
  return ICONS[kind];
}
