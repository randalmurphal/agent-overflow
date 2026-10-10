import { describe, expect, it } from 'vitest';
import {
  parsePRReference,
  prKey,
  prRefFromUrl,
  prReferenceWire,
  prScopeLabel,
  prSourceKey,
} from './prReference';

describe('parsePRReference — GitHub', () => {
  it('parses https://github.com/OWNER/REPO/pull/N', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'github', host: 'github.com', namespace: 'foo', repo: 'bar', number: 42 });
  });

  it('parses without a scheme', () => {
    const r = parsePRReference('github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'github', host: 'github.com', namespace: 'foo', repo: 'bar', number: 42 });
  });

  it('parses http:// (not just https)', () => {
    const r = parsePRReference('http://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(42);
  });

  it('parses short-form OWNER/REPO#N', () => {
    const r = parsePRReference('foo/bar#321');
    if (!r.ok) throw new Error('expected ok');
    // A short form names no host: the caller supplies the repository's.
    expect(r.value).toEqual({ forge: 'github', host: '', namespace: 'foo', repo: 'bar', number: 321 });
  });

  it('tolerates trailing path segments after the number', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15/files');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('tolerates trailing query strings', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15?diff=split');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('tolerates trailing anchors', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/15#issuecomment-9');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(15);
  });

  it('trims surrounding whitespace', () => {
    const r = parsePRReference('   https://github.com/foo/bar/pull/7   ');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(7);
  });
});

describe('parsePRReference — GitLab', () => {
  it('parses https://gitlab.com/NAMESPACE/REPO/-/merge_requests/N', () => {
    const r = parsePRReference('https://gitlab.com/group/repo/-/merge_requests/45');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'repo', number: 45 });
  });

  it('parses gitlab subgroup paths', () => {
    const r = parsePRReference('https://gitlab.com/group/sub/repo/-/merge_requests/3');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 3 });
  });

  it('parses deeply-nested gitlab subgroups', () => {
    const r = parsePRReference('https://gitlab.com/group/sub1/sub2/repo/-/merge_requests/9');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.namespace).toBe('group/sub1/sub2');
    expect(r.value.repo).toBe('repo');
  });

  it('parses gitlab short form NAMESPACE/REPO!N', () => {
    const r = parsePRReference('group/repo!42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', host: '', namespace: 'group', repo: 'repo', number: 42 });
  });

  it('parses gitlab subgroup short form', () => {
    const r = parsePRReference('group/sub/repo!7');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', host: '', namespace: 'group/sub', repo: 'repo', number: 7 });
  });

  it('parses gitlab without scheme', () => {
    const r = parsePRReference('gitlab.com/group/repo/-/merge_requests/1');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.forge).toBe('gitlab');
    expect(r.value.host).toBe('gitlab.com');
    expect(r.value.number).toBe(1);
  });
});

describe('parsePRReference — rejection', () => {
  it('rejects empty input', () => {
    const r = parsePRReference('');
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toMatch(/empty/i);
  });

  it('rejects whitespace-only input', () => {
    const r = parsePRReference('   ');
    expect(r.ok).toBe(false);
  });

  it('rejects unsupported hosts (bitbucket, self-hosted)', () => {
    expect(parsePRReference('https://bitbucket.org/foo/bar/pull-requests/1').ok).toBe(false);
    expect(parsePRReference('https://git.example.com/foo/bar/pull/1').ok).toBe(false);
  });

  it('rejects lookalike github / gitlab hosts (regex anchor regression guards)', () => {
    expect(parsePRReference('https://evilgithub.com/owner/repo/pull/1').ok).toBe(false);
    expect(parsePRReference('http://github.com.attacker.com/owner/repo/pull/1').ok).toBe(false);
    expect(parsePRReference('https://evilgitlab.com/group/repo/-/merge_requests/1').ok).toBe(false);
    expect(parsePRReference('https://gitlab.com.attacker.com/group/repo/-/merge_requests/1').ok).toBe(false);
  });

  it('rejects malformed gitlab URL with missing repo segment', () => {
    const r = parsePRReference('https://gitlab.com/foo/-/merge_requests/1');
    expect(r.ok).toBe(false);
  });

  it('rejects gitlab path that uses pull instead of merge_requests', () => {
    const r = parsePRReference('https://gitlab.com/foo/bar/pull/1');
    expect(r.ok).toBe(false);
  });

  it('rejects issues URLs', () => {
    const r = parsePRReference('https://github.com/foo/bar/issues/1');
    expect(r.ok).toBe(false);
  });

  it('rejects references missing a number', () => {
    expect(parsePRReference('foo/bar#').ok).toBe(false);
    expect(parsePRReference('foo/bar!').ok).toBe(false);
  });

  it('rejects non-integer PR/MR numbers', () => {
    expect(parsePRReference('foo/bar#abc').ok).toBe(false);
    expect(parsePRReference('group/repo!abc').ok).toBe(false);
  });

  it('rejects zero and negative PR/MR numbers', () => {
    expect(parsePRReference('foo/bar#0').ok).toBe(false);
    expect(parsePRReference('foo/bar#-3').ok).toBe(false);
    expect(parsePRReference('https://github.com/foo/bar/pull/0').ok).toBe(false);
    expect(parsePRReference('group/repo!0').ok).toBe(false);
  });

  it('rejects single-segment gitlab short form', () => {
    expect(parsePRReference('single!1').ok).toBe(false);
  });

  it('rejects plain text', () => {
    const r = parsePRReference('not a url');
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toMatch(/Unrecognised/);
  });
});

describe('parsePRReference — self-hosted GitLab', () => {
  it('rejects self-hosted host when not in allowlist', () => {
    const r = parsePRReference('https://gitlab.mycompany.com/group/repo/-/merge_requests/9');
    expect(r.ok).toBe(false);
  });

  it('parses self-hosted host when in allowlist', () => {
    const r = parsePRReference(
      'https://gitlab.mycompany.com/group/repo/-/merge_requests/9',
      { gitlabHosts: ['gitlab.mycompany.com'] },
    );
    if (!r.ok) throw new Error('expected ok, got: ' + r.error);
    expect(r.value).toEqual({ forge: 'gitlab', host: 'gitlab.mycompany.com', namespace: 'group', repo: 'repo', number: 9 });
  });

  it('still accepts gitlab.com when an allowlist is set', () => {
    const r = parsePRReference(
      'https://gitlab.com/group/repo/-/merge_requests/1',
      { gitlabHosts: ['gitlab.mycompany.com'] },
    );
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.forge).toBe('gitlab');
  });

  it('parses self-hosted subgroup paths', () => {
    const r = parsePRReference(
      'https://gl.example.test/group/sub/repo/-/merge_requests/12',
      { gitlabHosts: ['gl.example.test'] },
    );
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', host: 'gl.example.test', namespace: 'group/sub', repo: 'repo', number: 12 });
  });

  it('rejects lookalike host even when a partial-match suffix is allowlisted', () => {
    const r = parsePRReference(
      'https://evil.gitlab.mycompany.com.attacker.com/g/r/-/merge_requests/1',
      { gitlabHosts: ['gitlab.mycompany.com'] },
    );
    expect(r.ok).toBe(false);
  });
});

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

  it('prSourceKey is prKey behind the pr: scope prefix', () => {
    expect(prSourceKey({ forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 })).toBe(
      'pr:github:owner/repo:5',
    );
    expect(prSourceKey({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 12 })).toBe(
      'pr:gitlab:group/sub/repo:12',
    );
  });

  // A namespace containing ':' would make two different PRs share a key, so
  // the backend refuses one (internal/git/forge.go ValidateProjectSegment).
  // Nothing here can produce one: every parse path splits on '/' and the
  // short forms reject ':' in neither — this asserts the shape the parser
  // hands prKey, so a future loosening of the parser trips a test.
  it('never builds a key from a segment carrying the delimiter', () => {
    const parsed = parsePRReference('https://gitlab.com/group/sub/repo/-/merge_requests/12');
    if (!parsed.ok) throw new Error('expected ok');
    const key = prKey({ ...parsed.value });
    expect(key.split(':')).toHaveLength(3);
  });
});

describe('prReferenceWire', () => {
  it('sends the host the backend requires', () => {
    expect(
      prReferenceWire({ forge: 'gitlab', host: 'gitlab.example.com', namespace: 'group/sub', repo: 'repo', number: 3 }),
    ).toEqual({ Forge: 'gitlab', Host: 'gitlab.example.com', Namespace: 'group/sub', Repo: 'repo', Number: 3 });
  });
});
