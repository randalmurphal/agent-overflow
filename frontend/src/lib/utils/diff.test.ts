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

  it('classifies + lines as add (but not +++ file header)', () => {
    const out = parseDiffLines('+foo\n+++ b/file.ts');
    expect(out[0]).toEqual({ type: 'add', content: '+foo' });
    // +++ is a file header, not an added line — classified as context
    // so callers can style it the same way they style --- without
    // special cases.
    expect(out[1]).toEqual({ type: 'context', content: '+++ b/file.ts' });
  });

  it('classifies - lines as del (but not --- file header)', () => {
    const out = parseDiffLines('-bar\n--- a/file.ts');
    expect(out[0]).toEqual({ type: 'del', content: '-bar' });
    expect(out[1]).toEqual({ type: 'context', content: '--- a/file.ts' });
  });

  it('classifies lines without +/-/@@ prefix as context', () => {
    const out = parseDiffLines(' unchanged\nplain');
    expect(out[0]).toEqual({ type: 'context', content: ' unchanged' });
    expect(out[1]).toEqual({ type: 'context', content: 'plain' });
  });

  it('preserves the full line content including leading prefix', () => {
    const out = parseDiffLines('+hello');
    expect(out[0]?.content).toBe('+hello');
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
