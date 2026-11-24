import { describe, expect, it } from 'vitest';
import {
  disambiguatedPathLabels,
  formatPathLabel,
  pathBasename,
} from './pathDisplay';

describe('disambiguatedPathLabels', () => {
  it('leaves unique names bare', () => {
    const labels = disambiguatedPathLabels([
      { id: 'a', name: 'web', path: '/work/web' },
      { id: 'b', name: 'api', path: '/work/api' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: '', name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: '', name: 'api' });
  });

  it.each([
    ['adds one parent segment for a simple duplicate', '/work/clients/web', '/work/personal/web', 'clients', 'personal'],
    ['deepens the prefix only until the group is distinct', '/alpha/src/web', '/beta/src/web', '/alpha/src', '/beta/src'],
    ['handles Windows separators', 'C:\\repos\\clients\\web', 'C:\\repos\\personal\\web', 'clients', 'personal'],
  ])('%s', (_name, a, b, prefixA, prefixB) => {
    const labels = disambiguatedPathLabels([
      { id: 'a', name: 'web', path: a },
      { id: 'b', name: 'web', path: b },
    ]);
    expect(labels.get('a')).toEqual({ prefix: prefixA, name: 'web' });
    expect(labels.get('b')).toEqual({ prefix: prefixB, name: 'web' });
  });

  it.each([
    ['/Users/randy/repos/app', '/home/randy/repos/app', '/Users/randy/repos/app', '/home/randy/repos/app'],
    ['C:\\repos\\app', 'D:\\repos\\app', 'C:/repos/app', 'D:/repos/app'],
    ['\\\\one\\share\\app', '\\\\two\\share\\app', '//one/share/app', '//two/share/app'],
    ['/app', '/work/app', '/app', '/work/app'],
    ['/work/app', 'work/app', '/work/app', 'work/app'],
  ])('preserves roots when the complete path is needed: %s', (a, b, expectedA, expectedB) => {
    const labels = disambiguatedPathLabels([{ id: 'a', name: 'app', path: a }, { id: 'b', name: 'app', path: b }]);
    expect(formatPathLabel(labels.get('a')!)).toBe(expectedA);
    expect(formatPathLabel(labels.get('b')!)).toBe(expectedB);
  });

  it('keeps the real dir name in the prefix for a renamed project', () => {
    // Both display as "svc" but only b's dir is actually named svc.
    const labels = disambiguatedPathLabels([
      { id: 'a', name: 'svc', path: '/work/backend' },
      { id: 'b', name: 'svc', path: '/work/svc' },
    ]);
    expect(labels.get('a')).toEqual({ prefix: 'backend', name: 'svc' });
    expect(labels.get('b')).toEqual({ prefix: '/work', name: 'svc' });
  });

  it('disambiguates three-way groups', () => {
    const labels = disambiguatedPathLabels([
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
    const labels = disambiguatedPathLabels([
      { id: 'a', name: 'web', path: '/work/web' },
      { id: 'b', name: 'web', path: '/work' },
    ]);
    expect(labels.get('a')!.prefix).not.toBe('');
    expect(labels.get('b')!.prefix).not.toBe('');
  });
});

describe('formatPathLabel', () => {
  it('joins prefix and name with a slash', () => {
    expect(formatPathLabel({ prefix: 'clients', name: 'web' })).toBe('clients/web');
    expect(formatPathLabel({ prefix: '', name: 'web' })).toBe('web');
  });
});

describe('pathBasename', () => {
  it.each<[string, string | undefined, string]>([
    ['returns the final path segment', '/tmp/worktrees/feature-demo', 'feature-demo'],
    ['ignores trailing separators', '/tmp/worktrees/feature-demo///', 'feature-demo'],
    ['handles Windows separators', 'C:\\worktrees\\feature-demo', 'feature-demo'],
    ['returns an empty string for a missing path', undefined, ''],
    ['returns an empty string for an empty path', '', ''],
    ['returns an empty string for a root-only path', '/', ''],
  ])('%s', (_name, path, expected) => {
    expect(pathBasename(path)).toBe(expected);
  });
});
