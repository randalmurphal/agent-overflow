/**
 * Proven appends: nominal proof that one parser input extends another.
 *
 * Every incremental fast path in this tree keys on "the new source is the old
 * source plus a suffix". Establishing that by inspecting the strings is what
 * the fast paths exist to avoid: `String#startsWith` on a V8 cons string
 * FLATTENS it, so the prefix check alone copies the whole document on every
 * revealed word. The producer of the delta (the app's formatter/splitter)
 * already knows the lineage, so it mints a proof here and hands it down; a
 * stale or fabricated proof simply fails `matchesProvenAppend` and falls back
 * to a full parse.
 */
declare const provenAppendBrand: unique symbol;
export type ProvenAppend = Readonly<{
    previous: string;
    delta: string;
    next: string;
    [provenAppendBrand]: true;
}>;
const provenAppends = new WeakSet<object>();
// The brand is nominal, not structural: minting here and registering in the
// WeakSet is what `matchesProvenAppend` checks, so the cast is the mint.
const mintProvenAppend = (previous: string, delta: string, next: string): ProvenAppend => {
    const proof = Object.freeze({ previous, delta, next }) as unknown as ProvenAppend;
    provenAppends.add(proof);
    return proof;
};
export const createProvenAppend = (previous: string, delta: string): ProvenAppend => {
    return mintProvenAppend(previous, delta, previous + delta);
};
// V8 represents a non-trivial String#slice as a SlicedString that retains its
// complete parent. Joining two non-empty halves copies the code units into an
// independent sequential string. Keep this at the parser boundary: cached
// block raws live for the whole mounted message and must not pin the full
// document buffer from the checkpoint where marked produced each token.
export const materializeString = (value: string): string => {
    if (value.length < 2)
        return value;
    const middle = value.length >>> 1;
    return [value.slice(0, middle), value.slice(middle)].join('');
};
export const createMaterializedProvenAppend = (previous: string, deltas: readonly string[]): ProvenAppend => {
    if (deltas.length === 0)
        throw new Error('materialized append needs one or more non-empty deltas');
    for (const delta of deltas) {
        if (delta.length === 0)
            throw new Error('materialized append needs one or more non-empty deltas');
    }
    const delta = deltas.length === 1 ? deltas[0] : deltas.join('');
    // Array#join materializes one independent parser string. Unlike a `+`
    // concatenation, parsing and flattening it cannot mutate the canonical reveal
    // rope and leave every prior full-message checkpoint in that rope.
    const next = previous.length > 0
        ? [previous, delta].join('')
        : deltas.length > 1
            ? delta
            : materializeString(delta);
    return mintProvenAppend(previous, delta, next);
};
export const matchesProvenAppend = (
    proof: ProvenAppend | undefined,
    previous: string,
    next: string
): proof is ProvenAppend => proof !== undefined &&
    provenAppends.has(proof) &&
    proof.delta.length > 0 &&
    proof.previous === previous &&
    proof.next === next;
