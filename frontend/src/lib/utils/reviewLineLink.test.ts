import { afterEach, describe, expect, it, vi } from 'vitest';
import { hexDigest, pureHexDigest } from './hexDigest';
import { lineLink, linePositionAt } from './reviewLineLink';
import { parseReviewFiles } from './patchStore';

// Digests from `printf '%s' <path> | sha256sum` and `sha1sum`.
const DIGESTS = [
  { path: 'src/app.ts', sha256: '841254fe75488c1bd4cd7f68f00b4be0e48dcfbc4a16b45847b68295e0e3b27b', sha1: '216381173f187cf4c2baf119193855699f4bc616' },
  { path: 'img/moved.png', sha256: '9ed4f9955b1fa30cfe05274fc0f8cf75ca695b030c1ac5f2fb4e0874b20caf99', sha1: '88e00ccd5bbe5a1989523647ff40a81d2f8239ec' },
  { path: 'a b/ünï.txt', sha256: 'f1db545a5d36e6ba1355f94da081f0ef6b1b04621d85e00c54f2e2819f7dbc17', sha1: '7ca6a06256cb97f9d2b07b69c4c505514a1a1518' },
  // Past one 64-byte block.
  { path: 'deeply/nested/directory/structure/with/a/rather/long/file/name/component.tsx', sha256: 'ce37954892268c81cbd91e2a9735e8a683e0170896ce0188e1a81ec4199995d9', sha1: '2947a0767aa62c488b58672fe6194606205992b2' },
];

// Lines 1-3 unchanged, 4 removed, 5-6 (new 4-5) added, then unchanged.
const PATCH = [
  'diff --git a/src/app.ts b/src/app.ts',
  'index 1111111..2222222 100644',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -10,5 +10,6 @@',
  ' a',
  ' b',
  '-c',
  '+C',
  '+D',
  ' e',
  ' f',
  '',
].join('\n');

const RENAME = [
  'diff --git a/moved.png b/img/moved.png',
  'similarity index 90%',
  'rename from old/name.ts',
  'rename to img/moved.png',
  'index 1111111..2222222 100644',
  '--- a/old/name.ts',
  '+++ b/img/moved.png',
  '@@ -3,2 +3,2 @@',
  ' keep',
  '-gone',
  '+here',
  '',
].join('\n');

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('hexDigest', () => {
  it.each(DIGESTS)('the pure digests of $path match sha256sum and sha1sum', ({ path, sha256, sha1 }) => {
    const bytes = new TextEncoder().encode(path);
    expect(pureHexDigest('SHA-256', bytes)).toBe(sha256);
    expect(pureHexDigest('SHA-1', bytes)).toBe(sha1);
  });

  it('answers without Web Crypto, as a plain-HTTP page has none', async () => {
    vi.stubGlobal('crypto', {});
    await expect(hexDigest('SHA-256', DIGESTS[0].path)).resolves.toBe(DIGESTS[0].sha256);
    await expect(hexDigest('SHA-1', DIGESTS[0].path)).resolves.toBe(DIGESTS[0].sha1);
  });
});

describe('line links', () => {
  const file = parseReviewFiles(PATCH)[0];
  // Body lines: 0-3 meta, 4 hunk, 5 ' a', 6 ' b', 7 '-c', 8 '+C', 9 '+D', 10 ' e'.
  it('names an unchanged line by both numbers, an added one with the old counter, a removed one with the new counter', () => {
    expect(linePositionAt(file, 5)).toEqual({ kind: 'context', oldLine: 10, newLine: 10 });
    expect(linePositionAt(file, 7)).toEqual({ kind: 'del', oldLine: 12, newLine: 12 });
    expect(linePositionAt(file, 8)).toEqual({ kind: 'add', oldLine: 13, newLine: 12 });
    expect(linePositionAt(file, 9)).toEqual({ kind: 'add', oldLine: 13, newLine: 13 });
    expect(linePositionAt(file, 10)).toEqual({ kind: 'context', oldLine: 13, newLine: 14 });
    expect(linePositionAt(file, 4)).toBeNull();
  });

  it('builds GitHub anchors: L with a removed line\'s old number, R with any other line\'s new number', async () => {
    const pr = 'https://github.com/o/r/pull/7';
    const hash = DIGESTS[0].sha256;
    await expect(lineLink('github', pr, file.path, linePositionAt(file, 7)!)).resolves.toBe(`${pr}/files#diff-${hash}L12`);
    await expect(lineLink('github', pr, file.path, linePositionAt(file, 8)!)).resolves.toBe(`${pr}/files#diff-${hash}R12`);
    await expect(lineLink('github', `${pr}/`, file.path, linePositionAt(file, 10)!)).resolves.toBe(`${pr}/files#diff-${hash}R14`);
  });

  it('builds GitLab line codes: sha1 of the path, old and new positions', async () => {
    const mr = 'https://gitlab.com/o/r/-/merge_requests/7';
    const hash = DIGESTS[0].sha1;
    await expect(lineLink('gitlab', mr, file.path, linePositionAt(file, 5)!)).resolves.toBe(`${mr}/diffs#${hash}_10_10`);
    await expect(lineLink('gitlab', mr, file.path, linePositionAt(file, 7)!)).resolves.toBe(`${mr}/diffs#${hash}_12_12`);
    await expect(lineLink('gitlab', mr, file.path, linePositionAt(file, 9)!)).resolves.toBe(`${mr}/diffs#${hash}_13_13`);
    await expect(lineLink('gitlab', mr, file.path, linePositionAt(file, 10)!)).resolves.toBe(`${mr}/diffs#${hash}_13_14`);
  });

  it('hashes a renamed file\'s new path', async () => {
    const renamed = parseReviewFiles(RENAME)[0];
    expect(renamed.path).toBe('img/moved.png');
    // ' keep' 3/3, '-gone' 4, '+here' 4.
    const added = linePositionAt(renamed, 10)!;
    expect(added).toEqual({ kind: 'add', oldLine: 5, newLine: 4 });
    await expect(lineLink('github', 'https://github.com/o/r/pull/1', renamed.path, added)).resolves.toBe(`https://github.com/o/r/pull/1/files#diff-${DIGESTS[1].sha256}R4`);
    await expect(lineLink('gitlab', 'https://gitlab.com/o/r/-/merge_requests/1', renamed.path, added)).resolves.toBe(`https://gitlab.com/o/r/-/merge_requests/1/diffs#${DIGESTS[1].sha1}_5_4`);
  });
});
