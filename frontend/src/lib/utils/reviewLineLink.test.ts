import { describe, expect, it } from 'vitest';
import { lineLink, linePositionAt } from './reviewLineLink';
import { parseReviewFiles } from './patchStore';

// Lines 1-3 unchanged, 4 removed, 5-6 (new 4-5) added, then unchanged;
// a second hunk, a line ahead on the new side, removes one.
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
  '@@ -20,3 +21,2 @@',
  ' x',
  '-y',
  ' z',
  '',
].join('\n');

const RENAME = [
  'diff --git a/old/name.ts b/img/a b/ünï.ts',
  'similarity index 90%',
  'rename from old/name.ts',
  'rename to img/a b/ünï.ts',
  'index 1111111..2222222 100644',
  '--- a/old/name.ts',
  '+++ b/img/a b/ünï.ts',
  '@@ -3,2 +3,2 @@',
  ' keep',
  '-gone',
  '+here',
  '',
].join('\n');

// Path digests checked with `printf '%s' <path> | sha256sum` and `sha1sum`.
const APP_SHA256 = '841254fe75488c1bd4cd7f68f00b4be0e48dcfbc4a16b45847b68295e0e3b27b';
const APP_SHA1 = '216381173f187cf4c2baf119193855699f4bc616';
const PR = 'https://github.com/o/r/pull/7';
const MR = 'https://gitlab.example.com/group/sub/r/-/merge_requests/7';

describe('line links', () => {
  const file = parseReviewFiles(PATCH)[0];
  // Body lines: 0-3 meta, 4 hunk, 5 ' a', 6 ' b', 7 '-c', 8 '+C', 9 '+D', 10 ' e',
  // 11 ' f', 12 hunk, 13 ' x', 14 '-y'.
  it('reads a row\'s numbers, with the other side\'s counter for an added or removed line', () => {
    expect(linePositionAt(file, 5)).toEqual({ kind: 'context', oldLine: 10, newLine: 10 });
    expect(linePositionAt(file, 7)).toEqual({ kind: 'del', oldLine: 12, newLine: 12 });
    expect(linePositionAt(file, 8)).toEqual({ kind: 'add', oldLine: 13, newLine: 12 });
    expect(linePositionAt(file, 9)).toEqual({ kind: 'add', oldLine: 13, newLine: 13 });
    expect(linePositionAt(file, 10)).toEqual({ kind: 'context', oldLine: 13, newLine: 14 });
    expect(linePositionAt(file, 14)).toEqual({ kind: 'del', oldLine: 21, newLine: 22 });
    expect(linePositionAt(file, 4)).toBeNull();
  });

  it.each([
    { name: 'the first line of a hunk', line: 5, github: `R10`, gitlab: `_10_10` },
    { name: 'a context line', line: 10, github: `R14`, gitlab: `_13_14` },
    { name: 'a removed line', line: 7, github: `L12`, gitlab: `_12_12` },
    { name: 'an added line', line: 8, github: `R12`, gitlab: `_13_12` },
    { name: 'a second added line', line: 9, github: `R13`, gitlab: `_13_13` },
    { name: 'a removed line a line behind the new side', line: 14, github: `L21`, gitlab: `_21_22` },
  ])('links $name to its place on each forge\'s diff page', ({ line, github, gitlab }) => {
    const at = linePositionAt(file, line)!;
    expect(lineLink('github', PR, file.path, at)).toBe(`${PR}/files#diff-${APP_SHA256}${github}`);
    expect(lineLink('gitlab', MR, file.path, at)).toBe(`${MR}/diffs#${APP_SHA1}${gitlab}`);
  });

  it('hashes the GitHub path with SHA-256 as the forge does', () => {
    const at = { kind: 'context', oldLine: 1, newLine: 1 } as const;
    expect(lineLink('github', PR, 'README.md', at)).toBe(`${PR}/files#diff-b335630551682c19a781afebcf4d07bf978fb1f8ac04c6bf87428ed5106870f5R1`);
  });

  it('drops a trailing slash on the PR address', () => {
    const at = linePositionAt(file, 5)!;
    expect(lineLink('github', `${PR}/`, file.path, at)).toBe(`${PR}/files#diff-${APP_SHA256}R10`);
    expect(lineLink('gitlab', `${MR}/`, file.path, at)).toBe(`${MR}/diffs#${APP_SHA1}_10_10`);
  });

  it('hashes a renamed file\'s new path, with spaces and non-ASCII as UTF-8', () => {
    const renamed = parseReviewFiles(RENAME)[0];
    expect(renamed.path).toBe('img/a b/ünï.ts');
    // Body lines: 0-6 meta, 7 hunk, 8 ' keep', 9 '-gone', 10 '+here'.
    const removed = linePositionAt(renamed, 9)!;
    const added = linePositionAt(renamed, 10)!;
    expect(added).toEqual({ kind: 'add', oldLine: 5, newLine: 4 });
    const sha256 = 'f837d4c8cb236c88a1de3d10e7981f440f3a9a7cc4701d78d9763f08e6d0a339';
    const sha1 = 'cf7e08be98357d24cef5a67c258ec0cf11691041';
    expect(lineLink('github', PR, renamed.path, added)).toBe(`${PR}/files#diff-${sha256}R4`);
    expect(lineLink('github', PR, renamed.path, removed)).toBe(`${PR}/files#diff-${sha256}L4`);
    expect(lineLink('gitlab', MR, renamed.path, added)).toBe(`${MR}/diffs#${sha1}_5_4`);
    expect(lineLink('gitlab', MR, renamed.path, removed)).toBe(`${MR}/diffs#${sha1}_4_4`);
  });

  it('refuses an address that is not the forge\'s PR', () => {
    const at = linePositionAt(file, 8)!;
    expect(() => lineLink('github', 'https://github.com/o/r/issues/7', file.path, at)).toThrow('Not a pull request address');
    expect(() => lineLink('gitlab', PR, file.path, at)).toThrow('Not a merge request address');
    expect(() => lineLink('github', `${PR}/files`, file.path, at)).toThrow('Not a pull request address');
  });
});
