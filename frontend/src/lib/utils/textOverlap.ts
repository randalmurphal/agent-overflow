export function nonOverlappingSuffix(existing: string, delta: string): string {
  if (!existing || !delta) return delta;
  const maxOverlap = Math.min(existing.length, delta.length);
  for (let overlap = maxOverlap; overlap > 0; overlap -= 1) {
    if (existing.endsWith(delta.slice(0, overlap))) {
      return delta.slice(overlap);
    }
  }
  return delta;
}

/**
 * The portion of `revealed` to append to `existing` so the merged text becomes
 * the longer view of the same canonical stream — never re-appending text
 * `existing` already holds.
 *
 * Precondition: `revealed` is a prefix or interior slice of the SAME canonical
 * stream as `existing`, never an unrelated string. That is what makes
 * "contained ⟹ already shown" sound — this is NOT a general-purpose string
 * merge. Both callers satisfy it (payloadExpansion.appendRevealedSuffix and
 * ThinkingBlock.mergeStreamingExpandedText feed reveals of the same item body).
 *
 * `existing` is what we already display (a flushed payload snapshot plus prior
 * live appends); `revealed` is the smoother's latest reveal of the same
 * canonical body. Three cases:
 *   - `revealed` is fully CONTAINED in `existing` → append nothing. This covers
 *     both the snapshot-AHEAD prefix case (GetPayloadData flushes the live
 *     buffer before reading, so the fetched body leads the reveal and
 *     `revealed` is a strict prefix) AND the mid-stream RECONNECT interior case
 *     (the row's smoother reseeds from the bounded-tail summary, so `revealed`
 *     is an interior slice of `existing` rather than a prefix). Either way the
 *     text is already shown, so re-appending it would duplicate.
 *   - `revealed` overlaps the END of `existing` → append only the continuation
 *     tail (nonOverlappingSuffix), the snapshot-BEHIND case.
 *   - no overlap → append all of `revealed`.
 *
 * Why containment (includes), not just prefix (startsWith): nonOverlappingSuffix
 * detects only end-of-existing/start-of-revealed continuation overlap, so on a
 * reconnect it cannot tell that an interior-window `revealed` is already present
 * and re-appends it wholesale (the duplication defect). The containment check
 * fixes that, and it is exact rather than heuristic here:
 *   - A reconnect interior `revealed` is at least as long as the reseed tail
 *     (THINKING_TAIL_RUNES), so a false containment match would require the
 *     canonical reasoning to repeat a passage that long verbatim — which does
 *     not happen.
 *   - When the reveal OVERTAKES the flushed snapshot (a genuine new tail), those
 *     new bytes are not in `existing`, so `revealed` is no longer contained and
 *     the new tail falls through to the suffix scan — it is never dropped.
 *   - In the impossible verbatim-repeat case (the reasoning literally repeats a
 *     passage at least as long as the reseed tail), returning '' would
 *     transiently UNDER-count the repeat while streaming — never duplicate. That
 *     self-heals at settle, when the payload refetch reloads the authoritative
 *     full text; the same bound that kept the original duplication harmless.
 * This keeps the merge correct without the smoother having to track an absolute
 * reveal offset. The startsWith check stays as a cheap fast-path for the common
 * offset-0 stream so the O(n*m) includes only runs when the reveal isn't a
 * plain prefix.
 */
export function revealedSuffix(existing: string, revealed: string): string {
  if (!existing || !revealed) return revealed;
  if (existing.startsWith(revealed)) return '';
  if (existing.includes(revealed)) return '';
  return nonOverlappingSuffix(existing, revealed);
}
