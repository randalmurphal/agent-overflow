import { beforeEach, describe, expect, it } from 'vitest';
import { leftBehindMessage, warnLeftBehind } from './gitLeftBehind';
import { getToasts, resetToastsForTest } from './toast.svelte';

const SHA = '0123456789abcdef0123456789abcdef01234567';

describe('gitLeftBehind', () => {
  beforeEach(() => resetToastsForTest());

  it('names the count and the short sha to branch from', () => {
    expect(
      leftBehindMessage({ workspacePath: '/r', branch: '', leftBehind: { count: 2, commit: SHA } }),
    ).toBe('Left 2 commits on no branch at 01234567. Create a branch from 01234567 to keep them.');
    expect(
      leftBehindMessage({ workspacePath: '/r', branch: '', leftBehind: { count: 1, commit: SHA } }),
    ).toContain('Left 1 commit on no branch');
  });

  it('says nothing when nothing was left behind', () => {
    expect(leftBehindMessage(null)).toBeNull();
    expect(leftBehindMessage({ workspacePath: '/r', branch: 'main' })).toBeNull();
    expect(
      leftBehindMessage({ workspacePath: '/r', branch: '', leftBehind: { count: 0, commit: SHA } }),
    ).toBeNull();
  });

  it('raises a warning toast only for left-behind commits', () => {
    warnLeftBehind({ workspacePath: '/r', branch: 'main' });
    expect(getToasts()).toHaveLength(0);
    warnLeftBehind({ workspacePath: '/r', branch: 'main', leftBehind: { count: 3, commit: SHA } });
    expect(getToasts()).toEqual([
      expect.objectContaining({ type: 'warning', message: expect.stringContaining('01234567') }),
    ]);
  });
});
