import { describe, expect, it } from 'vitest';
import { normalizeRemoteURL, repoKey } from './repoKey';

describe('normalizeRemoteURL', () => {
  it('agrees across every spelling git accepts for one remote', () => {
    const want = 'github.com/Owner/Repo';
    for (const url of [
      'https://github.com/Owner/Repo.git',
      'https://user@GitHub.com/Owner/Repo',
      'ssh://git@github.com:22/Owner/Repo.git',
      'git@github.com:Owner/Repo.git',
      'git://github.com/Owner/Repo/',
      '  git@github.com:Owner/Repo  ',
    ]) {
      expect(normalizeRemoteURL(url), url).toBe(want);
    }
  });

  it('keeps the path’s case, because forges disagree on whether it matters', () => {
    expect(normalizeRemoteURL('git@github.com:owner/repo')).not.toBe(
      normalizeRemoteURL('git@github.com:Owner/Repo'),
    );
  });

  it('answers nothing for a remote that is a fact about one machine', () => {
    expect(normalizeRemoteURL('')).toBe('');
    expect(normalizeRemoteURL('/srv/git/repo.git')).toBe('');
    expect(normalizeRemoteURL('file:///srv/git/repo.git')).toBe('');
    expect(normalizeRemoteURL('C:\\repos\\thing')).toBe('');
    expect(normalizeRemoteURL('not a url')).toBe('');
  });
});

describe('repoKey', () => {
  it('prefers the remote, falls back to the root commit, else nothing', () => {
    expect(repoKey({ remoteURL: 'git@github.com:a/b.git', rootCommit: 'abc' })).toBe('remote:github.com/a/b');
    expect(repoKey({ remoteURL: '/local/remote', rootCommit: 'ABC ' })).toBe('commit:abc');
    expect(repoKey({})).toBe('');
  });
});
