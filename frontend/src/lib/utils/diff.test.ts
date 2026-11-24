import { describe, expect, it } from 'vitest';
import { parseDiffLines } from './diff';

describe('parseDiffLines', () => {
  it('returns an empty array for empty input', () => {
    expect(parseDiffLines('')).toEqual([]);
  });

  it('classifies @@ hunk headers as meta', () => {
    expect(parseDiffLines('@@ -1,3 +1,4 @@')).toEqual([
      { type: 'meta', content: '@@ -1,3 +1,4 @@' },
    ]);
  });

  // A +++ or --- file header is not an added or deleted line: it is
  // context, so callers style both headers alike without special cases.
  it.each([
    ['add', '+foo', '+++ b/file.ts'],
    ['del', '-bar', '--- a/file.ts'],
  ])('classifies %s lines like %s, keeping the full line, but not the file header %s', (type, line, header) => {
    expect(parseDiffLines(`${line}\n${header}`)).toEqual([
      { type, content: line },
      { type: 'context', content: header },
    ]);
  });

  it('reads header lookalikes inside a hunk as the lines they are', () => {
    expect(parseDiffLines('--- a/q.sql\n+++ b/q.sql\n@@ -1,2 +1,2 @@\n--- note\n+++i\n select 1;').map((line) => line.type))
      .toEqual(['context', 'context', 'meta', 'del', 'add', 'context']);
  });

  it('classifies lines without +/-/@@ prefix as context', () => {
    const out = parseDiffLines(' unchanged\nplain');
    expect(out[0]).toEqual({ type: 'context', content: ' unchanged' });
    expect(out[1]).toEqual({ type: 'context', content: 'plain' });
  });

  it('handles a realistic mixed hunk', () => {
    const patch =
      '--- a/x.ts\n' +
      '+++ b/x.ts\n' +
      '@@ -1,3 +1,3 @@\n' +
      ' import foo;\n' +
      '-const a = 1;\n' +
      '+const a = 2;\n' +
      ' const b = 3;';
    const types = parseDiffLines(patch).map((line) => line.type);
    expect(types).toEqual([
      'context', // ---
      'context', // +++
      'meta', // @@
      'context', //  import foo;
      'del', // -const a = 1;
      'add', // +const a = 2;
      'context', //  const b = 3;
    ]);
  });
});
