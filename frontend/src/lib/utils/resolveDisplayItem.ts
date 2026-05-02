// Resolves the Item that a transcript row should render given the
// pane's live (buffered) summary state.
//
// The function exists so streaming-text rows hand a STABLE Item
// reference to children when the buffered text matches the persisted
// summary. Without the equality short-circuit, a fresh `{...item,
// summary: liveSummary}` object would be produced on every rAF flush,
// which trickles a new prop reference through ToolCallCard /
// AssistantMessage / ThinkingBlock and forces their derivations to
// re-evaluate even when nothing visible changed. The most painful
// boundary is streaming → terminal: applyLiveStateForUpsert deletes
// the live entry on a non-streaming upsert, but the buffered text by
// that point already equals the persisted summary, so the spread copy
// just churns object identity for one frame before reverting.
//
// `liveSummary === undefined` covers fully-persisted rows; the
// equality branch covers the steady-state-streaming and final-frame
// cases. Both must return the original `item` reference, not a copy.

import type { Item } from '../types/models';

export function resolveDisplayItem(item: Item, liveSummary: string | undefined): Item {
  if (liveSummary === undefined || liveSummary === item.summary) {
    return item;
  }
  return { ...item, summary: liveSummary };
}
