import { describe, expect, it } from 'vitest';
import {
  disambiguatedProjectLabels,
  formatProjectLabel,
  pathBasename,
} from './pathDisplay';

describe('disambiguatedProjectLabels', () => {
  it('leaves unique names bare', () => {
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: '/work/web' },
      { id: 'b', name: 'api', path: '/work/api' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: '', name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: '', name: 'api' });
  });

  it('adds one parent segment for a simple duplicate', () => {
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: '/work/clients/web' },
      { id: 'b', name: 'web', path: '/work/personal/web' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: 'clients', name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: 'personal', name: 'web' });
  });

  it('deepens the prefix only until the group is distinct', () => {
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: '/alpha/src/web' },
      { id: 'b', name: 'web', path: '/beta/src/web' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: 'alpha/src', name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: 'beta/src', name: 'web' });
  });

  it('keeps the real dir name in the prefix for a renamed project', () => {
    // Both display as "svc" but only b's dir is actually named svc.
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'svc', path: '/work/backend' },
      { id: 'b', name: 'svc', path: '/work/svc' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: 'backend', name: 'svc' });
    expect(labels.get('b')).toEqual({ prefix: 'work', name: 'svc' });
  });

  it('handles Windows separators', () => {
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: 'C:\\repos\\clients\\web' },
      { id: 'b', name: 'web', path: 'C:\\repos\\personal\\web' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: 'clients', name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: 'personal', name: 'web' });
  });

  it('disambiguates three-way groups', () => {
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: '/x/web' },
      { id: 'b', name: 'web', path: '/y/web' },
      { id: 'c', name: 'web', path: '/z/web' },
    ]);
    expect(new Set([labels.get('a')!.prefix, labels.get('b')!.prefix, labels.get('c')!.prefix]).size)
      .toBe(3);
  });

  it('stamps the deepest available prefix when a tie cannot be broken', () => {
    // Pathological: same parents, one renamed so its basename got popped
    // and the remainder equals the other's parents. Better a tied prefix
    // than a bare ambiguous name.
    const labels = disambiguatedProjectLabels([
      { id: 'a', name: 'web', path: '/work/web' },
      { id: 'b', name: 'web', path: '/work' },
    ]);
    expect(labels.get('a')!.prefix).not.toBe('');
    expect(labels.get('b')!.prefix).not.toBe('');
  });
});

describe('formatProjectLabel', () => {
  it('joins prefix and name with a slash', () => {
    expect(formatProjectLabel({ prefix: 'clients', name: 'web' })).toBe('clients/web');
    expect(formatProjectLabel({ prefix: '', name: 'web' })).toBe('web');
  });
});

describe('pathBasename', () => {
  it('returns the final path segment', () => {
    expect(pathBasename('/tmp/worktrees/feature-demo')).toBe('feature-demo');
  });

  it('ignores trailing separators', () => {
    expect(pathBasename('/tmp/worktrees/feature-demo///')).toBe('feature-demo');
  });

  it('handles Windows separators', () => {
    expect(pathBasename('C:\\worktrees\\feature-demo')).toBe('feature-demo');
  });

  it('returns an empty string for empty or root-only paths', () => {
    expect(pathBasename(undefined)).toBe('');
    expect(pathBasename('')).toBe('');
    expect(pathBasename('/')).toBe('');
  });
});
