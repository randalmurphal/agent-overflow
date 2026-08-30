/**
 * `parseBlocks`' incremental cache: the raw/block arrays it maintains in
 * place, the string-materialization pass that detaches completed blocks from
 * marked's document buffer, and the trailing-block descent record that lets
 * the next append start INSIDE the last block.
 *
 * The cache is opaque to callers: one per Streamdown instance, passed on
 * every call. `parseBlocks.ts` is its only writer.
 */
import { materializeString, matchesProvenAppend } from './provenAppend';
import type { ProvenAppend } from './provenAppend';
import { isKeptType } from './lexer';
import type { Extension } from './lexer';
import {
    openFenceInfo,
    paragraphAppendSafe,
    sealedLengthOf,
    tableAppendInfo,
    tablePrefixLength
} from './geometry';
import type { BlockToken } from './geometry';
export const createParseBlocksCache = (observeLex?: ParseBlocksLexObserver): ParseBlocksCache => ({
    content: '',
    extKey: null,
    raws: [],
    keep: [],
    rawStarts: [],
    blockIndexes: [],
    blockRawIndexes: [],
    blocks: [],
    materializedBlocks: [],
    dirtyBlockStart: 0,
    materializationEnabled: false,
    trailingBlock: null,
    lastBlockAppend: undefined,
    lastPath: 'none',
    observeLex
});
export const truncateParseBlocksCache = (cache: ParseBlocksCache, rawLength: number): void => {
    let blockLength = 0;
    for (let i = rawLength - 1; i >= 0; i--) {
        const blockIndex = cache.blockIndexes[i];
        if (blockIndex >= 0) {
            blockLength = blockIndex + 1;
            break;
        }
    }
    cache.raws.length = rawLength;
    cache.keep.length = rawLength;
    cache.rawStarts.length = rawLength;
    cache.blockIndexes.length = rawLength;
    cache.blockRawIndexes.length = blockLength;
    cache.blocks.length = blockLength;
    cache.dirtyBlockStart = Math.min(cache.dirtyBlockStart, blockLength);
};
export const appendParseBlockRaw = (cache: ParseBlocksCache, raw: string, keep: boolean, start: number): number => {
    const rawIndex = cache.raws.length;
    cache.rawStarts.push(start);
    cache.raws.push(raw);
    cache.keep.push(keep);
    if (keep) {
        const blockIndex = cache.blocks.length;
        cache.blockIndexes.push(blockIndex);
        cache.blockRawIndexes.push(rawIndex);
        cache.blocks.push(raw);
        cache.dirtyBlockStart = Math.min(cache.dirtyBlockStart, blockIndex);
    }
    else {
        cache.blockIndexes.push(-1);
    }
    return start + raw.length;
};
export const replaceParseBlockRaw = (cache: ParseBlocksCache, rawIndex: number, raw: string): void => {
    const previous = cache.raws[rawIndex];
    cache.raws[rawIndex] = raw;
    const blockIndex = cache.blockIndexes[rawIndex];
    if (blockIndex >= 0) {
        cache.blocks[blockIndex] = raw;
        cache.dirtyBlockStart = Math.min(cache.dirtyBlockStart, blockIndex);
    }
    const lengthDelta = raw.length - previous.length;
    for (let i = rawIndex + 1; i < cache.rawStarts.length; i++)
        cache.rawStarts[i] += lengthDelta;
};
/**
 * Detach every published block from marked's full-document input string.
 *
 * Marked returns token raws as V8 substrings. A long-lived block array then
 * retains one whole historical document for every checkpoint that introduced
 * a block. The prior independent value is reused when parseBlocks reparses its
 * two-block safety suffix unchanged, so each completed block is copied once
 * rather than once per later boundary. Work starts at parseBlocks' dirty
 * suffix instead of rescanning the growing block array.
 *
 * The compact completed renderer enables this after parseBlocks. The volatile
 * renderer disables it: its final blocks are replaced on each append, and
 * copying a growing open block per reveal would be O(n²). Disabling clears the
 * independent history and makes a later re-enable revisit every current block.
 */
export const updateParseBlockStringMaterialization = (cache: ParseBlocksCache, enabled: boolean): void => {
    if (!enabled) {
        if (cache.materializationEnabled) {
            cache.materializedBlocks.length = 0;
            cache.dirtyBlockStart = 0;
            cache.materializationEnabled = false;
        }
        return;
    }
    if (!cache.materializationEnabled) {
        cache.dirtyBlockStart = 0;
        cache.materializationEnabled = true;
    }
    for (let blockIndex = cache.dirtyBlockStart; blockIndex < cache.blocks.length; blockIndex++) {
        const rawIndex = cache.blockRawIndexes[blockIndex];
        const current = cache.blocks[blockIndex];
        const previous = cache.materializedBlocks[blockIndex];
        const raw = previous !== undefined && previous === current
            ? previous
            : materializeString(current);
        cache.raws[rawIndex] = raw;
        cache.blocks[blockIndex] = raw;
        cache.materializedBlocks[blockIndex] = raw;
    }
    cache.materializedBlocks.length = cache.blocks.length;
    cache.dirtyBlockStart = cache.blocks.length;
};
// Only the final rendered block normally changes when bytes append. A partial
// table delimiter or setext underline is the exception: marked can expose that
// marker as its own provisional block, then merge it backward once the line
// becomes valid. Keep the preceding block live only for marker-only tails.
// Ignored space/footnote tokens after the rendered block stay in the same raw
// suffix. This avoids feeding marked an unrelated prior list/table on every
// prose reveal while preserving the backward-merge transition.
const BACKWARD_MERGE_MARKER = /^[|:=_*\- \t\r\n]+$/;
export const trailingBlockMayMergeBackward = (cache: ParseBlocksCache): boolean => {
    for (let i = cache.raws.length - 1; i >= 0; i--) {
        if (!cache.keep[i])
            continue;
        const raw = cache.raws[i];
        if (raw.trim().length > 0 && BACKWARD_MERGE_MARKER.test(raw))
            return true;
        const start = cache.rawStarts[i];
        // The simple-table extension can provisionally stop at a closing pipe
        // and expose the rest of that same source line as another block. A later
        // pipe merges both. Any kept token beginning mid-line therefore keeps
        // its predecessor live until a newline seals the split.
        return start > 0 && cache.content[start - 1] !== '\n' && cache.content[start - 1] !== '\r';
    }
    return false;
};
export const appendDeltaOf = (markdown: string, cache: ParseBlocksCache, provenAppend: ProvenAppend | undefined): string | null => {
    if (cache.content.length === 0 || markdown.length <= cache.content.length)
        return null;
    if (provenAppend !== undefined) {
        // Proofs are minted only by createProvenAppend, which constructs next
        // from previous + delta. Identity against both cache generations makes
        // stale or misrouted lineage fall back without rescanning the prefix.
        if (!matchesProvenAppend(provenAppend, cache.content, markdown))
            return null;
        return provenAppend.delta;
    }
    return markdown.startsWith(cache.content)
        ? markdown.slice(cache.content.length)
        : null;
};
// Maintain the trailing-block descent record after a parse pass. `tokens`
// covers the document from `tokensOffset` (byte) / `rawIndex` (raws slot)
// to the end. The record points at the last KEPT block iff it is a list or
// table with a sealable prefix; anything else clears it. `sealedBias`
// folds an already-sealed prefix into token 0's accounting (the list
// descent path lexes only the list's tail, so its token 0 is a suffix of
// the real block; the table descent updates its record directly instead,
// because its slice replays the header and is no suffix of anything).
export const updateTrailingBlockRecord = (
    cache: ParseBlocksCache,
    tokens: readonly BlockToken[],
    tokensOffset: number,
    rawIndex: number,
    sealedBias = 0
): void => {
    for (let i = tokens.length - 1; i >= 0; i--) {
        const token = tokens[i];
        if (token.raw.length === 0)
            continue;
        // A trailing omitted token still owns source bytes. In particular, a
        // blank-line `space` token means the next append starts a new block;
        // descending into the preceding rendered block would splice content
        // across that separator. Footnote definitions are source-owning too.
        if (!isKeptType(token.type)) {
            cache.trailingBlock = null;
            return;
        }
        const bias = i === 0 ? sealedBias : 0;
        let blockStart = tokensOffset;
        for (let j = 0; j < i; j++)
            blockStart += tokens[j].raw.length;
        if (token.type === 'list' && sealedLengthOf(token) + bias > 0) {
            cache.trailingBlock = {
                kind: 'list',
                rawIndex: rawIndex + i,
                blockStart: blockStart - bias,
                sealedLen: sealedLengthOf(token) + bias
            };
            return;
        }
        if (token.type === 'table' && bias === 0) {
            const info = tableAppendInfo(token, token.raw);
            if (info) {
                cache.trailingBlock = {
                    kind: 'table',
                    rawIndex: rawIndex + i,
                    blockStart,
                    prefixLen: info.prefixLen,
                    lastRowStart: blockStart + info.lastRowStart
                };
                return;
            }
            const prefixLen = tablePrefixLength(token, token.raw);
            if (prefixLen !== null) {
                cache.trailingBlock = {
                    kind: 'table-line',
                    rawIndex: rawIndex + i,
                    blockStart,
                    prefixLen
                };
                return;
            }
        }
        if (token.type === 'code' && bias === 0) {
            const fence = openFenceInfo(token.raw);
            if (fence) {
                cache.trailingBlock = {
                    kind: 'fence',
                    rawIndex: rawIndex + i,
                    blockStart,
                    char: fence.char,
                    length: fence.length,
                    state: fence.state
                };
                return;
            }
        }
        const startsOnLine = blockStart === 0 ||
            cache.content[blockStart - 1] === '\n' ||
            cache.content[blockStart - 1] === '\r';
        if (token.type === 'paragraph' &&
            bias === 0 &&
            startsOnLine &&
            paragraphAppendSafe(token.raw)) {
            cache.trailingBlock = {
                kind: 'paragraph',
                rawIndex: rawIndex + i,
                blockStart
            };
            return;
        }
        if (bias === 0 &&
            startsOnLine &&
            !/[\r\n]/.test(token.raw) &&
            (token.type === 'heading' || token.type === 'blockquote')) {
            cache.trailingBlock = {
                kind: 'line-block',
                rawIndex: rawIndex + i,
                blockStart
            };
            return;
        }
        if (token.type === 'list' &&
            bias === 0 &&
            startsOnLine) {
            const sourceEnd = blockStart + token.raw.length - 1;
            const sourceEndChar = cache.content[sourceEnd];
            const normalized = token.raw.endsWith('\n') &&
                (sourceEndChar === ' ' || sourceEndChar === '\t') &&
                !/[\r\n]/.test(token.raw.slice(0, -1));
            if (!/[\r\n]/.test(token.raw) || normalized) {
                cache.trailingBlock = {
                    kind: 'list-line',
                    rawIndex: rawIndex + i,
                    blockStart,
                    normalized
                };
                return;
            }
        }
        cache.trailingBlock = null;
        return;
    }
    cache.trailingBlock = null;
};

export type ParseBlocksLexPath = 'full' | 'append-tail' | 'list-descent' | 'table-descent';
export type ParseBlocksLexObserver = (path: ParseBlocksLexPath, inputLength: number, source: string) => void;
/**
 * Opaque incremental state for `parseBlocks`. Create one per Streamdown
 * instance (or per simulated stream) and pass it on every call: append-only
 * content updates then re-lex only the last couple of blocks instead of the
 * whole document. Any non-append update falls back to a full parse.
 */
export type ParseBlocksCache = {
    content: string;
    /** block-level custom extensions used to build the cached block boundaries */
    extKey: Extension[] | null;
    /** every block token's raw (including space/footnote tokens) in document order */
    raws: string[];
    /** parallel to raws: whether the token is part of the rendered block list */
    keep: boolean[];
    /** parallel to raws: source offset where the token starts */
    rawStarts: number[];
    /** parallel to raws: rendered block index, or -1 for an omitted token */
    blockIndexes: number[];
    /** parallel to blocks: raw-token index that owns each rendered block */
    blockRawIndexes: number[];
    /** cache-owned live rendered-block array, updated in place on each call */
    blocks: string[];
    /** last independent value published at each rendered block index */
    materializedBlocks: string[];
    /** first rendered block changed since the last materialization pass */
    dirtyBlockStart: number;
    /** whether independent completed-block strings are currently retained */
    materializationEnabled: boolean;
    /** descent record when the last rendered block has append-safe internal geometry */
    trailingBlock: {
        kind: 'list';
        rawIndex: number;
        blockStart: number;
        sealedLen: number;
    } | {
        kind: 'table';
        rawIndex: number;
        blockStart: number;
        prefixLen: number;
        lastRowStart: number;
    } | {
        kind: 'table-line';
        rawIndex: number;
        blockStart: number;
        prefixLen: number;
    } | {
        kind: 'fence';
        rawIndex: number;
        blockStart: number;
        char: '`' | '~';
        length: number;
        state: {
            phase: 'leading' | 'run' | 'trailing' | 'invalid';
            indent: number;
            run: number;
            lineStart: number;
        };
    } | {
        kind: 'paragraph';
        rawIndex: number;
        blockStart: number;
    } | {
        kind: 'line-block';
        rawIndex: number;
        blockStart: number;
    } | {
        kind: 'list-line';
        rawIndex: number;
        blockStart: number;
        normalized: boolean;
    } | null;
    /** proven append applied to the last rendered block by the latest call */
    lastBlockAppend?: ProvenAppend;
    /** debug breadcrumb for the latest parseBlocks path */
    lastPath: 'none' | 'unchanged' | 'full' | 'initial-boundary' | 'append-tail' | 'paragraph-append' | 'line-block-append' | 'list-line-append' | 'table-line-append' | 'list-descent' | 'table-descent' | 'fence-append';
    /** optional diagnostic observer invoked for each marked block-lexer call */
    observeLex?: ParseBlocksLexObserver;
};
