import { describe, expect, it } from 'vitest';
import { branchRef, checkoutRef, currentCheckout, currentCheckoutRef, isTagRef, refLabel, tagRef } from './gitRef';

describe('gitRef', () => {
  it('round-trips a tag name through its wire ref', () => {
    expect(tagRef('v2.4.0')).toBe('refs/tags/v2.4.0');
    expect(isTagRef(tagRef('v2.4.0'))).toBe(true);
    expect(refLabel(tagRef('v2.4.0'))).toBe('v2.4.0');
  });

  it('treats a branch name, even one shaped like a tag name, as a branch', () => {
    expect(isTagRef('v2.4.0')).toBe(false);
    expect(isTagRef('feature/tags/x')).toBe(false);
    expect(isTagRef('refs/tags/')).toBe(false);
    expect(isTagRef('')).toBe(false);
    expect(isTagRef(undefined)).toBe(false);
    expect(refLabel('feature/x')).toBe('feature/x');
  });

  it('resolves a checkout to its branch, else its tag, else nothing', () => {
    expect(checkoutRef('main', 'v1')).toBe('main');
    expect(checkoutRef('', 'v1')).toBe('refs/tags/v1');
    expect(checkoutRef(undefined, undefined)).toBe('');
    expect(checkoutRef('', '')).toBe('');
  });

  it('spells a branch named like a ref with the branch prefix and reads it back', () => {
    expect(branchRef('main')).toBe('main');
    expect(branchRef('feature/refs/x')).toBe('feature/refs/x');
    expect(branchRef('refs/tags/v1.0')).toBe('refs/heads/refs/tags/v1.0');
    expect(isTagRef(branchRef('refs/tags/v1.0'))).toBe(false);
    expect(refLabel(branchRef('refs/tags/v1.0'))).toBe('refs/tags/v1.0');
    expect(refLabel(branchRef('refs/heads/x'))).toBe('refs/heads/x');
    expect(checkoutRef('refs/tags/v1.0', '')).toBe('refs/heads/refs/tags/v1.0');
  });

  it('takes the current checkout from live status once it has loaded', () => {
    const live = { isRepo: true, branch: '', tag: 'v2', headCommit: 'abc' };
    expect(currentCheckout('main', live)).toEqual({ branch: '', tag: 'v2', headCommit: 'abc' });
    expect(currentCheckoutRef(currentCheckout('main', live))).toBe('refs/tags/v2');
    expect(currentCheckout('main', { isRepo: true, branch: 'feature', headCommit: '' }))
      .toEqual({ branch: 'feature', tag: '', headCommit: '' });
    // Before status loads, the cached branch stands in.
    expect(currentCheckout('main', null)).toEqual({ branch: 'main', tag: '', headCommit: '' });
    expect(currentCheckout('main', { isRepo: false, branch: '' })).toEqual({ branch: 'main', tag: '', headCommit: '' });
  });
});
