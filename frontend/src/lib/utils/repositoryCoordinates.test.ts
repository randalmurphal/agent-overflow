import { describe, expect, it } from 'vitest';
import { safeRepositoryMetadata, safeIdentityError } from './repositoryCoordinates';

describe('repository coordinate boundary', () => {
  it('marks an unverified legacy repository without turning it into a plain folder', () => {
    const legacy = { remoteURL: 'https://github.com/a/b', rootCommit: 'old' };
    expect(safeRepositoryMetadata(legacy)).toEqual({ identityError: 'Repository identity has not been verified yet.' });
    expect(safeRepositoryMetadata({ ...legacy, repositoryID: 'github:github.com:1' })).toEqual({ repositoryID: 'github:github.com:1' });
    expect(safeRepositoryMetadata({ remoteURL: '', rootCommit: '' })).toEqual({});
  });
  it('removes retired identity fields without touching conversation content', () => {
    const raw = 'https://user:SECRET@github.com/a/b.git?token=QUERY#fragment';
    const payload = { project: { remoteURL: raw, rootCommit: 'old', identitySource: 'private', identityError: `failed ${raw}` }, thread: { origin: { remoteUrl: raw } }, items: [{ text: raw }] };
    const safe = safeRepositoryMetadata(payload);
    expect(safe.project).toEqual({ identityError: 'failed [remote]' });
    expect(safe.thread.origin).not.toHaveProperty('remoteUrl');
    expect(safe.items).toBe(payload.items);
    expect(payload.project.remoteURL).toBe(raw);
    expect(safeRepositoryMetadata(safe)).toBe(safe);
  });
});

it('keeps quoted passwords private in Git errors', () => {
  for (const quote of ["'", '"']) {
    const error = `failed: ${quote}https://user:sec'ret@github.com/a/b${quote}`;
    expect(safeIdentityError(error)).not.toMatch(/sec|ret@/);
  }
});

it.each(['https://github.com/a/b', 'git@github.com:a/b.git', 'work-host:repo.git', 'ssh://git@work-host/a/b'])('removes the entire remote from diagnostics: %s', remote => {
  const safe = safeIdentityError(`failed '${remote}'`);
  expect(safe).not.toContain(remote);
  expect(safe).toContain('[remote]');
});
