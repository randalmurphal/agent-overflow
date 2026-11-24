import { describe, expect, it } from 'vitest';
import {
  prKey,
  prRefFromUrl,
  prReferenceWire,
  prScopeLabel,
  prSourceKey,
  prRefFromWire,
  repoKey,
  repoRefFromWire,
  repoReferenceWire,
} from './prReference';

describe('review-pane PRRef helpers', () => {
  it.each([
    ['github', 'https://github.com/foo/bar/pull/42', 42, { forge: 'github', host: 'github.com', namespace: 'foo', repo: 'bar', number: 42 }],
    ['github', 'https://github.com/foo/bar/pull/42/', 42, { forge: 'github', host: 'github.com', namespace: 'foo', repo: 'bar', number: 42 }],
    ['gitlab', 'https://gitlab.com/group/repo/-/merge_requests/45', 45, { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'repo', number: 45 }],
    ['gitlab', 'https://gitlab.com/group/sub/repo/-/merge_requests/3', 3, { forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 3 }],
    ['gitlab', 'https://git.example.com/group/repo/-/merge_requests/7', 7, { forge: 'gitlab', host: 'git.example.com', namespace: 'group', repo: 'repo', number: 7 }],
    // A GitHub Enterprise host is data, not a reason to refuse the PR.
    ['github', 'https://ghe.example.com/foo/bar/pull/42', 42, { forge: 'github', host: 'ghe.example.com', namespace: 'foo', repo: 'bar', number: 42 }],
    // URL.host: lowercase, a non-default port kept. Go's ParsePRURL agrees.
    ['gitlab', 'https://GitLab.Example.com:8443/group/repo/-/merge_requests/7', 7, { forge: 'gitlab', host: 'gitlab.example.com:8443', namespace: 'group', repo: 'repo', number: 7 }],
  ])('prRefFromUrl parses %s %s', (forge, url, number, want) => {
    expect(prRefFromUrl(forge, url, number)).toEqual(want);
  });

  // The same table as Go's TestParsePRURLSpellsTheHostAsURLHost: a default
  // or empty port is dropped, so one PR has one key on both sides.
  it.each([
    ['github', 'https://github.com:443/owner/repo/pull/9', 9, 'github.com', 'github:owner/repo:9'],
    ['github', 'http://github.com:80/owner/repo/pull/9', 9, 'github.com', 'github:owner/repo:9'],
    ['github', 'https://github.com:/owner/repo/pull/9', 9, 'github.com', 'github:owner/repo:9'],
    ['gitlab', 'https://GitLab.com:0443/group/repo/-/merge_requests/3', 3, 'gitlab.com', 'gitlab:group/repo:3'],
    ['github', 'http://ghe.example:443/owner/repo/pull/9', 9, 'ghe.example:443', 'github@ghe.example:443:owner/repo:9'],
    ['github', 'https://ghe.example:80/owner/repo/pull/9', 9, 'ghe.example:80', 'github@ghe.example:80:owner/repo:9'],
    ['github', 'https://ghe.example:08443/owner/repo/pull/9', 9, 'ghe.example:8443', 'github@ghe.example:8443:owner/repo:9'],
    ['github', 'https://[::1]:443/owner/repo/pull/9', 9, '[::1]', 'github@[::1]:owner/repo:9'],
  ] as const)('prRefFromUrl spells %s %s with host %s', (forge, url, number, host, key) => {
    const ref = prRefFromUrl(forge, url, number);
    expect(ref?.host).toBe(host);
    expect(ref && prKey(ref)).toBe(key);
  });

  it('prRefFromUrl returns null for garbage and mismatched numbers', () => {
    expect(prRefFromUrl('github', 'not a url', 1)).toBeNull();
    expect(prRefFromUrl('github', 'https://github.com/o/r/issues/1', 1)).toBeNull();
    expect(prRefFromUrl('gitlab', 'https://gitlab.com/g/r/-/merge_requests/2', 1)).toBeNull();
  });

  it('prScopeLabel adapts by forge', () => {
    expect(prScopeLabel({ forge: 'github', host: 'github.com', namespace: 'o', repo: 'r', number: 12 })).toBe('PR #12');
    expect(prScopeLabel({ forge: 'gitlab', host: 'gitlab.com', namespace: 'o', repo: 'r', number: 12 })).toBe('MR !12');
  });
});

// prKey is a WIRE address, not a local convenience: `PRReference.Key` in
// internal/git/forge.go builds the identical string and the `pr:updated`
// event is addressed with it, so a change on either side silently stops
// routing. These cases are the same table as Go's
// TestPRUpdateKeyMatchesTheFrontendSourceKey; if one moves, both fail.
describe('prKey — the shared PR wire address', () => {
  it.each([
    [{ forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 } as const, 'github:owner/repo:5'],
    [
      { forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 12 } as const,
      'gitlab:group/sub/repo:12',
    ],
    [
      { forge: 'github', host: 'ghe.example.com', namespace: 'owner', repo: 'repo', number: 5 } as const,
      'github@ghe.example.com:owner/repo:5',
    ],
    [
      { forge: 'gitlab', host: 'gitlab.example.com:8443', namespace: 'group/sub', repo: 'repo', number: 12 } as const,
      'gitlab@gitlab.example.com:8443:group/sub/repo:12',
    ],
  ])('prKey(%o) === %s', (ref, want) => {
    expect(prKey(ref)).toBe(want);
  });

  // Drafts persist under this sourceKey; a public-host PR must keep the
  // spelling it had before references carried a host.
  it('keeps the persisted public-host sourceKey spelling', () => {
    const ref = prRefFromUrl('github', 'https://github.com/owner/repo/pull/5', 5);
    if (ref === null) throw new Error('expected a ref');
    expect(prSourceKey(ref)).toBe('pr:github:owner/repo:5');
    const mr = prRefFromUrl('gitlab', 'https://gitlab.com/group/sub/repo/-/merge_requests/12', 12);
    if (mr === null) throw new Error('expected a ref');
    expect(prSourceKey(mr)).toBe('pr:gitlab:group/sub/repo:12');
  });

  // RepoReference.Key in internal/git/repo_reference.go: the PR key
  // without its number. A job log and a pipeline pump are addressed by it.
  it.each([
    [{ forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo' } as const, 'github:owner/repo'],
    [{ forge: 'gitlab', host: 'gitlab.example.com:8443', namespace: 'group/sub', repo: 'repo' } as const, 'gitlab@gitlab.example.com:8443:group/sub/repo'],
  ])('repoKey(%o) === %s', (ref, want) => {
    expect(repoKey(ref)).toBe(want);
  });

  it('converts wire repositories and PRs, refusing an unknown forge', () => {
    const wire = { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'repo', Number: 7 };
    expect(prRefFromWire(wire)).toEqual({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'repo', number: 7 });
    expect(repoReferenceWire(repoRefFromWire(wire)!)).toEqual({ Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'repo' });
    expect(repoRefFromWire({ ...wire, Forge: 'bitbucket' })).toBeNull();
  });

  it('prSourceKey is prKey behind the pr: scope prefix', () => {
    expect(prSourceKey({ forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 })).toBe(
      'pr:github:owner/repo:5',
    );
    expect(prSourceKey({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 12 })).toBe(
      'pr:gitlab:group/sub/repo:12',
    );
  });

});

describe('prReferenceWire', () => {
  it('sends the host the backend requires', () => {
    expect(
      prReferenceWire({ forge: 'gitlab', host: 'gitlab.example.com', namespace: 'group/sub', repo: 'repo', number: 3 }),
    ).toEqual({ Forge: 'gitlab', Host: 'gitlab.example.com', Namespace: 'group/sub', Repo: 'repo', Number: 3 });
  });
});
