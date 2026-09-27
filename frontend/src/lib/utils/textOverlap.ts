/**
 * Length of the longest prefix of `text` that `existing` ends with. Linear in
 * the two lengths (a KMP scan of `existing`'s last `text.length` characters).
 */
export function suffixPrefixOverlap(existing: string, text: string): number {
  const max = Math.min(existing.length, text.length);
  if (max === 0) return 0;
  // failure[i]: length of the longest proper border of text[0..i].
  const failure = new Int32Array(max);
  for (let i = 1, k = 0; i < max; i++) {
    while (k > 0 && text.charCodeAt(i) !== text.charCodeAt(k)) k = failure[k - 1];
    if (text.charCodeAt(i) === text.charCodeAt(k)) k++;
    failure[i] = k;
  }
  let matched = 0;
  for (let i = existing.length - max; i < existing.length; i++) {
    const code = existing.charCodeAt(i);
    while (matched > 0 && (matched === max || code !== text.charCodeAt(matched))) {
      matched = failure[matched - 1];
    }
    if (code === text.charCodeAt(matched)) matched++;
  }
  return matched;
}

/**
 * Where `revealed` sits in `existing`, and the portion of it to append so the
 * merged text becomes the longer view of the same canonical stream, never
 * re-appending text `existing` already holds. `offset` is the position of
 * `revealed`'s first character in `existing + suffix`.
 *
 * Precondition: `revealed` is a prefix or interior slice of the SAME canonical
 * stream as `existing`, never an unrelated string. That is what makes
 * "contained ⟹ already shown" sound; this is NOT a general-purpose string
 * merge. Callers pass a live reveal and a loaded body of the same item.
 *
 * `existing` is the body already shown (a flushed payload snapshot, plus any
 * live text appended to it); `revealed` is the smoother's reveal of the same
 * canonical body. Three cases:
 *   - `revealed` is fully CONTAINED in `existing` → append nothing. This covers
 *     both the snapshot-AHEAD prefix case (GetPayloadData flushes the live
 *     buffer before reading, so the fetched body leads the reveal and
 *     `revealed` is a strict prefix) AND the mid-stream RECONNECT interior case
 *     (the row's smoother reseeds from the bounded-tail summary, so `revealed`
 *     is an interior slice of `existing` rather than a prefix). Either way the
 *     text is already shown, so re-appending it would duplicate.
 *   - `revealed` overlaps the END of `existing` → append only the continuation
 *     tail, the snapshot-BEHIND case.
 *   - no overlap → append all of `revealed`.
 *
 * Why containment, not just prefix: the overlap scan detects only
 * end-of-existing/start-of-revealed continuation, so on a reconnect it cannot
 * tell that an interior-window `revealed` is already present and would
 * re-append it wholesale. The containment check is exact rather than
 * heuristic here:
 *   - A reconnect interior `revealed` is at least as long as the reseed tail
 *     (THINKING_TAIL_RUNES), so a false containment match would require the
 *     canonical reasoning to repeat a passage that long verbatim — which does
 *     not happen.
 *   - When the reveal OVERTAKES the flushed snapshot (a genuine new tail), those
 *     new bytes are not in `existing`, so `revealed` is no longer contained and
 *     the new tail falls through to the overlap scan; it is never dropped.
 * The startsWith check is a cheap fast path for the common offset-0 stream.
 */
export function alignRevealed(
  existing: string,
  revealed: string,
): { offset: number; suffix: string } {
  if (!existing || !revealed) return { offset: existing.length, suffix: revealed };
  if (existing.startsWith(revealed)) return { offset: 0, suffix: '' };
  const contained = existing.indexOf(revealed);
  if (contained >= 0) return { offset: contained, suffix: '' };
  const overlap = suffixPrefixOverlap(existing, revealed);
  return { offset: existing.length - overlap, suffix: revealed.slice(overlap) };
}
