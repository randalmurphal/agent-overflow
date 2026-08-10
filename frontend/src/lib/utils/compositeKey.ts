/**
 * One composite-key join for the registries that key state by a tuple of
 * ids.
 *
 * NUL because it is the one byte none of the parts can contain: thread,
 * item and payload ids are opaque provider strings, and the rest are
 * numbers, booleans or fixed literals — so the join is injective without
 * escaping. `JSON.stringify` was doing the same job with an escape pass,
 * an array allocation and quoting, on functions every rendered row calls
 * on every render.
 *
 * "Injective without escaping" is an assumption about the ids, and an
 * assumption that keys collide silently when it breaks — two different
 * tuples mapping to one key means one row reading another's expansion
 * state. So it is CHECKED rather than trusted: a part carrying the
 * separator throws, naming the part and its position. The cost is one
 * `indexOf` per string part at mounted-row cadence, which is nothing
 * next to the allocation the JSON version was doing.
 *
 * The parameter type is deliberately narrower than what a caller could
 * otherwise hand over: an object or an array would stringify to
 * `[object Object]` and collide with every other one, which JSON hid.
 * Keys are POSITIONAL, so two parts sharing a rendering (the string
 * `"true"` and the boolean `true`) can never be confused for each other.
 */
export const COMPOSITE_KEY_SEPARATOR = '\u0000';

export function compositeKey(
  ...parts: readonly (string | number | boolean)[]
): string {
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index];
    if (typeof part !== 'string') continue;
    if (part.indexOf(COMPOSITE_KEY_SEPARATOR) < 0) continue;
    throw new Error(
      `compositeKey: part ${index} contains the NUL separator and would collide `
        + `with a different key (${JSON.stringify(part)})`,
    );
  }
  return parts.join(COMPOSITE_KEY_SEPARATOR);
}
