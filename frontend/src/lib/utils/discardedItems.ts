// Rows Claude discarded after a stalled response.
//
// The backend never touches the discarded rows (completed history is
// immutable); it records the abandonment as its own `notification` row of
// kind `abandoned_blocks` right after them, whose `meta.item_ids` names the
// rows (internal/triage/abandoned_blocks.go). The timeline derives the
// muted marking from those rows, so the marking follows the loaded window
// through reload and paging without any state of its own.
//
// Memoized on the items array's identity: `pane.items` is a `$state.raw`
// array replaced wholesale on membership change and left alone by per-row
// patches, so the scan runs once per membership change, bounded by the
// window, and every row's lookup is a set probe.
import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

export const ABANDONED_BLOCKS_NOTIFICATION_KIND = 'abandoned_blocks';

const EMPTY: ReadonlySet<string> = new Set<string>();
const cache = new WeakMap<readonly Item[], ReadonlySet<string>>();

/** The abandonment record's discarded row ids, in block order. */
export function abandonedItemIds(item: Item): string[] {
  if (item.kind !== 'notification') return [];
  const meta = parseJsonObject(item.meta);
  if (meta?.kind !== ABANDONED_BLOCKS_NOTIFICATION_KIND) return [];
  const ids = meta.item_ids;
  if (!Array.isArray(ids)) return [];
  return ids.filter((id): id is string => typeof id === 'string' && id !== '');
}

/** Whether the record says the whole response was discarded, not a tail of it. */
export function abandonedWholeResponse(item: Item): boolean {
  const meta = parseJsonObject(item.meta);
  return meta?.from_block_index === 0;
}

/** Ids of every row the loaded abandonment records discard. */
export function discardedItemIds(items: readonly Item[]): ReadonlySet<string> {
  const cached = cache.get(items);
  if (cached) return cached;
  let ids: Set<string> | null = null;
  for (const item of items) {
    for (const id of abandonedItemIds(item)) {
      (ids ??= new Set<string>()).add(id);
    }
  }
  const result: ReadonlySet<string> = ids ?? EMPTY;
  cache.set(items, result);
  return result;
}

/**
 * The first assistant row after the record in its scope, if the window
 * holds one: what Claude wrote after discarding. Absent while the reply has
 * not come, or when nothing followed (an error, or the turn ended).
 */
export function assistantRowAfter(items: readonly Item[], record: Item): Item | null {
  const at = items.findIndex((item) => item.id === record.id);
  if (at < 0) return null;
  const scope = record.parentId ?? '';
  for (let i = at + 1; i < items.length; i++) {
    const item = items[i]!;
    if ((item.parentId ?? '') !== scope) continue;
    if (item.kind === 'assistant_text' || item.kind === 'thinking' || item.kind === 'tool_call') return item;
    if (item.kind === 'user_text') return null;
  }
  return null;
}
