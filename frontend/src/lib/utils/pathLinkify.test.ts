import { describe, expect, it } from 'vitest';
import { findPathRanges, getPathRefsFromMeta } from './pathLinkify';
import { __resetParseJsonObjectCacheForTest } from './parseJsonObject';

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

  it('consumes a :line-endLine range, exposing only the start line', () => {
    // The range bound is consumed (so the matched range covers the
    // full `:18-23` suffix, not just `:18`) but not surfaced — callers
    // open at the start line.
    const text = 'see src/lib/foo.ts:18-23 for context';
    const ranges = findPathRanges(text);
    expect(ranges).toEqual([
      { start: 4, end: 24, path: 'src/lib/foo.ts', line: 18, col: undefined },
    ]);
    // Lock the shape — no `endLine` field gets bolted on by accident.
    // A future change that decides to surface the range bound has to
    // update this test deliberately, not slip past it.
    expect('endLine' in ranges[0]).toBe(false);
    expect(text.slice(ranges[0].start, ranges[0].end)).toBe('src/lib/foo.ts:18-23');
  });

  it('degrades cleanly when the range bound is not digits', () => {
    // `:18-foo` is the failure mode that exercises the `-\d+`
    // alternative's lower bound. Without the `\d+` anchor, the regex
    // could over-consume the trailing word; with it, the suffix
    // matches `:18` only and `-foo` stays as trailing text.
    const text = 'see src/lib/foo.ts:18-foo for context';
    const ranges = findPathRanges(text);
    expect(ranges).toEqual([
      { start: 4, end: 21, path: 'src/lib/foo.ts', line: 18, col: undefined },
    ]);
    expect(text.slice(ranges[0].start, ranges[0].end)).toBe('src/lib/foo.ts:18');
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

  it('does not match trailing-dot tokens like something/else.', () => {
    // Regression: the old heuristic only required ANY dot in the
    // final segment, so `something/else.` linkified — see the chat
    // surface bug that motivated this refactor. The Go-side rule
    // mirrors this exclusion (`internal/pathlinks/pathlinks.go`).
    expect(findPathRanges('look in something/else. please')).toEqual([]);
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

  it('does not match @-prefixed paths in local mode (scoped-npm collision)', () => {
    // The local matcher cannot distinguish `@scope/pkg.foo` (scoped
    // npm) from `@workspace/file.ts` (real path) without fs validation,
    // so it rejects everything `@`-prefixed. The widened @-span lives
    // only in `enhancePathLinks` allowlist mode where Go has already
    // confirmed the file exists.
    expect(findPathRanges('see @src/foo.ts here')).toEqual([]);
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

describe('getPathRefsFromMeta', () => {
  it('returns undefined for empty/missing meta', () => {
    __resetParseJsonObjectCacheForTest();
    expect(getPathRefsFromMeta(undefined)).toBeUndefined();
    expect(getPathRefsFromMeta('')).toBeUndefined();
    expect(getPathRefsFromMeta(null)).toBeUndefined();
  });

  it('returns undefined when meta has no pathRefs key', () => {
    __resetParseJsonObjectCacheForTest();
    // Pre-pathlinks history rows: meta exists (e.g. carries task_id)
    // but no pathRefs. Caller should fall through to "render plain"
    // rather than treating absent-pathRefs as empty-pathRefs.
    expect(getPathRefsFromMeta('{"task_id":"abc"}')).toBeUndefined();
  });

  it('returns undefined for non-array pathRefs', () => {
    __resetParseJsonObjectCacheForTest();
    expect(getPathRefsFromMeta('{"pathRefs":"not-an-array"}')).toBeUndefined();
  });

  it('parses valid pathRefs into PathRef[]', () => {
    __resetParseJsonObjectCacheForTest();
    const meta = JSON.stringify({
      pathRefs: [
        { path: 'src/a.ts' },
        { path: 'src/b.ts', line: 12 },
        { path: 'src/c.ts', line: 1, col: 4 },
      ],
    });
    const refs = getPathRefsFromMeta(meta);
    expect(refs).toEqual([
      { path: 'src/a.ts' },
      { path: 'src/b.ts', line: 12 },
      { path: 'src/c.ts', line: 1, col: 4 },
    ]);
  });

  it('drops malformed entries silently', () => {
    __resetParseJsonObjectCacheForTest();
    const meta = JSON.stringify({
      pathRefs: [
        { path: 'src/a.ts' },
        { path: '' }, // empty path
        { line: 5 }, // missing path
        'not-an-object',
        { path: 'src/b.ts', line: 0 }, // line=0 stays unset
      ],
    });
    const refs = getPathRefsFromMeta(meta);
    expect(refs).toEqual([
      { path: 'src/a.ts' },
      { path: 'src/b.ts' },
    ]);
  });
});
