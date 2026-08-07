import { Lexer } from 'marked';
// marked 16's GFM del regex treats ~text~ (single tilde) as strikethrough,
// producing false positives on approximate values like ~240MB. Require ~~.
const _fixedDel = /^(~~)(?=[^\s~])((?:\\[\s\S]|[^\\])*?(?:\\[\s\S]|[^\s~\\]))\1(?=[^~]|$)/;
Lexer.rules.inline.gfm.del = Lexer.rules.inline.breaks.del = _fixedDel;
// Disable marked's mailto autolinking. Agent prose commonly uses
// labels like `composer@0.7s`; marked sees those as email addresses,
// Streamdown rejects the resulting `mailto:` URL, then renders a
// visible `[blocked]` marker. HTTP/HTTPS/FTP and `www.` autolinks
// still go through the original rules.
const _emailUrlAlternative = '|^[A-Za-z0-9._+-]+(@)[a-zA-Z0-9-_]+(?:\\.[a-zA-Z0-9-_]*[a-zA-Z0-9])+(?![-_])';
const _emailAutolinkAlternative = "|[a-zA-Z0-9.!#$%&'*+/=?_`{|}~-]+(@)[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+(?![-_])";
const _removeRegexAlternative = (re, alternative) => new RegExp(re.source.replace(alternative, ''), re.flags);
Lexer.rules.inline.gfm.url = _removeRegexAlternative(Lexer.rules.inline.gfm.url, _emailUrlAlternative);
Lexer.rules.inline.breaks.url = _removeRegexAlternative(Lexer.rules.inline.breaks.url, _emailUrlAlternative);
Lexer.rules.inline.normal.autolink = _removeRegexAlternative(Lexer.rules.inline.normal.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.gfm.autolink = _removeRegexAlternative(Lexer.rules.inline.gfm.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.breaks.autolink = _removeRegexAlternative(Lexer.rules.inline.breaks.autolink, _emailAutolinkAlternative);
Lexer.rules.inline.pedantic.autolink = _removeRegexAlternative(Lexer.rules.inline.pedantic.autolink, _emailAutolinkAlternative);
// marked's GFM inline `text` rule begins with `[`~]+` — a COMBINED run of
// backticks AND tildes — so a `~` fused to a backtick (e.g. ~`code`) gets
// swallowed into a literal text run and the code span never opens (it can
// also mis-pair onto a later backtick, producing a stray pill mid-line).
// Split that leading class into homogeneous runs so a backtick is never
// consumed as part of a tilde run. Mirrors the _fixedDel override above.
const _fixText = (re) => new RegExp(re.source.replace('[`~]+|[^`~]', '`+|~+|[^`~]'), re.flags);
Lexer.rules.inline.gfm.text = _fixText(Lexer.rules.inline.gfm.text);
Lexer.rules.inline.breaks.text = _fixText(Lexer.rules.inline.breaks.text);
import { markedAlert } from './marked-alert.js';
import { markedFootnote } from './marked-footnotes.js';
import { markedMath } from './marked-math.js';
import { markedSub, markedSup } from './marked-subsup.js';
import { markedList, tokenizeListItemContent } from './marked-list.js';
import { markedBr } from './marked-br.js';
import { markedHr } from './marked-hr.js';
import { markedTable } from './marked-table.js';
import { markedDl } from './marked-dl.js';
import { markedAlign } from './marked-align.js';
import { markedCitations } from './marked-citations.js';
import { markedMdx } from './marked-mdx.js';
// Default plugin sets, in registration order. Hoisted so the options object
// (and the regexes/closures inside each plugin) is built once, not per chunk.
// The tokenizers are stateless at creation time — per-document state lives on
// the Lexer instance (e.g. footnotes maps, reference-link defs), so a fresh
// Lexer per call keeps documents isolated while the options are shared.
const DEFAULT_LEX_EXTENSIONS = [
    markedHr,
    markedTable,
    ...markedFootnote(),
    markedAlert,
    ...markedMath,
    markedSub,
    markedSup,
    markedList,
    markedBr,
    markedDl,
    markedAlign,
    markedCitations,
    markedMdx
];
const DEFAULT_BLOCK_EXTENSIONS = [
    markedHr,
    ...markedFootnote(),
    markedDl,
    markedTable,
    markedAlign,
    markedMdx
];
const parseExtensions = (...extensions) => {
    const options = {
        gfm: true,
        extensions: {
            block: [],
            inline: [],
            childTokens: {},
            renderers: {},
            startBlock: [],
            startInline: []
        }
    };
    extensions.forEach(({ level, name, tokenizer, ...rest }) => {
        if ('start' in rest && rest.start) {
            if (level === 'block') {
                options.extensions.startBlock.push(rest.start);
            }
            else {
                options.extensions.startInline.push(rest.start);
            }
        }
        if (tokenizer) {
            if (level === 'block') {
                options.extensions.block.push(tokenizer);
            }
            else {
                options.extensions.inline.push(tokenizer);
            }
        }
    });
    return options;
};
// Options objects are stateless and reusable across Lexer instances; cache them
// per user-extension array (props are referentially stable across chunks) so the
// hot path skips rebuilding ~20 tokenizer registrations on every streamed chunk.
const DEFAULT_LEX_OPTIONS = parseExtensions(...DEFAULT_LEX_EXTENSIONS);
const DEFAULT_BLOCK_OPTIONS = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS);
const lexOptionsCache = new WeakMap();
const blockOptionsCache = new WeakMap();
const getLexOptions = (extensions) => {
    if (extensions.length === 0)
        return DEFAULT_LEX_OPTIONS;
    let options = lexOptionsCache.get(extensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_LEX_EXTENSIONS, ...extensions);
        lexOptionsCache.set(extensions, options);
    }
    return options;
};
const getBlockOptions = (extensions) => {
    if (extensions.length === 0)
        return DEFAULT_BLOCK_OPTIONS;
    let options = blockOptionsCache.get(extensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS, ...extensions.filter(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing));
        blockOptionsCache.set(extensions, options);
    }
    return options;
};
export const lex = (markdown, extensions = []) => {
    return new Lexer(getLexOptions(extensions))
        .lex(markdown)
        .filter((token) => token.type !== 'space' && token.type !== 'footnote');
};
export const createParseBlocksCache = () => ({
    content: '',
    raws: [],
    keep: [],
    trailingBlock: null
});
// Number of trailing rendered blocks that stay "live" (re-lexed every chunk).
// 2 covers constructs that merge backward as they stream in — e.g. a paragraph
// line becoming a table once its delimiter row arrives, or a setext heading.
const SEAL_SLACK = 2;
const blockTokensOf = (markdown, extensions) => new Lexer(getBlockOptions(extensions)).blockTokens(markdown, []);
const isKeptType = (type) => type !== 'space' && type !== 'footnote';
// A list token's items: `.tokens` from this package's markedList (the lex
// layer — getLexOptions registers it), `.items` from marked's built-in list
// tokenizer (the block layer — getBlockOptions deliberately does not).
const itemsOf = (list) => {
    if (Array.isArray(list.tokens) && list.tokens.length > 0)
        return list.tokens;
    if (Array.isArray(list.items) && list.items.length > 0)
        return list.items;
    return null;
};
// Byte length of every item except the last — the sealed region of a
// trailing list. Append-only growth can only touch the last item (extend
// it or add items after it): earlier items' bullet lines cannot be unmade
// by later bytes, so their raws are immutable offsets into the document.
const sealedLengthOf = (list) => {
    const items = itemsOf(list);
    if (!items)
        return 0;
    let len = 0;
    for (let i = 0; i < items.length - 1; i++)
        len += items[i].raw.length;
    return len;
};
// --- Trailing-table append geometry -----------------------------------------
// The table analog of sealedLengthOf: a table's rows are line-scoped, and
// without rowspan a row's parse depends only on the header's alignment and
// column count — so under append-only growth every body line before the
// LAST line of the source is sealed. Returns the byte geometry needed to
// re-lex just the volatile region (header+align prefix replayed for
// context, then the last line onward), or null when the shape is not
// append-safe: the fast path only handles the plain [thead, tbody] form
// with at least one sealable body row. Header-only tables, tables without
// an alignment row (their structure reinterprets when one arrives), and
// tables with a tfoot (footer detection re-homes the previous tfoot row on
// every append) all fall back to full re-lexes.
const lastLineStartOf = (source) => {
    let end = source.length;
    while (end > 0 && source[end - 1] === '\n')
        end--;
    return source.lastIndexOf('\n', end - 1) + 1;
};
const tableAppendInfo = (table, source) => {
    if (table.type !== 'table' || !Array.isArray(table.tokens) || table.tokens.length !== 2)
        return null;
    const [thead, tbody] = table.tokens;
    if (thead.type !== 'thead' || tbody.type !== 'tbody')
        return null;
    if (!Array.isArray(tbody.tokens) || tbody.tokens.length < 2)
        return null;
    // Header lines map 1:1 onto thead rows (the tokenizer filters empty
    // lines, but the table grammar admits none), then the alignment row.
    const prefixLines = thead.tokens.length + 1;
    let prefixLen = 0;
    for (let i = 0; i < prefixLines; i++) {
        const nl = source.indexOf('\n', prefixLen);
        if (nl === -1)
            return null;
        prefixLen = nl + 1;
    }
    // Body rows map 1:1 onto the remaining non-empty lines; the raw may
    // carry trailing newlines the tokenizer consumed past the last row.
    const lastRowStart = lastLineStartOf(source);
    if (lastRowStart <= prefixLen)
        return null;
    return { prefixLen, lastRowStart };
};
// Footer-marker rows (an alignment-shaped line in the body) re-home the
// last body row into a tfoot; a `^` closing a cell can be a rowspan
// indicator, which MUTATES the previous row's cells during the parse.
// Both break the "sealed rows are settled" premise, so any volatile slice
// containing either takes the full re-lex. The caret test matches the
// cell-final shape (caret, optional spaces, then a pipe/newline/end) —
// mid-cell carets like a footnote ref's `[^t]` stay on the fast path,
// while superscript closers and code-span carets in that position only
// cost a needless fallback, never correctness.
const FOOTER_MARKER_LINE = /^ *\| *:?-+:? *(\| *:?-+:? *)* *\| *$/m;
const ROWSPAN_CARET = /\^[ \t]*(\||\n|$)/;
const tableTailUnsafe = (slice) => ROWSPAN_CARET.test(slice) || FOOTER_MARKER_LINE.test(slice);
// A rowspan mutation is retroactive AND revocable while its row streams:
// a cell that momentarily ends with `^` (e.g. a half-arrived footnote ref
// `[^t]`) rowspan-mutates the PREVIOUS row during that tick's full lex,
// and the next characters un-happen it in a fresh parse — but the
// mutation is already baked into the cached sealed rows. Sealed rows are
// therefore only trustworthy when the cached document's tail carries no
// such trigger; a complete `^` row (newline landed) is permanent and
// safe. Matches the trailing cell shape: caret, optional spaces, an
// optional closing pipe, end of document.
const TRAILING_CARET = /\^[ \t]*(\|[ \t]*)?$/;
// Maintain the trailing-block descent record after a parse pass. `tokens`
// covers the document from `tokensOffset` (byte) / `rawIndex` (raws slot)
// to the end. The record points at the last KEPT block iff it is a list or
// table with a sealable prefix; anything else clears it. `sealedBias`
// folds an already-sealed prefix into token 0's accounting (the list
// descent path lexes only the list's tail, so its token 0 is a suffix of
// the real block; the table descent updates its record directly instead,
// because its slice replays the header and is no suffix of anything).
const updateTrailingBlockRecord = (cache, tokens, tokensOffset, rawIndex, sealedBias = 0) => {
    for (let i = tokens.length - 1; i >= 0; i--) {
        const token = tokens[i];
        if (!isKeptType(token.type))
            continue;
        const bias = i === 0 ? sealedBias : 0;
        let blockStart = tokensOffset;
        for (let j = 0; j < i; j++)
            blockStart += tokens[j].raw.length;
        if (token.type === 'list' &&
            itemsOf(token) !== null &&
            sealedLengthOf(token) + bias > 0) {
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
        }
        cache.trailingBlock = null;
        return;
    }
    cache.trailingBlock = null;
};
export const parseBlocks = (markdown, extensions = [], cache) => {
    if (cache &&
        cache.content.length > 0 &&
        markdown.length > cache.content.length &&
        markdown.startsWith(cache.content)) {
        // Trailing-block descent: when the last rendered block is a list or a
        // table, the block-level tail re-lex below still costs the WHOLE
        // block on every chunk (one list/table = one block token). The
        // record lets the append start inside the block instead — at the
        // list's last item, or at the table's last row (with the header
        // replayed for alignment context) — so a growing list or table costs
        // O(new content) here like every other block shape. Any mismatch
        // falls through to the standard append path.
        const t = cache.trailingBlock;
        if (t && t.rawIndex < cache.raws.length) {
            let blockStart = 0;
            for (let i = 0; i < t.rawIndex; i++)
                blockStart += cache.raws[i].length;
            if (t.kind === 'list') {
                const sliceStart = t.blockStart + t.sealedLen;
                if (blockStart === t.blockStart && sliceStart < markdown.length) {
                    const sliceTokens = blockTokensOf(markdown.slice(sliceStart), extensions);
                    const first = sliceTokens[0];
                    let sliceLength = 0;
                    for (const token of sliceTokens)
                        sliceLength += token.raw.length;
                    // Same contiguity guard as the standard path, plus: the slice
                    // must still open with a list (the sealed items' continuation).
                    if (first && first.type === 'list' && sliceStart + sliceLength === markdown.length) {
                        cache.raws.length = t.rawIndex;
                        cache.keep.length = t.rawIndex;
                        // Sealed source bytes + the tail token's OWN raw — not one
                        // source slice across the span: blockTokens normalizes a
                        // single trailing whitespace char into the token raw as
                        // "\n", and raws must carry what the lexer produced, byte
                        // for byte, or they diverge from a fresh parse. Lengths
                        // are 1:1, so every offset in the record stays valid.
                        cache.raws.push(markdown.slice(t.blockStart, sliceStart) + first.raw);
                        cache.keep.push(true);
                        for (let i = 1; i < sliceTokens.length; i++) {
                            cache.raws.push(sliceTokens[i].raw);
                            cache.keep.push(isKeptType(sliceTokens[i].type));
                        }
                        cache.content = markdown;
                        updateTrailingBlockRecord(cache, sliceTokens, sliceStart, t.rawIndex, t.sealedLen);
                        return cache.raws.filter((_, i) => cache.keep[i]);
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
                const prefix = markdown.slice(t.blockStart, t.blockStart + t.prefixLen);
                const volatileSrc = markdown.slice(t.lastRowStart);
                const miniTokens = blockTokensOf(prefix + volatileSrc, extensions);
                const mini = miniTokens[0];
                if (miniTokens.length === 1 &&
                    mini.type === 'table' &&
                    mini.raw.length === t.prefixLen + volatileSrc.length) {
                    cache.raws.length = t.rawIndex;
                    cache.keep.length = t.rawIndex;
                    // Sealed source bytes + the mini token's own tail raw, for
                    // the same byte-fidelity reason as the list branch.
                    const raw = markdown.slice(t.blockStart, t.lastRowStart) + mini.raw.slice(t.prefixLen);
                    cache.raws.push(raw);
                    cache.keep.push(true);
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
                    return cache.raws.filter((_, i) => cache.keep[i]);
                }
            }
        }
        // Append-only update: seal everything except the last SEAL_SLACK rendered
        // blocks and re-lex only the tail. cache.raws concatenates exactly to
        // cache.content (verified by length below), so summed lengths are offsets.
        let cut = cache.raws.length;
        let liveBlocks = 0;
        while (cut > 0 && liveBlocks < SEAL_SLACK) {
            cut--;
            if (cache.keep[cut])
                liveBlocks++;
        }
        let offset = 0;
        for (let i = 0; i < cut; i++)
            offset += cache.raws[i].length;
        const tailTokens = blockTokensOf(markdown.slice(offset), extensions);
        let tailLength = 0;
        for (const token of tailTokens)
            tailLength += token.raw.length;
        // Contiguity guard: if the lexer normalized the tail (so raws no longer
        // reconstruct the input), the offsets cannot be trusted — full reparse.
        if (offset + tailLength === markdown.length) {
            cache.raws.length = cut;
            cache.keep.length = cut;
            for (const token of tailTokens) {
                cache.raws.push(token.raw);
                cache.keep.push(isKeptType(token.type));
            }
            cache.content = markdown;
            updateTrailingBlockRecord(cache, tailTokens, offset, cut);
            return cache.raws.filter((_, i) => cache.keep[i]);
        }
    }
    // Full parse (first call, non-append update, or contiguity fallback).
    const tokens = blockTokensOf(markdown, extensions);
    const blocks = [];
    if (cache) {
        cache.raws = [];
        cache.keep = [];
        let total = 0;
        for (const token of tokens) {
            const keep = isKeptType(token.type);
            cache.raws.push(token.raw);
            cache.keep.push(keep);
            total += token.raw.length;
            if (keep)
                blocks.push(token.raw);
        }
        // Only trust the cache for future appends if raws reconstruct the input.
        cache.content = total === markdown.length ? markdown : '';
        if (cache.content.length > 0) {
            updateTrailingBlockRecord(cache, tokens, 0, 0);
        }
        else {
            cache.trailingBlock = null;
        }
        return blocks;
    }
    for (const token of tokens) {
        if (isKeptType(token.type))
            blocks.push(token.raw);
    }
    return blocks;
};
// --- Incremental block lexing ----------------------------------------------
// A streaming block whose shape is a list or a table defeats the
// block-level incrementality above at the LEX layer too: the whole
// construct is one marked block, so every appended word re-lexed every
// item/row (block-tokenize each, then the inline pass walks them all) and
// minted fresh token objects throughout — which also forced the Svelte
// side to re-evaluate every subtree, because nothing kept its reference.
// Measured: ~27ms per re-lex at a 120KB list, linear in size, on the
// hottest path the app has (a reveal tick).
//
// The unit of reuse is the completed list item (or table source row).
// Append-only growth can only touch the LAST one — extend it, or add more
// after it — so the merge re-lexes from its offset and splices the fresh
// tail onto the cached, reference-identical sealed tokens. A consumer
// diffing by reference (Svelte's prop equality) then skips every sealed
// subtree. Blockquotes are deliberately NOT given the same treatment: an
// exact seal would have to replicate marked's per-line marker strip and
// lazy-continuation rules (appended bytes can reinterpret earlier inner
// content), and agent prose does not produce blockquotes at sizes where
// the full re-lex matters. They take the fallback, correct by
// construction, like every other unhandled shape.
//
// Looseness is the one list-global property: a blank line arriving anywhere
// flips every item to paragraph-wrapped rendering. It is monotonic under
// append-only growth (a blank line cannot be unwritten), so the merge only
// detects the tight→loose flip and falls back to one full re-lex; a tail
// lexed standalone under an already-loose list comes back tight and its
// items are re-tokenized loose (loosenTailItems), mirroring finalizeList.
// The table-global properties are the header block, alignment row, footer
// detection, and rowspan chains — the merge verifies the first two are
// byte-stable and bails on any sign of the last two (tableTailUnsafe).
//
// Reference-link definitions are the one cross-item dependency: marked
// collects them per-Lexer (`tokens.links`, first definition wins) and
// resolves every reflink usage against that table when the inline queue
// drains. Two rules keep the merge exact:
//   - The tail lexer is SEEDED with the links captured by the last full
//     lex, so a definition inside a sealed item still resolves usages in
//     the live tail (a fresh full parse would see the same table — no
//     definition can have been added since without tripping the bail).
//   - A definition DECLARED in the tail bails to a full re-lex, every
//     tick it remains there. Seeding cannot express it: first-wins would
//     freeze the value a still-growing definition line had at the last
//     full lex. The bail keeps the table current instead; the window is
//     the definition's own item, and definitions inside list items are
//     rare (a def after a paragraph line is paragraph continuation, so
//     only the blank-line-in-loose-item form reaches the lexer at all).
// Footnote definitions bail the same way (see declaresDefs for why the
// extension's mutate-the-ref mechanism cannot survive a still-streaming
// def line), while footnote USAGE only needs the maps carried: a sealed
// definition resolves a ref arriving in the tail through the seeded
// maps' footnotes lookup, exactly as one full-document lexer would.
// Cross-BLOCK definitions never resolved mid-stream in the first place
// (each Block lexes its string in isolation upstream); these mechanisms
// only close the divergence WITHIN the trailing block.
//
// Incomplete-markdown completion composes cleanly: every completer edit is
// a suffix operation (seal an open fence at the end, drop a dangling
// trailing line), so applying `complete` to the re-lexed slice only is not
// an approximation — sealed content is byte-identical between the
// completed and raw documents.
//
// `cache.lastPath` is a debug breadcrumb for tests
// ('full' | 'list-append' | 'table-append') so descent coverage cannot
// regress to silent full re-lexes.
export const createIncrementalLexCache = () => ({
    src: '',
    extKey: null,
    tokens: null,
    links: null,
    footnotes: null,
    lastPath: 'none'
});
// lex(), but the Lexer is observable: seeded with a prior links table /
// footnote maps before the run, captured after. The public lex() stays a
// pure function; the incremental paths need the per-document state to
// carry sealed-item context into a tail-only re-lex.
const lexCapture = (markdown, extensions, seedLinks, seedFootnotes) => {
    const lexer = new Lexer(getLexOptions(extensions));
    if (seedLinks)
        Object.assign(lexer.tokens.links, seedLinks);
    if (seedFootnotes) {
        lexer.footnotes = seedFootnotes;
        lexer.hasFootnotes = true;
    }
    const tokens = lexer.lex(markdown).filter((token) => isKeptType(token.type));
    return {
        tokens,
        links: lexer.tokens.links,
        // In component context the footnote extension uses the shared
        // Streamdown maps and never touches the lexer — null here keeps
        // the cache out of the way and the context authoritative.
        footnotes: lexer.hasFootnotes ? lexer.footnotes : null
    };
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
const declaresDefs = (src, extensions) => {
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
const loosenTailItems = (items, extensions, cache) => {
    const lexer = new Lexer(getLexOptions(extensions));
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
    for (let i = 0; i < lexer.inlineQueue.length; i++) {
        const next = lexer.inlineQueue[i];
        lexer.inlineTokens(next.src, next.tokens);
    }
};
const mergeTrailingList = (cachedList, base, extensions, complete, cache) => {
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
const mergeTrailingTable = (cachedTable, base, extensions, complete) => {
    if (TRAILING_CARET.test(cachedTable.raw))
        return null;
    const info = tableAppendInfo(cachedTable, cachedTable.raw);
    if (!info || info.lastRowStart >= base.length)
        return null;
    // Rowspan (`^` mutates the PREVIOUS row's cells) and footer markers
    // (re-home the last row into a tfoot) break sealed-row immutability.
    if (tableTailUnsafe(base.slice(info.lastRowStart)))
        return null;
    const miniSrc = base.slice(0, info.prefixLen) + base.slice(info.lastRowStart);
    const completedMini = complete ? complete(miniSrc) : miniSrc;
    const miniTokens = lex(completedMini, extensions);
    if (miniTokens.length !== 1)
        return null;
    const mini = miniTokens[0];
    if (mini.type !== 'table' ||
        mini.raw !== completedMini ||
        !Array.isArray(mini.tokens) ||
        mini.tokens.length !== 2 ||
        mini.tokens[0].type !== 'thead' ||
        mini.tokens[1].type !== 'tbody' ||
        mini.tokens[0].tokens.length !== cachedTable.tokens[0].tokens.length ||
        JSON.stringify(mini.align) !== JSON.stringify(cachedTable.align))
        return null;
    const sealed = cachedTable.tokens[1].tokens.slice(0, -1);
    return {
        ...cachedTable,
        raw: base.slice(0, info.lastRowStart) + mini.raw.slice(info.prefixLen),
        tokens: [
            cachedTable.tokens[0],
            { ...cachedTable.tokens[1], tokens: sealed.concat(mini.tokens[1].tokens) }
        ]
    };
};
// Drop-in replacement for `lex` on streaming content: same output, but an
// append-only update to a document whose single block is a list or a
// table re-lexes only from the last cached item / source row. Everything
// else — first call, non-append update, other shapes, any merge surprise —
// is a full `lex`, so correctness never depends on the fast path.
// `complete` (the incomplete-markdown pass) runs inside so the fast path
// can scope it to the re-lexed slice; pass null to lex the input verbatim.
export const incrementalLex = (block, extensions = [], cache, complete = null) => {
    const base = complete ? block.trim() : block;
    const extKey = extensions.length > 0 ? extensions : null;
    if (cache.tokens && cache.extKey === extKey) {
        if (base === cache.src)
            return cache.tokens;
        if (cache.tokens.length === 1 &&
            base.length > cache.src.length &&
            base.startsWith(cache.src)) {
            const head = cache.tokens[0];
            let merged = null;
            let path = 'full';
            if (head.type === 'list') {
                merged = mergeTrailingList(head, base, extensions, complete, cache);
                path = 'list-append';
            }
            else if (head.type === 'table') {
                merged = mergeTrailingTable(head, base, extensions, complete);
                path = 'table-append';
            }
            if (merged) {
                cache.src = base;
                cache.tokens = [merged];
                cache.lastPath = path;
                return cache.tokens;
            }
        }
    }
    cache.src = base;
    cache.extKey = extKey;
    const result = lexCapture(complete ? complete(base) : base, extensions, null, null);
    cache.tokens = result.tokens;
    cache.links = null;
    for (const _ in result.links) {
        cache.links = result.links;
        break;
    }
    cache.footnotes = result.footnotes;
    cache.lastPath = 'full';
    return cache.tokens;
};
