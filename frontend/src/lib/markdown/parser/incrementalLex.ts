/**
 * A streaming block whose shape is a list or a table defeats the
 * block-level incrementality of `parseBlocks` at the LEX layer too: the whole
 * construct is one marked block, so every appended word re-lexed every
 * item/row (block-tokenize each, then the inline pass walks them all) and
 * minted fresh token objects throughout — which also forced the Svelte
 * side to re-evaluate every subtree, because nothing kept its reference.
 * Measured: ~27ms per re-lex at a 120KB list, linear in size, on the
 * hottest path the app has (a reveal tick).
 *
 * The unit of reuse is the completed list item (or table source row).
 * Append-only growth can only touch the LAST one — extend it, or add more
 * after it — so the merge re-lexes from its offset and splices the fresh
 * tail onto the cached, reference-identical sealed tokens. A consumer
 * diffing by reference (Svelte's prop equality) then skips every sealed
 * subtree. Blockquotes are deliberately NOT given the same treatment: an
 * exact seal would have to replicate marked's per-line marker strip and
 * lazy-continuation rules (appended bytes can reinterpret earlier inner
 * content), and agent prose does not produce blockquotes at sizes where
 * the full re-lex matters. They take the fallback, correct by
 * construction, like every other unhandled shape.
 *
 * Looseness is the one list-global property: a blank line arriving anywhere
 * flips every item to paragraph-wrapped rendering. It is monotonic under
 * append-only growth (a blank line cannot be unwritten), so the merge only
 * detects the tight→loose flip and falls back to one full re-lex; a tail
 * lexed standalone under an already-loose list comes back tight and its
 * items are re-tokenized loose (loosenTailItems), mirroring finalizeList.
 * The table-global properties are the header block, alignment row, footer
 * detection, and rowspan chains — the merge verifies the first two are
 * byte-stable and bails on any sign of the last two (tableTailUnsafe).
 *
 * Reference-link definitions are the one cross-item dependency: marked
 * collects them per-Lexer (`tokens.links`, first definition wins) and
 * resolves every reflink usage against that table when the inline queue
 * drains. Two rules keep the merge exact:
 *   - The tail lexer is SEEDED with the links captured by the last full
 *     lex, so a definition inside a sealed item still resolves usages in
 *     the live tail (a fresh full parse would see the same table — no
 *     definition can have been added since without tripping the bail).
 *   - A definition DECLARED in the tail bails to a full re-lex, every
 *     tick it remains there. Seeding cannot express it: first-wins would
 *     freeze the value a still-growing definition line had at the last
 *     full lex. The bail keeps the table current instead; the window is
 *     the definition's own item, and definitions inside list items are
 *     rare (a def after a paragraph line is paragraph continuation, so
 *     only the blank-line-in-loose-item form reaches the lexer at all).
 * Footnote definitions bail the same way (see declaresDefs for why the
 * extension's mutate-the-ref mechanism cannot survive a still-streaming
 * def line), while footnote USAGE only needs the maps carried: a sealed
 * definition resolves a ref arriving in the tail through the seeded
 * maps' footnotes lookup, exactly as one full-document lexer would.
 * Cross-BLOCK definitions never resolved mid-stream in the first place
 * (each Block lexes its string in isolation upstream); these mechanisms
 * only close the divergence WITHIN the trailing block.
 *
 * Incomplete-markdown completion composes cleanly: every completer edit is
 * a suffix operation (seal an open fence at the end, drop a dangling
 * trailing line), so applying `complete` to the re-lexed slice only is not
 * an approximation — sealed content is byte-identical between the
 * completed and raw documents.
 *
 * `cache.lastPath` is a debug breadcrumb for tests
 * ('full' | 'list-append' | 'table-append') so descent coverage cannot
 * regress to silent full re-lexes.
  */
import type { Tokens } from 'marked';
import { createProvenAppend, matchesProvenAppend } from './provenAppend';
import type { ProvenAppend } from './provenAppend';
import { lexCapture } from './lexer';
import type { Extension, StreamdownToken } from './lexer';
import {
    hasTrailingRowspanCaret,
    openFenceInfo,
    scanFenceBody,
    tableAppendInfo
} from './geometry';
import type { OpenFence, TableAppendInfo } from './geometry';
import { commitLexSource, trimBlock } from './incrementalLex.cache';
import type { IncrementalLexCache, IncrementalLexPath } from './incrementalLex.cache';
import { mergeTrailingList, mergeTrailingTable, reuseUnchangedTokens } from './incrementalLex.merge';
import { parseIncompleteMarkdown as defaultIncompleteMarkdown } from './incompleteMarkdown';
import type { ListToken } from './extensions/list';
import type { TableToken } from './extensions/table';
const openFenceSourceEnd = (base: string, fence: OpenFence, complete: ((markdown: string) => string) | null): number => {
    let sourceEnd = base.length;
    if (complete === defaultIncompleteMarkdown &&
        fence.state.phase === 'run' &&
        fence.state.run > 0 &&
        fence.state.run < fence.length) {
        // The incomplete-markdown pass withholds a trailing partial closer,
        // including its preceding newline.
        sourceEnd = Math.max(0, fence.state.lineStart - 1);
    }
    return sourceEnd;
};
const renderOpenFenceRaw = (base: string, fence: OpenFence, complete: ((markdown: string) => string) | null, sourceEnd: number): string => {
    if (complete !== defaultIncompleteMarkdown)
        return base;
    const visibleSource = sourceEnd === base.length
        ? base
        : base.slice(0, sourceEnd);
    return visibleSource + '\n' + fence.char.repeat(fence.length);
};
const renderOpenFenceToken = (base: string, fence: OpenFence, complete: ((markdown: string) => string) | null): { raw: string; text: string } => {
    const sourceEnd = openFenceSourceEnd(base, fence, complete);
    const text = base.slice(fence.bodyStart, sourceEnd);
    return {
        raw: renderOpenFenceRaw(base, fence, complete, sourceEnd),
        text
    };
};
const openFenceLexRecord = (base: string, token: Tokens.Code, complete: ((markdown: string) => string) | null): OpenFence | null => {
    if (complete !== null && complete !== defaultIncompleteMarkdown)
        return null;
    const fence = openFenceInfo(base);
    if (!fence)
        return null;
    const rendered = renderOpenFenceToken(base, fence, complete);
    // This equality is the semantic guard for marked version changes,
    // custom extensions, newline normalization, and completion changes.
    // The fast path is armed only after the real lexer proves our compact
    // representation byte-identical on the current source.
    return token.raw === rendered.raw && token.text === rendered.text
        ? fence
        : null;
};
const mergeOpenFence = (
    head: Tokens.Code,
    base: string,
    appendDelta: string,
    complete: ((markdown: string) => string) | null,
    fence: OpenFence
): { token: Tokens.Code; fence: OpenFence; textAppend: ProvenAppend | undefined } | null => {
    const scan = scanFenceBody(
        appendDelta,
        0,
        fence.char,
        fence.length,
        fence.state,
        base.length - appendDelta.length
    );
    if (scan.closed)
        return null;
    const nextFence = { ...fence, state: scan.state };
    const sourceEnd = openFenceSourceEnd(base, nextFence, complete);
    const nextTextLength = Math.max(0, sourceEnd - nextFence.bodyStart);
    let text: string;
    let textAppend: ProvenAppend | undefined;
    if (nextTextLength >= head.text.length) {
        const textDelta = base.slice(nextFence.bodyStart + head.text.length, sourceEnd);
        textAppend = textDelta.length > 0
            ? createProvenAppend(head.text, textDelta)
            : undefined;
        text = textAppend?.next ?? head.text;
    }
    else {
        // A newly arrived partial closer is withheld by the incomplete-
        // markdown pass, so the rendered code text can temporarily shrink.
        // This rare transition is a replacement, not an append proof.
        text = base.slice(nextFence.bodyStart, sourceEnd);
    }
    return {
        token: {
            ...head,
            raw: renderOpenFenceRaw(base, nextFence, complete, sourceEnd),
            text
        },
        fence: nextFence,
        textAppend
    };
};
// Drop-in replacement for `lex` on streaming content: same output. An
// append-only list/table re-lexes only from the last cached item/source row;
// an open fence updates its code token from closer state. Everything else —
// first call, non-append update, other shapes, any merge surprise —
// is a full `lex`, so correctness never depends on the fast path.
// `complete` (the incomplete-markdown pass) runs inside so the fast path
// can scope it to the re-lexed slice; pass null to lex the input verbatim.
export const incrementalLex = (
    block: string,
    extensions: Extension[] | undefined = [],
    cache: IncrementalLexCache,
    complete: ((markdown: string) => string) | null = null,
    provenAppend?: ProvenAppend
): StreamdownToken[] => {
    cache.lastCodeTextAppend = undefined;
    const appendIsProven = matchesProvenAppend(provenAppend, cache.input, block);
    // Derive trim bounds from the proven suffix. String#trim flattened and
    // copied the whole active block on every revealed word.
    const trim = trimBlock(block, cache, complete, appendIsProven, provenAppend?.delta);
    const base = trim.value;
    const extKey = extensions.length > 0 ? extensions : null;
    const previousTokens = cache.extKey === extKey && cache.completeKey === complete
        ? cache.tokens
        : null;
    if (previousTokens) {
        if (base === cache.src) {
            commitLexSource(cache, block, trim);
            return previousTokens;
        }
        const appendDelta = base.length > cache.src.length
            ? appendIsProven
                ? trim.append
                : base.startsWith(cache.src)
                    ? base.slice(cache.src.length)
                    : null
            : null;
        if (previousTokens.length === 1 &&
            appendDelta !== null) {
            const head = previousTokens[0];
            if (head.type === 'code' && cache.codeFence) {
                const merged = mergeOpenFence(
                    head,
                    base,
                    appendDelta,
                    complete,
                    cache.codeFence
                );
                if (merged) {
                    commitLexSource(cache, block, trim);
                    cache.tokens = [merged.token];
                    cache.codeFence = merged.fence;
                    cache.tableAppend = null;
                    cache.lastCodeTextAppend = merged.textAppend;
                    cache.lastPath = 'code-append';
                    cache.observeLex?.('code-append', appendDelta.length);
                    return cache.tokens;
                }
            }
            let merged: ListToken | TableToken | null = null;
            let nextTableAppend: TableAppendInfo | null = null;
            let path: IncrementalLexPath = 'full';
            if (head.type === 'list') {
                merged = mergeTrailingList(head, base, extensions, complete, cache);
                path = 'list-append';
            }
            else if (head.type === 'table') {
                const tableMerge = mergeTrailingTable(
                    head,
                    base,
                    appendDelta,
                    extensions,
                    complete,
                    cache.tableTailUnstable,
                    cache.tableAppend
                );
                merged = tableMerge?.token ?? null;
                nextTableAppend = tableMerge?.appendInfo ?? null;
                path = 'table-append';
            }
            if (merged) {
                commitLexSource(cache, block, trim);
                // List/table merge functions retain every sealed subtree and
                // run equality reuse only across the bounded volatile tail.
                cache.tokens = [merged];
                cache.codeFence = null;
                cache.tableTailUnstable = false;
                cache.tableAppend = nextTableAppend;
                cache.lastPath = path;
                cache.observeLex?.(path, appendDelta.length);
                return cache.tokens;
            }
        }
    }
    commitLexSource(cache, block, trim);
    cache.extKey = extKey;
    cache.completeKey = complete;
    const completedBase = complete ? complete(base) : base;
    cache.observeLex?.('full', completedBase.length);
    const result = lexCapture(completedBase, extensions, null, null);
    cache.tokens = previousTokens
        ? reuseUnchangedTokens(previousTokens, result.tokens)
        : result.tokens;
    cache.tableTailUnstable = result.tokens.length === 1 &&
        result.tokens[0].type === 'table' &&
        hasTrailingRowspanCaret(result.tokens[0].raw);
    cache.tableAppend = result.tokens.length === 1 &&
        result.tokens[0].type === 'table' &&
        completedBase === base
        ? tableAppendInfo(result.tokens[0], base)
        : null;
    cache.links = null;
    for (const _ in result.links) {
        cache.links = result.links;
        break;
    }
    cache.footnotes = result.footnotes;
    cache.codeFence = result.tokens.length === 1 && result.tokens[0].type === 'code'
        ? openFenceLexRecord(base, result.tokens[0], complete)
        : null;
    cache.lastPath = 'full';
    return cache.tokens;
};
