// How a checkout is named wherever the app shows it: the workspace strip's
// branch trigger, the compact header's branch segment and the worktree
// picker's rows. A branch, else the tag at a detached HEAD, else the
// detached commit, else "No branch" (status not loaded, an unborn
// repository, or a backend that reports none of these).

import GitBranchIcon from '@lucide/svelte/icons/git-branch';
import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
import TagIcon from '@lucide/svelte/icons/tag';
import type { LucideProps } from '@lucide/svelte';
import type { Component } from 'svelte';

export type CheckoutKind = 'branch' | 'tag' | 'commit' | 'none';

export interface CheckoutFacts {
  branch?: string | null;
  tag?: string | null;
  /** Full commit id of a detached HEAD. */
  headCommit?: string | null;
}

export interface CheckoutDisplay {
  kind: CheckoutKind;
  /** The bare name: branch, tag or short sha; '' for none. */
  name: string;
  /** What the label reads. */
  label: string;
}

const SHORT_SHA_LENGTH = 7;

export function shortSha(commit: string): string {
  return commit.slice(0, SHORT_SHA_LENGTH);
}

export function checkoutDisplay(facts: CheckoutFacts): CheckoutDisplay {
  if (facts.branch) return { kind: 'branch', name: facts.branch, label: facts.branch };
  if (facts.tag) return { kind: 'tag', name: facts.tag, label: facts.tag };
  if (facts.headCommit) {
    const sha = shortSha(facts.headCommit);
    return { kind: 'commit', name: sha, label: `detached at ${sha}` };
  }
  return { kind: 'none', name: '', label: 'No branch' };
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
