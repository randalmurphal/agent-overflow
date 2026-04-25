import { describe, expect, it } from 'vitest';
import { decodeToolCardPreview } from './toolCardPreview';

describe('decodeToolCardPreview', () => {
  it('returns plain text when the input has no path', () => {
    expect(decodeToolCardPreview('Waiting on agents')).toEqual({
      text: 'Waiting on agents',
    });
  });

  it('returns plain text for empty input', () => {
    expect(decodeToolCardPreview('')).toEqual({ text: '' });
  });

  it('extracts a leading path with line + col', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts:12:7 read OK');
    expect(decoded.text).toBe('src/lib/foo.ts:12:7 read OK');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: 12, col: 7 });
  });

  it('extracts a leading path with only a line', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts:12');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: 12, col: undefined });
  });

  it('extracts a bare leading path', () => {
    const decoded = decodeToolCardPreview('src/lib/foo.ts');
    expect(decoded.path).toEqual({ path: 'src/lib/foo.ts', line: undefined, col: undefined });
  });

  it('does not extract paths that appear mid-string', () => {
    const decoded = decodeToolCardPreview('Wrote 3 files (a.ts, b.ts, src/c.ts)');
    expect(decoded.path).toBeUndefined();
  });

  it('does not match URLs as paths', () => {
    const decoded = decodeToolCardPreview('https://example.com/foo.bar');
    expect(decoded.path).toBeUndefined();
  });
});
