import { describe, expect, it } from 'vitest';
import { BUILTIN_SPRITES, DEFAULT_COMPACTION_SPRITE_ID, type SpinnerSprite } from './catalog';
import {
  assembleSpritePool,
  mergedSprites,
  resolveCompactionSprite,
  selectWorkingSprite,
} from './select';

function custom(id: string): SpinnerSprite {
  return {
    id,
    label: id,
    src: `data:image/png;base64,${id}`,
    frames: 4,
    frameMs: 100,
    frameWidth: 72,
    frameHeight: 72,
    custom: true,
  };
}

describe('mergedSprites', () => {
  it('is builtins plus customs, builtins first', () => {
    const all = mergedSprites([custom('mine')]);
    expect(all.length).toBe(BUILTIN_SPRITES.length + 1);
    expect(all.at(-1)?.id).toBe('mine');
  });
});

describe('assembleSpritePool', () => {
  it('excludes disabled ids only', () => {
    const all = mergedSprites([]);
    const pool = assembleSpritePool(all, ['nyan-cat']);
    expect(pool.length).toBe(all.length - 1);
    expect(pool.some((sprite) => sprite.id === 'nyan-cat')).toBe(false);
  });

  it('ignores unknown disabled ids', () => {
    const all = mergedSprites([]);
    expect(assembleSpritePool(all, ['no-such']).length).toBe(all.length);
  });
});

describe('resolveCompactionSprite', () => {
  const all = mergedSprites([custom('mine')]);

  it('resolves "" to the built-in default', () => {
    expect(resolveCompactionSprite(all, '')?.id).toBe(DEFAULT_COMPACTION_SPRITE_ID);
  });

  it('resolves "none" to null', () => {
    expect(resolveCompactionSprite(all, 'none')).toBeNull();
  });

  it('resolves a custom id', () => {
    expect(resolveCompactionSprite(all, 'mine')?.custom).toBe(true);
  });

  it('answers null for an id that matches nothing', () => {
    expect(resolveCompactionSprite(all, 'ghost')).toBeNull();
  });
});

describe('selectWorkingSprite', () => {
  it('holds the compaction override while compacting', () => {
    const sprite = selectWorkingSprite([], [], true, '', 't1', 'turn1');
    expect(sprite?.id).toBe(DEFAULT_COMPACTION_SPRITE_ID);
  });

  it('falls through to the random pick when the slot is none', () => {
    const random = selectWorkingSprite([], [], false, 'none', 't1', 'turn1');
    const compacting = selectWorkingSprite([], [], true, 'none', 't1', 'turn1');
    expect(compacting?.id).toBe(random?.id);
  });

  it('falls through when the assigned id does not exist', () => {
    const random = selectWorkingSprite([], [], false, '', 't1', 'turn1');
    const compacting = selectWorkingSprite([], [], true, 'ghost', 't1', 'turn1');
    expect(compacting?.id).toBe(random?.id);
  });

  it('compaction sprite works even when unchecked from the pool', () => {
    const disabled = [DEFAULT_COMPACTION_SPRITE_ID];
    const sprite = selectWorkingSprite([], disabled, true, '', 't1', 'turn1');
    expect(sprite?.id).toBe(DEFAULT_COMPACTION_SPRITE_ID);
  });

  it('answers null when everything is unchecked', () => {
    const disabled = mergedSprites([]).map((sprite) => sprite.id);
    expect(selectWorkingSprite([], disabled, false, 'none', 't1', 'turn1')).toBeNull();
  });

  it('is stable within a turn and can differ across turns', () => {
    const first = selectWorkingSprite([], [], false, '', 't1', 'turn1');
    expect(selectWorkingSprite([], [], false, '', 't1', 'turn1')?.id).toBe(first?.id);
    const across = new Set(
      Array.from(
        { length: 30 },
        (_, i) => selectWorkingSprite([], [], false, '', 't1', `turn${i}`)?.id,
      ),
    );
    expect(across.size).toBeGreaterThan(1);
  });
});
