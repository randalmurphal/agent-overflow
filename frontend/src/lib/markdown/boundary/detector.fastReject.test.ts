import { describe, expect, it, vi } from 'vitest';
import {
  createInitialContext,
  isSetextHeadingUnderline,
  isTableDelimiter,
  isThematicBreak,
  updateContext,
} from './detector';

describe('boundary detector fast rejection', () => {
  it('rejects an ordinary growing line without trimming the whole string', () => {
    const trim = vi.spyOn(String.prototype, 'trim').mockImplementation(() => {
      throw new Error('ordinary line fell back to a full trim');
    });
    try {
      const line = 'ordinary prose '.repeat(10_000);
      expect(isSetextHeadingUnderline(line, 'heading')).toBe(false);
      expect(isThematicBreak(line)).toBe(false);
      expect(isTableDelimiter(line)).toBe(false);
    } finally {
      trim.mockRestore();
    }
  });

  it('preserves surrounding-whitespace acceptance for structural lines', () => {
    expect(isSetextHeadingUnderline('   ===   ', 'Heading')).toBe(true);
    expect(isSetextHeadingUnderline('    ===   ', 'Heading')).toBe(false);
    expect(isThematicBreak('   ***   ')).toBe(true);
    expect(isTableDelimiter('  | :--- | ---: |   ')).toBe(true);
  });

  it('reuses immutable context while an ordinary line changes no block state', () => {
    const initial = createInitialContext();
    const ordinary = updateContext('ordinary prose', initial);
    expect(ordinary).toBe(initial);

    const fenced = updateContext('```ts', ordinary);
    expect(fenced).not.toBe(initial);
    expect(fenced.inFencedCode).toBe(true);
    expect(initial.inFencedCode).toBe(false);

    const fenceBody = updateContext('const value = 1;', fenced);
    expect(fenceBody).toBe(fenced);

    const closed = updateContext('```', fenceBody);
    expect(closed).not.toBe(fenceBody);
    expect(closed.inFencedCode).toBe(false);
    expect(fenceBody.inFencedCode).toBe(true);
  });
});
