import { describe, expect, it } from 'vitest';
import { gutterTintClass, lineTintClass, prefixTintClass } from './diffLineTint';

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
