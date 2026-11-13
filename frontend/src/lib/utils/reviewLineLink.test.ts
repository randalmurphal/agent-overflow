import { describe, expect, it } from 'vitest';
import { lineLink, lineLinkLabel, linePositionAt } from './reviewLineLink';
import { parseReviewFiles } from './patchStore';

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

const HEAD = 'a'.repeat(40);

describe('line links', () => {
  const file = parseReviewFiles(PATCH)[0];
  // Body lines: 0-3 meta, 4 hunk, 5 ' a', 6 ' b', 7 '-c', 8 '+C', 9 '+D', 10 ' e'.
  it('reads a row\'s numbers, with the other side\'s counter for an added or removed line', () => {
    expect(linePositionAt(file, 5)).toEqual({ kind: 'context', oldLine: 10, newLine: 10 });
    expect(linePositionAt(file, 7)).toEqual({ kind: 'del', oldLine: 12, newLine: 12 });
    expect(linePositionAt(file, 8)).toEqual({ kind: 'add', oldLine: 13, newLine: 12 });
    expect(linePositionAt(file, 9)).toEqual({ kind: 'add', oldLine: 13, newLine: 13 });
    expect(linePositionAt(file, 10)).toEqual({ kind: 'context', oldLine: 13, newLine: 14 });
    expect(linePositionAt(file, 4)).toBeNull();
  });

  it('links a GitHub line to its file at the new side\'s commit, with the documented #L anchor', () => {
    const pr = 'https://github.com/o/r/pull/7';
    expect(lineLink('github', pr, file.path, linePositionAt(file, 8)!, HEAD)).toBe(`https://github.com/o/r/blob/${HEAD}/src/app.ts#L12`);
    expect(lineLink('github', `${pr}/`, file.path, linePositionAt(file, 10)!, HEAD)).toBe(`https://github.com/o/r/blob/${HEAD}/src/app.ts#L14`);
    expect(lineLinkLabel('github')).toBe('Copy link to line');
  });

  it('links a GitLab line to its file at the new side\'s commit, with no undocumented anchor', () => {
    const mr = 'https://gitlab.example.com/group/sub/r/-/merge_requests/7';
    expect(lineLink('gitlab', mr, file.path, linePositionAt(file, 5)!, HEAD)).toBe(`https://gitlab.example.com/group/sub/r/-/blob/${HEAD}/src/app.ts`);
    expect(lineLinkLabel('gitlab')).toBe('Copy link to file');
  });

  it('gives a removed line no link: it has no place at the new side\'s commit', () => {
    expect(lineLink('github', 'https://github.com/o/r/pull/7', file.path, linePositionAt(file, 7)!, HEAD)).toBeNull();
    expect(lineLink('gitlab', 'https://gitlab.com/o/r/-/merge_requests/7', file.path, linePositionAt(file, 7)!, HEAD)).toBeNull();
  });

  it('links a renamed file by its new path, each segment escaped', () => {
    const renamed = parseReviewFiles(RENAME)[0];
    expect(renamed.path).toBe('img/a b/ünï.ts');
    const added = linePositionAt(renamed, 10)!;
    expect(added).toEqual({ kind: 'add', oldLine: 5, newLine: 4 });
    expect(lineLink('github', 'https://github.com/o/r/pull/1', renamed.path, added, HEAD)).toBe(`https://github.com/o/r/blob/${HEAD}/img/a%20b/%C3%BCn%C3%AF.ts#L4`);
    expect(lineLink('gitlab', 'https://gitlab.com/o/r/-/merge_requests/1', renamed.path, added, HEAD)).toBe(`https://gitlab.com/o/r/-/blob/${HEAD}/img/a%20b/%C3%BCn%C3%AF.ts`);
  });

  it('refuses an address that is not the forge\'s PR, and a commit that is not one', () => {
    const at = linePositionAt(file, 8)!;
    expect(() => lineLink('github', 'https://github.com/o/r/issues/7', file.path, at, HEAD)).toThrow('Not a pull request address');
    expect(() => lineLink('gitlab', 'https://github.com/o/r/pull/7', file.path, at, HEAD)).toThrow('Not a merge request address');
    expect(() => lineLink('github', 'https://github.com/o/r/pull/7', file.path, at, '')).toThrow('Not a commit');
  });
});
