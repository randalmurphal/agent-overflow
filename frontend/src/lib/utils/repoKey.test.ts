import { describe, expect, it } from 'vitest';
import { groupRepositories, hasRepoIdentity, normalizeRemoteURL } from './repoKey';

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

describe('hasRepoIdentity', () => {
  it('is a usable remote or a root commit', () => {
    expect(hasRepoIdentity({ remoteURL: 'git@github.com:a/b.git' })).toBe(true);
    expect(hasRepoIdentity({ remoteURL: '/local/remote', rootCommit: 'ABC ' })).toBe(true);
    expect(hasRepoIdentity({ remoteURL: '/local/remote', rootCommit: ' ' })).toBe(false);
    expect(hasRepoIdentity({})).toBe(false);
  });
});

describe('groupRepositories', () => {
  let nextId = 0;
  const row = (computer: string, project: { remoteURL?: string; rootCommit?: string; createdAt?: number; archived?: boolean }) => ({
    computer,
    project: { id: `p${nextId++}`, createdAt: project.createdAt ?? 1, archived: project.archived ?? false, ...project },
  });
  const ids = (rows: { project: { id: string } }[] | undefined) => (rows ?? []).map((r) => r.project.id);

  it('merges a remoteless checkout into the one remote group holding its root', () => {
    const mac = row('home', { remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' });
    const desktop = row('desktop', { rootCommit: 'ABC' });
    const { keyOf, members } = groupRepositories([mac, desktop]);
    expect(keyOf.get(desktop)).toBe('remote:github.com/me/app');
    expect(ids(members.get('remote:github.com/me/app'))).toEqual([mac.project.id, desktop.project.id]);
  });

  it('keeps a remoteless checkout apart when its root is shared by several remotes', () => {
    const upstream = row('home', { remoteURL: 'git@github.com:org/app.git', rootCommit: 'abc' });
    const fork = row('laptop', { remoteURL: 'git@github.com:me/app.git', rootCommit: 'abc' });
    const local = row('desktop', { rootCommit: 'abc' });
    const { keyOf } = groupRepositories([upstream, fork, local]);
    expect(keyOf.get(upstream)).not.toBe(keyOf.get(fork));
    expect(keyOf.get(local)).toBe('commit:abc');
  });

  it('takes one member per computer: the oldest live clone', () => {
    const remote = 'https://github.com/me/app';
    const older = row('home', { remoteURL: remote, createdAt: 1 });
    const newer = row('home', { remoteURL: remote, createdAt: 2 });
    const archivedOldest = row('laptop', { remoteURL: remote, createdAt: 0, archived: true });
    const laptop = row('laptop', { remoteURL: remote, createdAt: 5 });
    const { keyOf, members } = groupRepositories([newer, older, laptop, archivedOldest]);
    expect(ids(members.get('remote:github.com/me/app'))).toEqual([older.project.id, laptop.project.id]);
    expect(keyOf.get(newer)).toBe('remote:github.com/me/app');
  });

  it('never groups a row with no identity', () => {
    const a = row('home', {});
    const b = row('laptop', { remoteURL: '/srv/git/local.git' });
    const { keyOf, members } = groupRepositories([a, b]);
    expect(keyOf.get(a)).toBe('');
    expect(keyOf.get(b)).toBe('');
    expect(members.size).toBe(0);
  });
});
