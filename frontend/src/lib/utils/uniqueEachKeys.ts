// Keys for a `{#each}` over a list whose key strings come off the wire.
//
// THE HAZARD. Pristine svelte's keyed `{#each}` THROWS `each_key_duplicate`,
// and a throw inside an update flush aborts the whole batch — the pane stops
// rendering and the app looks frozen (production incident 2026-08-29, where
// the duplicate was a subagent card key; `subagentGrouping`'s
// `enforceUniqueTimelineNodeKeys` is the timeline's own copy of this repair).
// The `each-key-repair` vendor hunk (frontend/AGENTS.md § Vendor patches)
// now repairs a repeat inside svelte and REPORTS it as a bug through the
// frontend error log. This helper is for lists whose repeats are legitimate:
// any key derived from provider-, model- or MCP-server-authored text, where
// nothing upstream promises the strings are distinct and a repeat is the
// producer's statement, not a defect to log on every render. The surfaces
// that render them (the question card, the pending-input panel, the
// elicitation form) sit inside a pane that is usually streaming.
//
// WHAT THIS DOES. Returns the natural keys unchanged when they are already
// unique — the overwhelmingly common case, and the one where key identity
// must not move — and otherwise suffixes the later occurrences (`Yes`,
// `Yes#2`, `Yes#3`). Duplicates therefore render as the separate rows the
// producer asked for instead of throwing, and no row is dropped: which
// options a model offered is the model's statement to make, not this
// helper's.
//
// WHEN NOT TO USE IT. A list whose keys are unique BY CONSTRUCTION (item
// ids, object keys, line ranges, an already-deduped set) does not need it —
// wrapping those buys an allocation per derive and hides the invariant.

/**
 * Unique `{#each}` keys for `items`.
 *
 * Returns a plain array to index by the each-block's positional index:
 * `{#each options as option, i (optionKeys[i])}`. The array is
 * reference-fresh per call, so call it from a `$derived` and let the derived
 * memo do the caching.
 */
export function uniqueEachKeys<T>(
  items: readonly T[],
  keyOf: (item: T) => string,
): string[] {
  const keys: string[] = new Array<string>(items.length);
  // One pass, and no Set at all below 2 entries: these lists are option rows
  // and command rows, so the fixed cost matters more than the asymptotics.
  if (items.length === 0) return keys;
  if (items.length === 1) {
    keys[0] = keyOf(items[0] as T);
    return keys;
  }
  const seen = new Set<string>();
  for (let i = 0; i < items.length; i += 1) {
    const natural = keyOf(items[i] as T);
    let key = natural;
    if (seen.has(key)) {
      let occurrence = 2;
      key = `${natural}#${occurrence}`;
      while (seen.has(key)) {
        occurrence += 1;
        key = `${natural}#${occurrence}`;
      }
    }
    seen.add(key);
    keys[i] = key;
  }
  return keys;
}
