/**
 * The append merges: how a tail-only re-lex is spliced back onto sealed,
 * reference-identical tokens for the two shapes that are one block token but
 * many independently-parsed units — lists (items) and tables (source rows) —
 * plus the position-matched token reuse both rely on.
 *
 * Every function here returns null rather than guessing. A null is a full
 * re-lex in `incrementalLex.ts`, which is always correct.
 */
import { Lexer } from 'marked';
import type { Token } from 'marked';
import { getLexOptions, lexCapture } from './lexer';
import { footnoteLexer } from './extensions/footnotes';
import type { Extension } from './lexer';
import { lastLineStartOf, sealedLengthOf, tableTailUnsafe } from './geometry';
import type { TableAppendInfo } from './geometry';
import type { IncrementalLexCache } from './incrementalLex.cache';
import { tokenizeListItemContent } from './extensions/list';
import type { ListItemToken, ListToken } from './extensions/list';
import { tokenizeTableTail } from './extensions/table';
import type { TableToken } from './extensions/table';
// Marked recreates every token object on a fallback lex. Svelte then treats
// those fresh objects as changed each-items and re-runs the nested branch tree,
// even when all but the trailing inline token are byte-identical. Reuse only
// position-matched tokens whose complete observable shape is equal. Unknown
// extension fields are compared too. Any object-valued field other than
// `tokens` keeps the new token rather than assuming an extension contract.
const sameTokenFields = (previous: TokenRecord, next: TokenRecord): boolean => {
    let previousCount = 0;
    let nextCount = 0;
    for (const key in previous) {
        if (Object.hasOwn(previous, key))
            previousCount++;
    }
    for (const key in next) {
        if (!Object.hasOwn(next, key))
            continue;
        nextCount++;
        if (key === 'tokens')
            continue;
        if (!Object.hasOwn(previous, key) || !Object.is(previous[key], next[key]))
            return false;
    }
    return previousCount === nextCount && previous.tokens === next.tokens;
};
export const reuseUnchangedTokens = <T extends ReusableToken>(previous: T[], next: T[]): T[] => {
    let allReused = previous.length === next.length;
    for (let index = 0; index < next.length; index++) {
        const priorToken = previous[index];
        const nextToken = next[index];
        if (!priorToken || !nextToken || priorToken.type !== nextToken.type) {
            allReused = false;
            continue;
        }
        const priorChildren = priorToken.tokens;
        const nextChildren = nextToken.tokens;
        if (isReusableTokenArray(priorChildren) && isReusableTokenArray(nextChildren)) {
            nextToken.tokens = reuseUnchangedTokens(priorChildren, nextChildren);
        }
        if (sameTokenFields(priorToken as TokenRecord, nextToken as TokenRecord)) {
            next[index] = priorToken;
        }
        else {
            allReused = false;
        }
    }
    return allReused ? previous : next;
};
// Does this slice DECLARE any definitions — reference links or footnotes?
// Link defs: block-only pass (they register during block tokenization),
// gated behind a cheap substring probe so def-free tails — nearly all of
// them — never pay it. Footnote defs: a textual line test, because the
// hazard is not the def itself but the maps: a still-streaming def line
// tokenizes as a REF until its colon arrives, hijacking the shared
// refs-map slot, so the def's later in-place mutation lands on that
// discarded transient token instead of the sealed ref. Bailing every
// tick the def sits in the tail keeps sealed refs refreshed by full
// lexes instead (a fence line shaped like a def only costs a fallback).
const FOOTNOTE_DEF_LINE = /(^|\n)[ \t]*\[\^[^\]\n]+\]:/;
const declaresDefs = (src: string, extensions: Extension[]): boolean => {
    if (!src.includes(']:'))
        return false;
    if (FOOTNOTE_DEF_LINE.test(src))
        return true;
    const lexer = new Lexer(getLexOptions(extensions));
    lexer.blockTokens(src, []);
    for (const _ in lexer.tokens.links)
        return true;
    return false;
};
// Mirror of finalizeList's loose branch for standalone-lexed tail items:
// paragraph-wrapped block tokens per item, then the inline pass the outer
// lex() would have run over its queue — seeded like the merge lexer so
// reflinks and footnotes inside loosened items resolve identically. Item
// content goes through markedList's tokenizeListItemContent, the same
// chokepoint finalizeList uses, so the marker-line indented-code rewrite
// cannot depend on which path re-tokenized the item.
const loosenTailItems = (items: ListItemToken[], extensions: Extension[], cache: IncrementalLexCache): void => {
    const lexer = footnoteLexer(new Lexer(getLexOptions(extensions)));
    if (cache.links)
        Object.assign(lexer.tokens.links, cache.links);
    if (cache.footnotes) {
        lexer.footnotes = cache.footnotes;
        lexer.hasFootnotes = true;
    }
    for (const item of items) {
        item.loose = true;
        tokenizeListItemContent(lexer, item, true);
    }
    // marked declares `inlineQueue` private; draining it here is what the
    // outer `lex()` would have done for these standalone-lexed items.
    const queue = (lexer as unknown as { inlineQueue: { src: string; tokens: Token[] }[] }).inlineQueue;
    for (let i = 0; i < queue.length; i++) {
        const next = queue[i];
        lexer.inlineTokens(next.src, next.tokens);
    }
};
export const mergeTrailingList = (
    cachedList: ListToken,
    base: string,
    extensions: Extension[],
    complete: ((markdown: string) => string) | null,
    cache: IncrementalLexCache
): ListToken | null => {
    const sealedLen = sealedLengthOf(cachedList);
    if (sealedLen <= 0 || sealedLen >= base.length)
        return null;
    const tailSrc = base.slice(sealedLen);
    const completedTail = complete ? complete(tailSrc) : tailSrc;
    if (declaresDefs(completedTail, extensions))
        return null;
    const result = lexCapture(completedTail, extensions, cache.links, cache.footnotes);
    const tailTokens = result.tokens;
    if (tailTokens.length !== 1)
        return null;
    const tail = tailTokens[0];
    if (tail.type !== 'list' ||
        tail.ordered !== cachedList.ordered ||
        tail.listType !== cachedList.listType ||
        tail.tokens.length === 0)
        return null;
    // Tight → loose flip: a list-global rendering change; one full re-lex.
    if (tail.loose && !cachedList.loose)
        return null;
    if (cachedList.loose && !tail.loose)
        loosenTailItems(tail.tokens, extensions, cache);
    // A footnote first tokenized by this tail lex must stay reachable by
    // later ticks (its ref seals; a def arriving afterwards mutates it
    // through these maps). No links carry: the tail declared none.
    if (result.footnotes)
        cache.footnotes = result.footnotes;
    const sealed = cachedList.tokens.slice(0, -1);
    const priorTail = cachedList.tokens[cachedList.tokens.length - 1];
    if (priorTail && tail.tokens.length > 0)
        tail.tokens = reuseUnchangedTokens([priorTail], tail.tokens);
    const merged = {
        ...cachedList,
        raw: base.slice(0, sealedLen) + tail.raw,
        loose: cachedList.loose,
        tokens: sealed.concat(tail.tokens)
    };
    if (merged.ordered) {
        // markedList's expectedValue chain compares item k (k >= 1) against
        // start + k - 1. The standalone tail lex numbered its items from its
        // own first item; restamp the spliced ones against the merged chain.
        const start = merged.start ?? 1;
        for (let k = sealed.length; k < merged.tokens.length; k++) {
            const item = merged.tokens[k];
            item.skipped = k > 0 && item.value !== null && item.value !== start + k - 1;
        }
    }
    return merged;
};
// The table analog: sealed rows are every body row before the last SOURCE
// LINE of the cached document. Without rowspan a row's parse depends only
// on the header (alignment, column count) — the mini-document replays the
// header bytes verbatim over the volatile rows, and its tbody splices
// onto the reference-identical sealed rows. Reference-link definitions
// cannot occur INSIDE a table (a def-shaped line parses as a row), so no
// def machinery here; footnote refs in cells resolve through the shared
// maps exactly as sealed-row reuse requires (a late definition mutates
// the sealed ref token in place).
export const mergeTrailingTable = (
    cachedTable: TableToken,
    base: string,
    appendDelta: string,
    extensions: Extension[],
    complete: ((markdown: string) => string) | null,
    tableTailUnstable: boolean,
    appendInfo: TableAppendInfo | null
): { token: TableToken; appendInfo: TableAppendInfo } | null => {
    if (tableTailUnstable || !appendInfo)
        return null;
    if (appendInfo.lastRowStart >= base.length)
        return null;
    // Rowspan (`^` mutates the PREVIOUS row's cells) and footer markers
    // (re-home the last row into a tfoot) break sealed-row immutability.
    const volatileSrc = appendInfo.lastRow + appendDelta;
    if (tableTailUnsafe(volatileSrc))
        return null;
    const miniSrc = appendInfo.prefix + volatileSrc;
    const completedMini = complete ? complete(miniSrc) : miniSrc;
    if (completedMini !== miniSrc)
        return null;
    const cachedHead = Array.isArray(cachedTable.tokens) ? cachedTable.tokens[0] : null;
    const cachedBody = Array.isArray(cachedTable.tokens) ? cachedTable.tokens[1] : null;
    if (cachedHead?.type !== 'thead' ||
        !Array.isArray(cachedHead.tokens) ||
        cachedBody?.type !== 'tbody' ||
        !Array.isArray(cachedBody.tokens) ||
        !Array.isArray(cachedTable.align))
        return null;
    const tailLexer = new Lexer(getLexOptions(extensions));
    const mini = tokenizeTableTail(completedMini, tailLexer);
    if (!mini || mini.headerRowCount !== cachedHead.tokens.length || mini.align.length !== cachedTable.align.length)
        return null;
    for (let index = 0; index < mini.align.length; index++) {
        if (mini.align[index] !== cachedTable.align[index])
            return null;
    }
    const sealed = cachedBody.tokens.slice(0, -1);
    const priorTail = cachedBody.tokens[cachedBody.tokens.length - 1];
    const volatileRows = priorTail && mini.rows.length > 0
        ? reuseUnchangedTokens([priorTail], mini.rows)
        : mini.rows;
    const lastRow = volatileSrc.slice(lastLineStartOf(volatileSrc));
    return {
        token: {
            ...cachedTable,
            raw: base,
            tokens: [
                cachedHead,
                { ...cachedBody, tokens: sealed.concat(volatileRows) }
            ]
        },
        appendInfo: {
            prefixLen: appendInfo.prefixLen,
            lastRowStart: base.length - lastRow.length,
            prefix: appendInfo.prefix,
            lastRow
        }
    };
};

/** A token compared field-by-field by `sameTokenFields`. */
type TokenRecord = Record<string, unknown> & { tokens?: unknown };

/** The shape `reuseUnchangedTokens` walks: a type tag and an optional subtree. */
type ReusableToken = { type: string; tokens?: unknown };
const isReusableTokenArray = (value: unknown): value is ReusableToken[] => Array.isArray(value);
