import { beforeEach, describe, expect, it } from 'vitest';
import { warnLeftBehind } from './gitLeftBehind';
import { getToasts, resetToastsForTest } from './toast.svelte';

const SHA = '0123456789abcdef0123456789abcdef01234567';

describe('gitLeftBehind', () => {
  beforeEach(() => resetToastsForTest());

  it('names the count and the short sha the detached label shows', () => {
    warnLeftBehind({ workspacePath: '/r', branch: '', leftBehind: { count: 2, commit: SHA } });
    warnLeftBehind({ workspacePath: '/r', branch: '', leftBehind: { count: 1, commit: SHA } });
    expect(getToasts().map((t) => [t.type, t.message])).toEqual([
      ['warning', 'Left 2 commits on no branch at 0123456. Create a branch from 0123456 to keep them.'],
      ['warning', 'Left 1 commit on no branch at 0123456. Create a branch from 0123456 to keep them.'],
    ]);
  });

  it('says nothing when nothing was left behind', () => {
    warnLeftBehind(null);
    warnLeftBehind({ workspacePath: '/r', branch: 'main' });
    warnLeftBehind({ workspacePath: '/r', branch: '', leftBehind: { count: 0, commit: SHA } });
    warnLeftBehind({ workspacePath: '/r', branch: '', leftBehind: { count: 2, commit: '' } });
    expect(getToasts()).toHaveLength(0);
  });
});
