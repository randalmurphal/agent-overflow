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
  /** The row streams, or its reveal still drains after it settled. */
  isStreaming: boolean;
}

// reasoningBodyText picks the right body text for the row's current state:
//   - collapsed: the live window (or the trimmed summary once settled);
//   - expanded + streaming: the loaded snapshot merged with the live reveal,
//     ending where the reveal ends;
//   - expanded + settled: whichever of payload / live is longer.
// alignRevealed (textOverlap.ts) places the reveal in the snapshot. The
// snapshot can lead it (GetPayloadData flushes the stream before reading), and
// the text past the reveal is not shown until the reveal reaches it, so the
// expanded body streams at the collapsed tail's pace instead of landing ahead
// of it in one block. The expansion handle appends each live reveal to the
// snapshot (payloadExpansion's live appends), so the merge adds only text the
// handle never received, such as a summary ahead of a stale snapshot.
export function reasoningBodyText(input: ReasoningBodyTextInput): TextWindow {
  if (!input.expanded) return input.liveWindow ?? { text: input.summary, start: 0 };
  const live = input.liveText() ?? input.summary;
  if (input.isStreaming) {
    if (live === '') return { text: '', start: 0 };
    const { offset, suffix } = alignRevealed(input.persisted, live);
    return { text: (input.persisted + suffix).slice(0, offset + live.length), start: 0 };
  }
  return { text: input.persisted.length > live.length ? input.persisted : live, start: 0 };
}
