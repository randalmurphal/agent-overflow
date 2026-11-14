import { describe, expect, it } from 'vitest';
import GitBranchIcon from '@lucide/svelte/icons/git-branch';
import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
import TagIcon from '@lucide/svelte/icons/tag';
import {
  checkoutDisplay,
  checkoutIcon,
  checkoutOf,
  checkoutRef,
  currentCheckout,
  isDetached,
  refCheckout,
} from './checkout';

const SHA = 'abcdef0123456789abcdef0123456789abcdef01';

describe('checkout', () => {
  it('is its branch, else the tag at HEAD, else the detached commit', () => {
    expect(checkoutOf({ branch: 'main', tag: 'v1', headCommit: SHA })).toEqual({ branch: 'main', tag: '', headCommit: '' });
    expect(checkoutOf({ branch: '', tag: 'v1', headCommit: SHA })).toEqual({ branch: '', tag: 'v1', headCommit: SHA });
    expect(checkoutOf({ branch: null, headCommit: SHA })).toEqual({ branch: '', tag: '', headCommit: SHA });
  });

  it('takes the current checkout from live status once it has loaded', () => {
    const live = { isRepo: true, branch: '', tag: 'v2', headCommit: 'abc' };
    expect(currentCheckout('main', live)).toEqual({ branch: '', tag: 'v2', headCommit: 'abc' });
    expect(currentCheckout('main', { isRepo: true, branch: 'feature', headCommit: '' }))
      .toEqual({ branch: 'feature', tag: '', headCommit: '' });
    // Before status loads, the cached branch stands in.
    expect(currentCheckout('main', null)).toEqual({ branch: 'main', tag: '', headCommit: '' });
    expect(currentCheckout('main', { isRepo: false, branch: '' })).toEqual({ branch: 'main', tag: '', headCommit: '' });
  });

  it('spells the checkout as a wire ref', () => {
    expect(checkoutRef(checkoutOf({ branch: 'main', tag: 'v1' }))).toBe('main');
    expect(checkoutRef(checkoutOf({ tag: 'v1', headCommit: SHA }))).toBe('refs/tags/v1');
    expect(checkoutRef(checkoutOf({ headCommit: SHA }))).toBe('');
    expect(checkoutRef(checkoutOf({}))).toBe('');
    expect(checkoutRef(checkoutOf({ branch: 'refs/tags/v1.0' }))).toBe('refs/heads/refs/tags/v1.0');
  });

  it('is detached only on a tag or a commit', () => {
    expect(isDetached(checkoutOf({ branch: 'main', headCommit: SHA }))).toBe(false);
    expect(isDetached(checkoutOf({ tag: 'v1' }))).toBe(true);
    expect(isDetached(checkoutOf({ headCommit: SHA }))).toBe(true);
    expect(isDetached(checkoutOf({}))).toBe(false);
  });

  it('displays a branch, a tag, a detached commit and nothing', () => {
    expect(checkoutDisplay(checkoutOf({ branch: 'main', tag: 'v1', headCommit: SHA }))).toEqual({
      kind: 'branch', name: 'main', label: 'main', title: 'Branch: main',
    });
    expect(checkoutDisplay(checkoutOf({ tag: 'v2.4.0', headCommit: SHA }))).toEqual({
      kind: 'tag', name: 'v2.4.0', label: 'v2.4.0', title: 'Tag: v2.4.0',
    });
    expect(checkoutDisplay(checkoutOf({ headCommit: SHA }))).toEqual({
      kind: 'commit', name: 'abcdef0', label: 'detached at abcdef0', title: 'Commit: abcdef0',
    });
    expect(checkoutDisplay(checkoutOf({}))).toEqual({ kind: 'none', name: '', label: 'No branch', title: 'No branch' });
  });

  it('reads a staged wire ref back as the checkout it names', () => {
    expect(refCheckout('refs/tags/v1')).toEqual(checkoutOf({ tag: 'v1' }));
    expect(refCheckout('feature')).toEqual(checkoutOf({ branch: 'feature' }));
    expect(refCheckout('refs/heads/refs/tags/v1')).toEqual(checkoutOf({ branch: 'refs/tags/v1' }));
  });

  it('pairs each kind with its icon', () => {
    expect(checkoutIcon('branch')).toBe(GitBranchIcon);
    expect(checkoutIcon('tag')).toBe(TagIcon);
    expect(checkoutIcon('commit')).toBe(GitCommitHorizontal);
    expect(checkoutIcon('none')).toBe(GitBranchIcon);
  });
});
