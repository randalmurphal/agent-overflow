import type { TextWindow } from '../../utils/liveText';
import { alignRevealed } from '../../utils/textOverlap';

// Body-text sourcing for the reasoning-tail rows. Both kinds, ThinkingBlock
// and CompactionReasoning, render through the shared ReasoningTailRow, which
// clips the reasoning to a sliding 3-line tail while collapsed and reveals the
// full payload once expanded. This module is that row's body-text core: it
// picks the right text for the row's current state. Extracted as a pure
// function so the selection rule is unit-testable and lives in exactly one
// place.

export interface ReasoningBodyTextInput {
  /** The row's persisted (tail-trimmed) summary: the settled fallback. */
  summary: string;
  /**
   * The end of the per-pane live smoother text for this item. Grows only at
   * its end, and starts at 0 or just after a '\n'. TailClampedText scrolls
   * it off the top via CSS clip and bounds its layout cost with a wrap-stable
   * window, both of which rely on that. Retained across a
   * content-consistent settle so the collapsed clamp stays byte-stable at
   * the streaming → settled boundary; null after an overwrite settle, an
   * offscreen prune, a remount, or a thread switch (the trimmed summary
   * takes over there).
   */
  liveWindow: TextWindow | null;
  /** The whole live smoother text, or null. Read only while expanded. */
  liveText: () => string | null;
  /** Full payload loaded on expand (empty until the user expands). */
  persisted: string;
  expanded: boolean;
  isStreaming: boolean;
}

// reasoningBodyText picks the right body text for the row's current state:
//   - collapsed: the live window (or the trimmed summary once settled);
//   - expanded + streaming: the loaded snapshot merged with the live reveal
//     into the longer view of the same canonical stream;
//   - expanded + settled: whichever of payload / live is longer.
// alignRevealed (textOverlap.ts) is containment-aware: when the flushed
// snapshot already leads the reveal it appends nothing rather than duplicating
// the prefix. The expansion handle appends each live reveal to the snapshot
// (payloadExpansion's live appends), so the merge adds only text the handle
// never received, such as a summary ahead of a stale snapshot.
export function reasoningBodyText(input: ReasoningBodyTextInput): TextWindow {
  if (!input.expanded) return input.liveWindow ?? { text: input.summary, start: 0 };
  const live = input.liveText() ?? input.summary;
  if (input.isStreaming) {
    return { text: input.persisted + alignRevealed(input.persisted, live).suffix, start: 0 };
  }
  return { text: input.persisted.length > live.length ? input.persisted : live, start: 0 };
}
