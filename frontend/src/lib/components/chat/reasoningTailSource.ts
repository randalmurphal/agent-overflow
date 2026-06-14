import { revealedSuffix } from '../../utils/textOverlap';

// Body-text sourcing for the reasoning-tail rows. Both kinds — ThinkingBlock
// and CompactionReasoning — render through the shared ReasoningTailRow, which
// clips the reasoning to a sliding 3-line tail while collapsed and reveals the
// full payload once expanded. This module is that row's body-text core: it
// picks the right text for the row's current state and merges the live tail
// with the loaded payload. Extracted as a pure function so the live-tail /
// expansion merge rule is unit-testable and lives in exactly one place.

export interface ReasoningBodyTextInput {
  /** The row's persisted (tail-trimmed) summary — the settled fallback. */
  summary: string;
  /**
   * The per-pane live smoother tail for this item, or null once the stream
   * settles and the smoother disposes. Grows monotonically, so the CSS clip
   * can scroll older lines off the top without re-wrapping a sliding window.
   */
  liveTail: string | null;
  /** Full payload loaded on expand (empty until the user expands). */
  persisted: string;
  expanded: boolean;
  isStreaming: boolean;
}

// reasoningBodyText picks the right body text for the row's current state:
//   - collapsed: the live tail (or the trimmed summary once settled);
//   - expanded + streaming: the loaded snapshot merged with the live reveal
//     into the longer view of the same canonical stream;
//   - expanded + settled: whichever of payload / live is longer.
// revealedSuffix (textOverlap.ts) is containment-aware: when the flushed
// snapshot already leads the reveal it appends nothing rather than duplicating
// the prefix.
export function reasoningBodyText(input: ReasoningBodyTextInput): string {
  const live = input.liveTail ?? input.summary;
  if (!input.expanded) return live;
  if (input.isStreaming) return input.persisted + revealedSuffix(input.persisted, live);
  return input.persisted.length > live.length ? input.persisted : live;
}
