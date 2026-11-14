import { describe, expect, it } from 'vitest';
import GitBranchIcon from '@lucide/svelte/icons/git-branch';
import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
import TagIcon from '@lucide/svelte/icons/tag';
import { checkoutDisplay, checkoutIcon } from './checkoutDisplay';

const SHA = 'abcdef0123456789abcdef0123456789abcdef01';

describe('checkoutDisplay', () => {
  it('names a branch first', () => {
    expect(checkoutDisplay({ branch: 'main', tag: 'v1', headCommit: SHA })).toEqual({
      kind: 'branch',
      name: 'main',
      label: 'main',
    });
  });

  it('names the tag at a detached HEAD', () => {
    expect(checkoutDisplay({ branch: '', tag: 'v2.4.0', headCommit: SHA })).toEqual({
      kind: 'tag',
      name: 'v2.4.0',
      label: 'v2.4.0',
    });
  });

  it('names a detached commit by its short sha', () => {
    expect(checkoutDisplay({ branch: '', headCommit: SHA })).toEqual({
      kind: 'commit',
      name: 'abcdef0',
      label: 'detached at abcdef0',
    });
  });

  it('reads "No branch" when nothing is known', () => {
    expect(checkoutDisplay({})).toEqual({ kind: 'none', name: '', label: 'No branch' });
  });

  it('pairs each kind with its icon', () => {
    expect(checkoutIcon('branch')).toBe(GitBranchIcon);
    expect(checkoutIcon('tag')).toBe(TagIcon);
    expect(checkoutIcon('commit')).toBe(GitCommitHorizontal);
    expect(checkoutIcon('none')).toBe(GitBranchIcon);
  });
});
