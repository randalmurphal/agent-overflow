import { describe, expect, it } from 'vitest';
import { parsePRReference, prRefFromThread, prRefFromUrl, prScopeLabel } from './prReference';

describe('parsePRReference — GitHub', () => {
  it('parses https://github.com/OWNER/REPO/pull/N', () => {
    const r = parsePRReference('https://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'github', namespace: 'foo', repo: 'bar', number: 42 });
  });

  it('parses without a scheme', () => {
    const r = parsePRReference('github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'github', namespace: 'foo', repo: 'bar', number: 42 });
  });

  it('parses http:// (not just https)', () => {
    const r = parsePRReference('http://github.com/foo/bar/pull/42');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.number).toBe(42);
  });

  it('parses short-form OWNER/REPO#N', () => {
    const r = parsePRReference('foo/bar#321');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'github', namespace: 'foo', repo: 'bar', number: 321 });
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
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group', repo: 'repo', number: 45 });
  });

  it('parses gitlab subgroup paths', () => {
    const r = parsePRReference('https://gitlab.com/group/sub/repo/-/merge_requests/3');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 3 });
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
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group', repo: 'repo', number: 42 });
  });

  it('parses gitlab subgroup short form', () => {
    const r = parsePRReference('group/sub/repo!7');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 7 });
  });

  it('parses gitlab without scheme', () => {
    const r = parsePRReference('gitlab.com/group/repo/-/merge_requests/1');
    if (!r.ok) throw new Error('expected ok');
    expect(r.value.forge).toBe('gitlab');
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
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group', repo: 'repo', number: 9 });
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
    expect(r.value).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 12 });
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
    ['github', 'https://github.com/foo/bar/pull/42', 42, { forge: 'github', namespace: 'foo', repo: 'bar', number: 42 }],
    ['github', 'https://github.com/foo/bar/pull/42/', 42, { forge: 'github', namespace: 'foo', repo: 'bar', number: 42 }],
    ['gitlab', 'https://gitlab.com/group/repo/-/merge_requests/45', 45, { forge: 'gitlab', namespace: 'group', repo: 'repo', number: 45 }],
    ['gitlab', 'https://gitlab.com/group/sub/repo/-/merge_requests/3', 3, { forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 3 }],
    ['gitlab', 'https://git.example.com/group/repo/-/merge_requests/7', 7, { forge: 'gitlab', namespace: 'group', repo: 'repo', number: 7 }],
  ])('prRefFromUrl parses %s %s', (forge, url, number, want) => {
    expect(prRefFromUrl(forge, url, number)).toEqual(want);
  });

  it('prRefFromUrl returns null for garbage and mismatched numbers', () => {
    expect(prRefFromUrl('github', 'not a url', 1)).toBeNull();
    expect(prRefFromUrl('github', 'https://github.com/o/r/issues/1', 1)).toBeNull();
    expect(prRefFromUrl('gitlab', 'https://gitlab.com/g/r/-/merge_requests/2', 1)).toBeNull();
  });

  it('prRefFromThread parses persisted Go JSON and rejects invalid JSON', () => {
    expect(prRefFromThread({
      prRef: JSON.stringify({ Forge: 'gitlab', Namespace: 'group/sub', Repo: 'repo', Number: 12 }),
    })).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 12 });
    expect(prRefFromThread({ prRef: '{nope' })).toBeNull();
  });

  it('prScopeLabel adapts by forge', () => {
    expect(prScopeLabel({ forge: 'github', namespace: 'o', repo: 'r', number: 12 })).toBe('PR #12');
    expect(prScopeLabel({ forge: 'gitlab', namespace: 'o', repo: 'r', number: 12 })).toBe('MR !12');
  });
});
