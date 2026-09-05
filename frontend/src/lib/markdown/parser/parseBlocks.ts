/**
 * Document -> outer block boundaries, incrementally.
 *
 * `parseBlocks` splits one markdown document into the raw strings each
 * rendered block owns. It is called once per Streamdown evaluation, so the
 * whole design is about NOT re-lexing sealed prefix on every streamed word:
 * a proven append re-lexes only the tail, and a trailing-block descent
 * record (`parseBlocks.cache.ts`) lets that tail start inside the last
 * list, table or open fence. Every guard falls back to a full lex, so
 * correctness never depends on a fast path.
 */
import { Lexer } from './engine';
import {
    DEFAULT_BLOCK_OPTIONS,
    blockExtensionsOf,
    getBlockOptions,
    isKeptType,
    sameExtensionSequence
} from './lexer';
import type { Extension } from './lexer';
import { createProvenAppend } from './provenAppend';
import type { ProvenAppend } from './provenAppend';
import {
    lastLineStartOf,
    openFenceInfo,
    paragraphAppendSafe,
    scanFenceBody
} from './geometry';
import type { BlockToken } from './geometry';
import {
    appendParseBlockRaw,
    replaceParseBlockRaw,
    trailingBlockMayMergeBackward,
    truncateParseBlocksCache,
    updateTrailingBlockRecord
} from './parseBlocks.cache';
import type { ParseBlocksCache, ParseBlocksLexPath } from './parseBlocks.cache';
import { parseListSource } from './extensions/list';
import { parseTableBlockSource } from './extensions/tableSource';
import { parseBlockquoteSource } from './extensions/blockquoteSource';
const INITIAL_HEADING = /^ {0,3}#{1,6}(?:[ \t]+|$)/;
const initialBlockToken = (markdown: string, extensions: Extension[]): BlockToken | null => {
    if (extensions.some(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing))
        return null;
    const list = parseListSource(markdown, DEFAULT_BLOCK_OPTIONS, true);
    if (list?.raw === markdown)
        return list;
    const table = parseTableBlockSource(markdown, true);
    if (table?.raw === markdown)
        return { type: 'table', ...table };
    const blockquote = parseBlockquoteSource(markdown);
    if (blockquote === markdown)
        return { type: 'blockquote', raw: markdown };
    const fence = openFenceInfo(markdown);
    if (fence)
        return { type: 'code', raw: markdown };
    if (!/[\r\n]/.test(markdown)) {
        if (paragraphAppendSafe(markdown))
            return { type: 'paragraph', raw: markdown };
        if (INITIAL_HEADING.test(markdown))
            return { type: 'heading', raw: markdown };
    }
    return null;
};
const blockTokensOf = (
    markdown: string,
    extensions: Extension[],
    cache: ParseBlocksCache | undefined,
    path: ParseBlocksLexPath
): BlockToken[] => {
    cache?.observeLex?.(path, markdown.length, markdown);
    return new Lexer(getBlockOptions(extensions)).blockTokens(markdown, []);
};
export const parseBlocks = (
    markdown: string,
    extensions: Extension[] = [],
    cache?: ParseBlocksCache,
    provenAppend?: ProvenAppend
): string[] => {
    const blocks = parseBlocksCached(markdown, extensions, cache, provenAppend);
    if (cache) {
        // Appending inside an established block cannot advance its source
        // boundary. In particular, never inspect a growing fence per reveal.
        switch (cache.lastPath) {
            case 'unchanged':
            case 'paragraph-append':
            case 'line-block-append':
            case 'list-line-append':
            case 'table-line-append':
            case 'fence-append':
                return blocks;
        }
        if (cache.content.length === 0) {
            cache.source.reset('');
        } else if (cache.blockRawIndexes.length > 0) {
            // Backward-merging markers need two rendered blocks plus the byte
            // preceding them. All intervening omitted source stays exact too.
            const slack = trailingBlockMayMergeBackward(cache) ? 2 : 1;
            const rawIndex = cache.blockRawIndexes[Math.max(0, cache.blockRawIndexes.length - slack)];
            cache.source.retainFrom(Math.max(0, cache.rawStarts[rawIndex] - 1));
        }
    }
    return blocks;
};
const parseBlocksCached = (
    markdown: string,
    extensions: Extension[] = [],
    cache?: ParseBlocksCache,
    provenAppend?: ProvenAppend
): string[] => {
    if (cache) {
        cache.lastBlockAppend = undefined;
        cache.lastPath = 'none';
    }
    const blockExtensions = blockExtensionsOf(extensions);
    const extKey = cache && sameExtensionSequence(cache.extKey, blockExtensions)
        ? cache.extKey
        : blockExtensions;
    // Svelte may re-evaluate this derived when a sibling prop changes while
    // the committed prefix itself is unchanged. Re-lexing the whole document
    // in that case is both unnecessary and destructive to token identity.
    // Inline extensions do not participate in outer block parsing, so their
    // identity is deliberately absent from this cache key; Block.svelte's
    // incrementalLex cache still observes them and refreshes inline output.
    if (cache && cache.extKey === extKey && markdown === cache.content) {
        cache.lastPath = 'unchanged';
        return cache.blocks;
    }
    const appendDelta = cache && cache.extKey === extKey
        ? cache.source.update(markdown, provenAppend)
        : null;
    if (cache && cache.extKey !== extKey) cache.source.reset(markdown);
    if (cache && appendDelta !== null) {
        // Trailing-block descent: when the last rendered block is a list or a
        // table, the block-level tail re-lex below still costs the WHOLE
        // block on every chunk (one list/table = one block token). The
        // record lets the append start inside the block instead — at the
        // list's last item, or at the table's last row (with the header
        // replayed for alignment context). Open fences track their closer
        // candidate without lexing. Each shape therefore costs O(new content)
        // here. Any mismatch
        // falls through to the standard append path.
        const t = cache.trailingBlock;
        if (t && t.rawIndex < cache.raws.length) {
            const blockStart = cache.rawStarts[t.rawIndex];
            const priorRaw = cache.raws[t.rawIndex];
            if ((t.kind === 'paragraph' || t.kind === 'line-block') &&
                blockStart === t.blockStart &&
                (blockStart === 0 || cache.source.charAt(blockStart - 1) === '\n' || cache.source.charAt(blockStart - 1) === '\r') &&
                !/[\r\n]$/.test(priorRaw) &&
                !/[\r\n]/.test(appendDelta) &&
                !extensions.some(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing)) {
                const blockAppend = createProvenAppend(priorRaw, appendDelta);
                replaceParseBlockRaw(cache, t.rawIndex, blockAppend.next);
                cache.content = markdown;
                cache.lastBlockAppend = blockAppend;
                cache.lastPath = t.kind === 'paragraph' ? 'paragraph-append' : 'line-block-append';
                return cache.blocks;
            }
            if (t.kind === 'table-line' &&
                blockStart === t.blockStart &&
                priorRaw.length >= t.prefixLen &&
                !/[\r\n]/.test(appendDelta) &&
                (!/[\r\n]$/.test(priorRaw) || /^ {0,3}\|/.test(appendDelta)) &&
                !extensions.some(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing)) {
                const blockAppend = createProvenAppend(priorRaw, appendDelta);
                replaceParseBlockRaw(cache, t.rawIndex, blockAppend.next);
                cache.content = markdown;
                cache.lastBlockAppend = blockAppend;
                cache.lastPath = 'table-line-append';
                return cache.blocks;
            }
            if (t.kind === 'list-line' &&
                blockStart === t.blockStart &&
                (blockStart === 0 || cache.source.charAt(blockStart - 1) === '\n' || cache.source.charAt(blockStart - 1) === '\r') &&
                !/[\r\n]/.test(appendDelta) &&
                !extensions.some(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing)) {
                let trailingWhitespace = 0;
                for (let i = appendDelta.length - 1; i >= 0; i--) {
                    if (appendDelta[i] !== ' ' && appendDelta[i] !== '\t')
                        break;
                    trailingWhitespace++;
                }
                const priorSourceWhitespace = t.normalized
                    ? cache.source.charAt(blockStart + priorRaw.length - 1)
                    : '';
                const combinedTrailingWhitespace = trailingWhitespace === appendDelta.length
                    ? trailingWhitespace + (t.normalized ? 1 : 0)
                    : trailingWhitespace;
                if (combinedTrailingWhitespace <= 1) {
                    const blockAppend = createProvenAppend(priorRaw, appendDelta);
                    let nextRaw = t.normalized
                        ? priorRaw.slice(0, -1) + priorSourceWhitespace + appendDelta
                        : blockAppend.next;
                    const normalized = combinedTrailingWhitespace === 1;
                    if (normalized) {
                        // Marked's built-in block list parser normalizes one
                        // trailing space/tab into a newline. Two or more are
                        // length-changing and take the lexer fallback above.
                        nextRaw = nextRaw.slice(0, -1) + '\n';
                    }
                    replaceParseBlockRaw(cache, t.rawIndex, nextRaw);
                    if (!t.normalized && !normalized && nextRaw === blockAppend.next)
                        cache.lastBlockAppend = blockAppend;
                    t.normalized = normalized;
                    cache.content = markdown;
                    cache.lastPath = 'list-line-append';
                    return cache.blocks;
                }
            }
            if (t.kind === 'fence' && blockStart === t.blockStart) {
                const scan = scanFenceBody(
                    appendDelta,
                    0,
                    t.char,
                    t.length,
                    t.state,
                    priorRaw.length
                );
                if (!scan.closed) {
                    const blockAppend = createProvenAppend(priorRaw, appendDelta);
                    replaceParseBlockRaw(cache, t.rawIndex, blockAppend.next);
                    cache.content = markdown;
                    t.state = scan.state;
                    cache.lastBlockAppend = blockAppend;
                    cache.lastPath = 'fence-append';
                    return cache.blocks;
                }
            }
            else if (t.kind === 'list') {
                const sliceStart = t.blockStart + t.sealedLen;
                if (blockStart === t.blockStart && sliceStart < markdown.length) {
                    const sliceTokens = blockTokensOf(cache.source.slice(sliceStart), extensions, cache, 'list-descent');
                    const first = sliceTokens[0];
                    let sliceLength = 0;
                    for (const token of sliceTokens)
                        sliceLength += token.raw.length;
                    // Same contiguity guard as the standard path, plus: the slice
                    // must still open with a list (the sealed items' continuation).
                    if (first && first.type === 'list' && sliceStart + sliceLength === markdown.length) {
                        truncateParseBlocksCache(cache, t.rawIndex);
                        let rawStart = t.blockStart;
                        // Sealed source bytes + the tail token's OWN raw — not one
                        // source slice across the span: blockTokens normalizes a
                        // single trailing whitespace char into the token raw as
                        // "\n", and raws must carry what the lexer produced, byte
                        // for byte, or they diverge from a fresh parse. Lengths
                        // are 1:1, so every offset in the record stays valid.
                        rawStart = appendParseBlockRaw(
                            cache,
                            cache.source.slice(t.blockStart, sliceStart) + first.raw,
                            true,
                            rawStart
                        );
                        for (let i = 1; i < sliceTokens.length; i++) {
                            rawStart = appendParseBlockRaw(
                                cache,
                                sliceTokens[i].raw,
                                isKeptType(sliceTokens[i].type),
                                rawStart
                            );
                        }
                        cache.content = markdown;
                        updateTrailingBlockRecord(cache, sliceTokens, sliceStart, t.rawIndex, t.sealedLen);
                        const nextRaw = cache.raws[t.rawIndex];
                        const blockAppend = createProvenAppend(priorRaw, appendDelta);
                        if (nextRaw === blockAppend.next)
                            cache.lastBlockAppend = blockAppend;
                        cache.lastPath = 'list-descent';
                        return cache.blocks;
                    }
                }
            }
            else if (t.kind === 'table' &&
                blockStart === t.blockStart &&
                t.lastRowStart < markdown.length) {
                // Replay the header+align prefix, then lex from the last row.
                // The mini-document must come back as ONE table consuming all
                // of it — anything else (the table ended, a new block opened,
                // the tail rewrote the shape) falls through to the standard
                // append path, which re-lexes the whole trailing region. Only
                // the RAW BOUNDARY matters at this layer, and the table
                // grammar consumes body lines independently of one another,
                // so replaying the identical header bytes makes the mini's
                // boundary decisions exactly the full parse's.
                const prefix = cache.source.slice(t.blockStart, t.blockStart + t.prefixLen);
                const volatileSrc = cache.source.slice(t.lastRowStart);
                const miniTokens = blockTokensOf(prefix + volatileSrc, extensions, cache, 'table-descent');
                const mini = miniTokens[0];
                if (miniTokens.length === 1 &&
                    mini.type === 'table' &&
                    mini.raw.length === t.prefixLen + volatileSrc.length) {
                    truncateParseBlocksCache(cache, t.rawIndex);
                    // Sealed source bytes + the mini token's own tail raw, for
                    // the same byte-fidelity reason as the list branch.
                    const raw = cache.source.slice(t.blockStart, t.lastRowStart) + mini.raw.slice(t.prefixLen);
                    appendParseBlockRaw(cache, raw, true, t.blockStart);
                    cache.content = markdown;
                    const nextLastRow = lastLineStartOf(raw);
                    cache.trailingBlock = nextLastRow > t.prefixLen
                        ? {
                            kind: 'table',
                            rawIndex: t.rawIndex,
                            blockStart: t.blockStart,
                            prefixLen: t.prefixLen,
                            lastRowStart: t.blockStart + nextLastRow
                        }
                        : null;
                    const blockAppend = createProvenAppend(priorRaw, appendDelta);
                    if (raw === blockAppend.next)
                        cache.lastBlockAppend = blockAppend;
                    cache.lastPath = 'table-descent';
                    return cache.blocks;
                }
            }
        }
        // Append-only update: seal everything except the last SEAL_SLACK rendered
        // blocks and re-lex only the tail. cache.raws concatenates exactly to
        // cache.content (verified by length below), so summed lengths are offsets.
        const sealSlack = trailingBlockMayMergeBackward(cache) ? 2 : 1;
        let cut = cache.raws.length;
        let liveBlocks = 0;
        while (cut > 0 && liveBlocks < sealSlack) {
            cut--;
            if (cache.keep[cut])
                liveBlocks++;
        }
        const offset = cut < cache.rawStarts.length
            ? cache.rawStarts[cut]
            : cache.content.length;
        const tailTokens = blockTokensOf(cache.source.slice(offset), extensions, cache, 'append-tail');
        let tailLength = 0;
        for (const token of tailTokens)
            tailLength += token.raw.length;
        // Contiguity guard: if the lexer normalized the tail (so raws no longer
        // reconstruct the input), the offsets cannot be trusted — full reparse.
        if (offset + tailLength === markdown.length) {
            truncateParseBlocksCache(cache, cut);
            let rawStart = offset;
            for (const token of tailTokens) {
                rawStart = appendParseBlockRaw(
                    cache,
                    token.raw,
                    isKeptType(token.type),
                    rawStart
                );
            }
            cache.content = markdown;
            updateTrailingBlockRecord(cache, tailTokens, offset, cut);
            cache.lastPath = 'append-tail';
            return cache.blocks;
        }
    }
    // A fresh one-line tail normally starts as prose, a heading, or a quote.
    // Its outer raw boundary is the whole source regardless of later inline
    // tokenization. Seed that boundary directly; the first newline falls into
    // the normal append lexer and can still reinterpret setext/list/table
    // transitions. This avoids constructing a Lexer for every newly committed
    // streaming block.
    if (cache && cache.raws.length === 0 && cache.content.length === 0) {
        const token = initialBlockToken(markdown, extensions);
        if (token) {
            appendParseBlockRaw(cache, token.raw, true, 0);
            cache.content = markdown;
            updateTrailingBlockRecord(cache, [token], 0, 0);
            cache.lastPath = 'initial-boundary';
            cache.extKey = extKey;
            return cache.blocks;
        }
    }
    // Full parse (first call, non-append update, or contiguity fallback).
    cache?.source.reset(markdown);
    const tokens = blockTokensOf(markdown, extensions, cache, 'full');
    if (cache) {
        truncateParseBlocksCache(cache, 0);
        let total = 0;
        for (const token of tokens) {
            const keep = isKeptType(token.type);
            total = appendParseBlockRaw(cache, token.raw, keep, total);
        }
        // Only trust the cache for future appends if raws reconstruct the input.
        cache.content = total === markdown.length ? markdown : '';
        if (cache.content.length > 0) {
            updateTrailingBlockRecord(cache, tokens, 0, 0);
        }
        else {
            cache.trailingBlock = null;
        }
        cache.lastPath = 'full';
        cache.extKey = extKey;
        return cache.blocks;
    }
    const blocks: string[] = [];
    for (const token of tokens) {
        if (isKeptType(token.type))
            blocks.push(token.raw);
    }
    return blocks;
};
