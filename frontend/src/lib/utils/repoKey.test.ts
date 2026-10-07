import { describe, expect, it } from 'vitest';
import { groupRepositories, hasRepoIdentity } from './repoKey';

const row = (computer: string, id: string, repositoryID?: string, createdAt = 1, archived = false) => ({
  computer, project: { id, repositoryID, createdAt, archived },
});

describe('verified repository grouping', () => {
  it('requires a verified ID even when legacy URLs and roots agree', () => {
    const legacy = { remoteURL: 'https://github.com/me/app', rootCommit: 'same' };
    const a = { ...row('home', 'a'), project: { ...row('home', 'a').project, ...legacy } };
    const b = { ...row('other', 'b'), project: { ...row('other', 'b').project, ...legacy } };
    expect(hasRepoIdentity(a.project)).toBe(false);
    expect(groupRepositories([a, b]).members.size).toBe(0);
    const verified = row('third', 'c', 'github:github.com:1');
    expect(groupRepositories([a, b, verified]).members.get('forge:github:github.com:1')).toEqual([verified]);
  });

  it('merges only equal forge IDs across computers', () => {
    const a = row('home', 'a', 'github:github.com:1');
    const b = row('other', 'b', 'github:github.com:1');
    const fork = row('third', 'fork', 'github:github.com:2');
    const groups = groupRepositories([a, b, fork]);
    expect(groups.members.get('forge:github:github.com:1')).toEqual([a, b]);
    expect(groups.members.get('forge:github:github.com:2')).toEqual([fork]);
    expect(hasRepoIdentity(a.project)).toBe(true);
    expect(hasRepoIdentity({})).toBe(false);
  });

  it('takes the oldest live clone per computer and leaves extra clones separate', () => {
    const older = row('home', 'old', 'github:github.com:1', 1);
    const newer = row('home', 'new', 'github:github.com:1', 2);
    const archived = row('other', 'archived', 'github:github.com:1', 0, true);
    const live = row('other', 'live', 'github:github.com:1', 3);
    expect(groupRepositories([newer, older, archived, live]).members.get('forge:github:github.com:1')).toEqual([older, live]);
  });
});
