// Sprite selection for a working turn: pool assembly over the merged
// catalog (built-ins + customs), the per-turn random pick, and the
// compaction override. Pure — the rail derives through this so the
// rules are testable without a Svelte runtime.

import { BUILTIN_SPRITES, DEFAULT_COMPACTION_SPRITE_ID, type SpinnerSprite } from './catalog';
import { pickFromPool } from './pick';

/** Every known sprite, built-ins first. Custom ids shadow nothing:
 * a custom that reuses a built-in id is a distinct entry, and the
 * pool/compaction resolvers below match the first hit. */
export function mergedSprites(customs: readonly SpinnerSprite[]): SpinnerSprite[] {
  return [...BUILTIN_SPRITES, ...customs];
}

/**
 * The random pool: everything not explicitly unchecked. Exclusion
 * rather than inclusion is the settings shape (spinnerDisabledAnimations)
 * so a newly dropped custom joins the pool without a settings write.
 */
export function assembleSpritePool(
  all: readonly SpinnerSprite[],
  disabledIds: readonly string[],
): SpinnerSprite[] {
  if (disabledIds.length === 0) return [...all];
  const disabled = new Set(disabledIds);
  return all.filter((sprite) => !disabled.has(sprite.id));
}

/**
 * Resolve the compaction slot against the merged catalog. "" means the
 * built-in default, "none" disables the override, anything else is a
 * sprite id — deliberately resolved against ALL sprites, not the pool:
 * assigning a sprite to compaction and unchecking it from the random
 * pool is a coherent configuration. An id that matches nothing answers
 * null and the caller falls through to the random pick.
 */
export function resolveCompactionSprite(
  all: readonly SpinnerSprite[],
  compactionId: string,
): SpinnerSprite | null {
  if (compactionId === 'none') return null;
  const id = compactionId === '' ? DEFAULT_COMPACTION_SPRITE_ID : compactionId;
  return all.find((sprite) => sprite.id === id) ?? null;
}

/**
 * The one entry the rail uses: compaction override when compacting,
 * otherwise the per-turn stable pick from the pool. Null means "show
 * the LED chase" (empty pool, or animations resolved to nothing).
 */
export function selectWorkingSprite(
  customs: readonly SpinnerSprite[],
  disabledIds: readonly string[],
  compacting: boolean,
  compactionId: string,
  threadId: string,
  turnKey: string,
): SpinnerSprite | null {
  const all = mergedSprites(customs);
  if (compacting) {
    const override = resolveCompactionSprite(all, compactionId);
    if (override !== null) return override;
  }
  return pickFromPool(assembleSpritePool(all, disabledIds), threadId, turnKey, 'sprite');
}
