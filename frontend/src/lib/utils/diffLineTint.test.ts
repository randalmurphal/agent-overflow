import { describe, expect, it } from 'vitest';
import { fontStyleClass, lineTintClass } from './diffLineTint';

describe('lineTintClass', () => {
  it('maps add to success bg + fg', () => {
    expect(lineTintClass('add')).toBe('bg-success/20 text-success');
  });

  it('maps del to error bg + fg', () => {
    expect(lineTintClass('del')).toBe('bg-error/20 text-error');
  });

  it('maps meta to dimmed accent fg', () => {
    expect(lineTintClass('meta')).toBe('text-accent/70');
  });

  it('maps context to text-text-secondary', () => {
    expect(lineTintClass('context')).toBe('text-text-secondary');
  });
});

describe('fontStyleClass', () => {
  it('returns empty string when fontStyle is 0 or undefined', () => {
    expect(fontStyleClass(0)).toBe('');
    expect(fontStyleClass(undefined)).toBe('');
  });

  it('maps each Shiki bit-flag combination to its class string', () => {
    // Shiki fontStyle bits: 1=italic, 2=bold, 4=underline.
    expect(fontStyleClass(1)).toBe('italic');
    expect(fontStyleClass(2)).toBe('font-bold');
    expect(fontStyleClass(3)).toBe('italic font-bold');
    expect(fontStyleClass(4)).toBe('underline');
    expect(fontStyleClass(5)).toBe('italic underline');
    expect(fontStyleClass(6)).toBe('font-bold underline');
    expect(fontStyleClass(7)).toBe('italic font-bold underline');
  });

  it('masks fontStyle to 3 bits — extraneous high bits are ignored', () => {
    // A future Shiki version that adds a 4th bit shouldn't make us
    // index out of range; we cap at the 8-entry table.
    expect(fontStyleClass(0b1111)).toBe('italic font-bold underline');
  });
});
