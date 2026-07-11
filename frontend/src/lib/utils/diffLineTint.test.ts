import { describe, expect, it } from 'vitest';
import { fontStyleClass, gutterTintClass, lineTintClass, prefixTintClass } from './diffLineTint';

describe('lineTintClass', () => {
  it('tints add/del backgrounds only — text keeps the normal fg', () => {
    expect(lineTintClass('add')).toBe('bg-success/12');
    expect(lineTintClass('del')).toBe('bg-error/12');
  });

  it('maps meta to dimmed accent fg', () => {
    expect(lineTintClass('meta')).toBe('text-accent/70');
  });

  it('leaves context untinted', () => {
    expect(lineTintClass('context')).toBe('');
  });
});

describe('gutterTintClass', () => {
  it('tints changed-row gutters a step stronger than the row wash', () => {
    expect(gutterTintClass('add')).toBe('bg-success/20 text-success/75');
    expect(gutterTintClass('del')).toBe('bg-error/20 text-error/75');
  });

  it('keeps context gutters subtle', () => {
    expect(gutterTintClass('context')).toBe('text-fg-subtle');
    expect(gutterTintClass('meta')).toBe('text-fg-subtle');
  });
});

describe('prefixTintClass', () => {
  it('colors only the +/- prefix', () => {
    expect(prefixTintClass('add')).toBe('text-success');
    expect(prefixTintClass('del')).toBe('text-error');
    expect(prefixTintClass('context')).toBe('');
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
