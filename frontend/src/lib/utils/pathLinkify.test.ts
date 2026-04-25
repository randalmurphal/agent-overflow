import { describe, expect, it } from 'vitest';
import { findPathRanges } from './pathLinkify';

describe('findPathRanges', () => {
  it('returns an empty list for an empty string', () => {
    expect(findPathRanges('')).toEqual([]);
  });

  it('matches a simple relative path with extension', () => {
    const ranges = findPathRanges('see src/lib/foo.ts for context');
    // "src/lib/foo.ts" — 14 chars starting at offset 4.
    expect(ranges).toEqual([
      { start: 4, end: 18, path: 'src/lib/foo.ts', line: undefined, col: undefined },
    ]);
  });

  it('matches a path with line', () => {
    const ranges = findPathRanges('check src/lib/foo.ts:42 please');
    expect(ranges).toEqual([
      { start: 6, end: 23, path: 'src/lib/foo.ts', line: 42, col: undefined },
    ]);
  });

  it('matches a path with line and column', () => {
    const ranges = findPathRanges('error in src/lib/foo.ts:42:7 oops');
    expect(ranges).toEqual([
      { start: 9, end: 28, path: 'src/lib/foo.ts', line: 42, col: 7 },
    ]);
  });

  it('matches a leading-slash absolute path', () => {
    const ranges = findPathRanges('crashed at /Users/me/code/foo.ts:10');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('/Users/me/code/foo.ts');
    expect(ranges[0].line).toBe(10);
  });

  it('does not match URLs', () => {
    expect(findPathRanges('see https://example.com/foo for docs')).toEqual([]);
    expect(findPathRanges('also http://x.org/path/to/page')).toEqual([]);
  });

  it('does not match scoped npm packages', () => {
    expect(findPathRanges('install @sveltejs/kit and use it')).toEqual([]);
    expect(findPathRanges('depends on @scope/pkg.foo')).toEqual([]);
  });

  it('does not match emails', () => {
    expect(findPathRanges('contact user@example.com please')).toEqual([]);
  });

  it('does not match bare module names without slashes', () => {
    // `marked` looks like a single token; with no slash it never
    // looks like a path.
    expect(findPathRanges('we use the marked package')).toEqual([]);
  });

  it('does not match plain directory paths without a filename', () => {
    // `path/to/dir` without an extension on the final segment is
    // ambiguous; we err on the side of NOT linkifying. Directory
    // paths carrying an extension (e.g. `assets/foo.bar/`) are
    // intentionally rejected too because `looksLikeFilePath` runs
    // on the captured token, which won't include the trailing slash.
    expect(findPathRanges('look in path/to/dir')).toEqual([]);
  });

  it('does not match version strings', () => {
    expect(findPathRanges('upgrade lib/1.2.3')).toEqual([]);
  });

  it('finds multiple paths in one string', () => {
    const ranges = findPathRanges(
      'edit src/a.ts:1 then src/b.ts:2:3 and finally src/c.ts',
    );
    expect(ranges.length).toBe(3);
    expect(ranges.map((r) => r.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    expect(ranges.map((r) => r.line)).toEqual([1, 2, undefined]);
  });

  it('returns ranges sorted by start ascending', () => {
    const ranges = findPathRanges('src/z.ts and src/a.ts');
    expect(ranges.map((r) => r.start)).toEqual([0, 13]);
  });

  it('matches paths inside parentheses', () => {
    const ranges = findPathRanges('see (src/lib/foo.ts:5) for the bug');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('src/lib/foo.ts');
    expect(ranges[0].line).toBe(5);
  });

  it('rejects paths embedded inside URL paths even when partial', () => {
    // The trailing `/foo.bar` on https://x.com/foo.bar must not match.
    expect(findPathRanges('see https://x.com/path/to/foo.bar')).toEqual([]);
  });

  it('extracts the offsets correctly for substring slicing', () => {
    const text = 'before/x/y.ts:10 after';
    const ranges = findPathRanges(text);
    expect(ranges.length).toBe(1);
    expect(text.slice(ranges[0].start, ranges[0].end)).toBe('before/x/y.ts:10');
  });

  it('does not match paths inside emails-with-paths edge case', () => {
    // `name@host/path.ts` — the `@` lookbehind blocks linkification.
    expect(findPathRanges('user@host/path.ts')).toEqual([]);
  });

  it('matches a leading `./` path', () => {
    const ranges = findPathRanges('see ./src/foo.ts');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('./src/foo.ts');
  });

  it('matches a leading `../` path', () => {
    const ranges = findPathRanges('relative ../sibling/x.ts:1');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('../sibling/x.ts');
    expect(ranges[0].line).toBe(1);
  });

  it('produces no false positive on consecutive calls (regex statefulness)', () => {
    const a = findPathRanges('src/a.ts');
    const b = findPathRanges('src/b.ts');
    expect(a.length).toBe(1);
    expect(b.length).toBe(1);
  });

  it('matches a path inside double quotes', () => {
    const ranges = findPathRanges('Edited "src/lib/foo.ts" successfully');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('src/lib/foo.ts');
  });

  it('matches a path inside backticks', () => {
    const ranges = findPathRanges('check `src/lib/foo.ts:42` for bug');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('src/lib/foo.ts');
    expect(ranges[0].line).toBe(42);
  });

  it('matches a path before an angle-bracket close', () => {
    const ranges = findPathRanges('see <src/lib/foo.ts> for context');
    expect(ranges.length).toBe(1);
    expect(ranges[0].path).toBe('src/lib/foo.ts');
  });
});
