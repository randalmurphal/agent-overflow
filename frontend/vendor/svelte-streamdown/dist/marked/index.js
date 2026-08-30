import { Lexer } from 'marked';
import { parseIncompleteMarkdown as defaultIncompleteMarkdown } from '../utils/parse-incomplete-markdown.js';
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
import { markedFootnote, markedFootnoteBlock } from './marked-footnotes.js';
import { markedMath } from './marked-math.js';
import { markedSub, markedSup } from './marked-subsup.js';
import { markedList, markedListBlock, parseListSource, tokenizeListItemContent } from './marked-list.js';
import { markedBr } from './marked-br.js';
import { markedHr } from './marked-hr.js';
import { markedTable, tokenizeTableTail } from './marked-table.js';
import { markedTableBlock, parseTableBlockSource } from './marked-table-source.js';
import { markedDl, markedDlBlock } from './marked-dl.js';
import { markedAlign, markedAlignBlock } from './marked-align.js';
import { markedCitations } from './marked-citations.js';
import { markedMdx, markedMdxBlock } from './marked-mdx.js';
import { markedBlockquoteBlock, parseBlockquoteSource } from './marked-blockquote-source.js';
const provenAppends = new WeakSet();
const mintProvenAppend = (previous, delta, next) => {
    const proof = Object.freeze({ previous, delta, next });
    provenAppends.add(proof);
    return proof;
};
export const createProvenAppend = (previous, delta) => {
    return mintProvenAppend(previous, delta, previous + delta);
};
// V8 represents a non-trivial String#slice as a SlicedString that retains its
// complete parent. Joining two non-empty halves copies the code units into an
// independent sequential string. Keep this at the parser boundary: cached
// block raws live for the whole mounted message and must not pin the full
// document buffer from the checkpoint where marked produced each token.
const materializeString = (value) => {
    if (value.length < 2)
        return value;
    const middle = value.length >>> 1;
    return [value.slice(0, middle), value.slice(middle)].join('');
};
export const createMaterializedProvenAppend = (previous, deltas) => {
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
export const matchesProvenAppend = (proof, previous, next) => proof !== undefined &&
    provenAppends.has(proof) &&
    proof.delta.length > 0 &&
    proof.previous === previous &&
    proof.next === next;
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
    markedFootnoteBlock,
    markedBlockquoteBlock,
    markedListBlock,
    markedDlBlock,
    markedTableBlock,
    markedAlignBlock,
    markedMdxBlock
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
const blockExtensionsCache = new WeakMap();
const blockExtensionsOf = (extensions) => {
    if (extensions.length === 0)
        return null;
    if (blockExtensionsCache.has(extensions))
        return blockExtensionsCache.get(extensions);
    let blockExtensions = null;
    for (const extension of extensions) {
        if (extension.level !== 'block' || !extension.applyInBlockParsing)
            continue;
        blockExtensions ??= [];
        blockExtensions.push(extension);
    }
    const result = blockExtensions?.length === extensions.length
        ? extensions
        : blockExtensions;
    blockExtensionsCache.set(extensions, result);
    return result;
};
const sameExtensionSequence = (left, right) => {
    if (left === right)
        return true;
    if (!left || !right || left.length !== right.length)
        return false;
    for (let index = 0; index < left.length; index++) {
        if (left[index] !== right[index])
            return false;
    }
    return true;
};
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
    const blockExtensions = blockExtensionsOf(extensions);
    if (!blockExtensions)
        return DEFAULT_BLOCK_OPTIONS;
    let options = blockOptionsCache.get(blockExtensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS, ...blockExtensions);
        blockOptionsCache.set(blockExtensions, options);
    }
    return options;
};
export const lex = (markdown, extensions = []) => {
    return new Lexer(getLexOptions(extensions))
        .lex(markdown)
        .filter((token) => token.type !== 'space' && token.type !== 'footnote');
};
// Divergence 29: `[^label]` → its `[^label]: body` definition, for a whole
// document. The render path cannot answer this. A definition is always its
// own block, parseBlocks splits blocks apart, and each Block lexes its
// string in its own Lexer (see incrementalLex's cross-BLOCK note), so the
// tokenizer's `ref.content` back-reference is the empty placeholder for
// every ref whose definition lives in another block — which is every real
// document, since definitions conventionally sit at the end. The definition
// tokens are also filtered out of every render list, so the body reaches no
// DOM either. One Lexer over the whole source answers it with the real
// grammar (fenced code, list nesting and blockquotes included) instead of a
// second `[^x]:` regex somewhere in the app. Returns null when the document
// declares no footnotes at all. Callers own the memo: this is a full lex,
// paid lazily on the gesture that needs it, never during render.
export const lexFootnoteDefinitions = (markdown, extensions = []) => {
    const lexer = new Lexer(getLexOptions(extensions));
    lexer.lex(markdown);
    return lexer.hasFootnotes ? lexer.footnotes.footnotes : null;
};
export const createParseBlocksCache = (observeLex) => ({
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
const truncateParseBlocksCache = (cache, rawLength) => {
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
const appendParseBlockRaw = (cache, raw, keep, start) => {
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
const replaceParseBlockRaw = (cache, rawIndex, raw) => {
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
export const updateParseBlockStringMaterialization = (cache, enabled) => {
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
const trailingBlockMayMergeBackward = (cache) => {
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
// A one-line paragraph whose first word cannot still become this package's
// alphabetic/Roman list marker is block-stable until a newline arrives. Inline
// punctuation may still change its token tree, but that belongs to
// incrementalLex; parseBlocks only owns the outer raw boundary. Requiring a
// multi-letter non-Roman word is deliberately conservative around partial
// list, HTML, MDX, math, definition, fence, and description-list openers.
const STABLE_PARAGRAPH_WORD = /^ {0,3}([\p{L}]+)/u;
const ROMAN_MARKER_PREFIX = /^[ivxlcdm]+$/i;
const paragraphAppendSafe = (raw) => {
    if (/[\r\n]/.test(raw))
        return false;
    const word = STABLE_PARAGRAPH_WORD.exec(raw)?.[1];
    return word !== undefined && word.length > 1 && !ROMAN_MARKER_PREFIX.test(word);
};
const INITIAL_HEADING = /^ {0,3}#{1,6}(?:[ \t]+|$)/;
const initialBlockToken = (markdown, extensions) => {
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
const blockTokensOf = (markdown, extensions, cache, path) => {
    cache?.observeLex?.(path, markdown.length, markdown);
    return new Lexer(getBlockOptions(extensions)).blockTokens(markdown, []);
};
const isKeptType = (type) => type !== 'space' && type !== 'footnote';
// --- Open-fence append geometry --------------------------------------------
// Marked treats an unclosed fenced block as one token through EOF. Re-lexing
// that token for every streamed word makes a long code answer O(n²), even
// though append-only bytes inside an open fence cannot alter any earlier block
// boundary. Track only the current source line's closer-candidate state. A
// valid closer causes an immediate fallback to the normal lexer, where the
// code block may close and following markdown may open new blocks.
const newFenceLineState = (lineStart) => ({
    phase: 'leading',
    indent: 0,
    run: 0,
    lineStart
});
const fenceLineCloses = (state, fenceLength) => (state.phase === 'run' || state.phase === 'trailing') && state.run >= fenceLength;
const scanFenceBody = (source, start, fenceChar, fenceLength, initialState, sourceOffset = 0) => {
    const state = initialState
        ? { ...initialState }
        : newFenceLineState(start);
    for (let i = start; i < source.length; i++) {
        const ch = source[i];
        if (ch === '\n') {
            if (fenceLineCloses(state, fenceLength))
                return { closed: true, state };
            Object.assign(state, newFenceLineState(sourceOffset + i + 1));
            continue;
        }
        if (state.phase === 'invalid')
            continue;
        if (state.phase === 'leading') {
            if (ch === ' ' && state.indent < 3) {
                state.indent++;
                continue;
            }
            if (ch === fenceChar) {
                state.phase = 'run';
                state.run = 1;
                continue;
            }
            state.phase = 'invalid';
            continue;
        }
        if (state.phase === 'run') {
            if (ch === fenceChar) {
                state.run++;
                continue;
            }
            if ((ch === ' ' || ch === '\t') && state.run >= fenceLength) {
                state.phase = 'trailing';
                continue;
            }
            state.phase = 'invalid';
            continue;
        }
        if (state.phase === 'trailing' && ch !== ' ' && ch !== '\t')
            state.phase = 'invalid';
    }
    return { closed: fenceLineCloses(state, fenceLength), state };
};
const openFenceInfo = (source) => {
    // This is marked 16's opener grammar. Keep the backtick lookahead: an
    // info string containing a backtick is not a fence opener.
    const opener = /^ {0,3}(`{3,}(?=[^`\n]*(?:\n|$))|~{3,})([^\n]*)(\n|$)/.exec(source);
    if (!opener || opener[3] !== '\n')
        return null;
    const fence = opener[1];
    const bodyStart = opener[0].length;
    const scan = scanFenceBody(source, bodyStart, fence[0], fence.length);
    if (scan.closed)
        return null;
    return {
        char: fence[0],
        length: fence.length,
        bodyStart,
        state: scan.state
    };
};
const appendDeltaOf = (markdown, cache, provenAppend) => {
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
    if (Number.isInteger(list.sealedLen))
        return list.sealedLen;
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
const tablePrefixLength = (table, source) => {
    if (table.type !== 'table')
        return null;
    const thead = Array.isArray(table.tokens) ? table.tokens[0] : undefined;
    const headerRowCount = Number.isInteger(table.headerRowCount)
        ? table.headerRowCount
        : thead?.type === 'thead' && Array.isArray(thead.tokens)
            ? thead.tokens.length
            : 0;
    if (headerRowCount === 0)
        return null;
    // Header lines map 1:1 onto thead rows (the tokenizer filters empty
    // lines, but the table grammar admits none), then the alignment row.
    const prefixLines = headerRowCount + 1;
    let prefixLen = 0;
    for (let i = 0; i < prefixLines; i++) {
        const nl = source.indexOf('\n', prefixLen);
        if (nl === -1)
            return null;
        prefixLen = nl + 1;
    }
    return prefixLen;
};
const tableAppendInfo = (table, source) => {
    if (table.hasFooter === true ||
        (table.hasFooter === undefined &&
            Array.isArray(table.tokens) &&
            table.tokens.some((section) => section.type === 'tfoot')))
        return null;
    const tbody = Array.isArray(table.tokens) ? table.tokens[1] : undefined;
    const bodyRowCount = Number.isInteger(table.bodyRowCount)
        ? table.bodyRowCount
        : tbody?.type === 'tbody' && Array.isArray(tbody.tokens)
            ? tbody.tokens.length
            : 0;
    if (bodyRowCount < 2)
        return null;
    const prefixLen = tablePrefixLength(table, source);
    if (prefixLen === null)
        return null;
    // Body rows map 1:1 onto the remaining non-empty lines; the raw may
    // carry trailing newlines the tokenizer consumed past the last row.
    const lastRowStart = lastLineStartOf(source);
    if (lastRowStart <= prefixLen)
        return null;
    return {
        prefixLen,
        lastRowStart,
        prefix: source.slice(0, prefixLen),
        lastRow: source.slice(lastRowStart)
    };
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
const hasTrailingRowspanCaret = (source) => {
    let index = source.length - 1;
    while (index >= 0 && (source.charCodeAt(index) === 32 || source.charCodeAt(index) === 9))
        index--;
    if (source.charCodeAt(index) === 124) {
        index--;
        while (index >= 0 && (source.charCodeAt(index) === 32 || source.charCodeAt(index) === 9))
            index--;
    }
    return source.charCodeAt(index) === 94;
};
const isTrimWhitespaceCode = (code) => (code >= 9 && code <= 13) ||
    code === 32 ||
    code === 160 ||
    code === 5760 ||
    (code >= 8192 && code <= 8202) ||
    code === 8232 ||
    code === 8233 ||
    code === 8239 ||
    code === 8287 ||
    code === 12288 ||
    code === 65279;
const trimBlock = (block, cache, complete, appendIsProven, appendDelta) => {
    if (!complete)
        return { value: block, leading: 0, trailing: '', append: appendIsProven ? appendDelta : null };
    if (appendIsProven && cache.completeKey === complete && appendDelta) {
        let deltaEnd = appendDelta.length;
        while (deltaEnd > 0 && isTrimWhitespaceCode(appendDelta.charCodeAt(deltaEnd - 1)))
            deltaEnd--;
        if (cache.src.length === 0) {
            let deltaStart = 0;
            while (deltaStart < deltaEnd && isTrimWhitespaceCode(appendDelta.charCodeAt(deltaStart)))
                deltaStart++;
            if (deltaStart === deltaEnd)
                return { value: '', leading: block.length, trailing: '', append: '' };
            const leading = cache.input.length + deltaStart;
            const trailing = appendDelta.slice(deltaEnd);
            const append = appendDelta.slice(deltaStart, deltaEnd);
            return {
                value: leading === 0 && trailing.length === 0
                    ? block
                    : block.slice(leading, block.length - trailing.length),
                leading,
                trailing,
                append
            };
        }
        const leading = cache.leadingTrim;
        if (deltaEnd === 0) {
            return {
                value: cache.src,
                leading,
                trailing: cache.trailingTrim + appendDelta,
                append: ''
            };
        }
        const trailing = appendDelta.slice(deltaEnd);
        const append = cache.trailingTrim + appendDelta.slice(0, deltaEnd);
        return {
            value: leading === 0 && trailing.length === 0
                ? block
                : block.slice(leading, block.length - trailing.length),
            leading,
            trailing,
            append
        };
    }
    let leading = 0;
    while (leading < block.length && isTrimWhitespaceCode(block.charCodeAt(leading)))
        leading++;
    let end = block.length;
    while (end > leading && isTrimWhitespaceCode(block.charCodeAt(end - 1)))
        end--;
    const trailing = block.slice(end);
    return {
        value: leading === 0 && trailing.length === 0 ? block : block.slice(leading, end),
        leading,
        trailing,
        append: null
    };
};
const commitLexSource = (cache, block, trim) => {
    cache.src = trim.value;
    cache.input = block;
    cache.leadingTrim = trim.leading;
    cache.trailingTrim = trim.trailing;
};
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
            const items = itemsOf(token);
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
export const parseBlocks = (markdown, extensions = [], cache, provenAppend) => {
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
        ? appendDeltaOf(markdown, cache, provenAppend)
        : null;
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
                (blockStart === 0 || markdown[blockStart - 1] === '\n' || markdown[blockStart - 1] === '\r') &&
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
                (blockStart === 0 || markdown[blockStart - 1] === '\n' || markdown[blockStart - 1] === '\r') &&
                !/[\r\n]/.test(appendDelta) &&
                !extensions.some(({ level, applyInBlockParsing }) => level === 'block' && applyInBlockParsing)) {
                let trailingWhitespace = 0;
                for (let i = appendDelta.length - 1; i >= 0; i--) {
                    if (appendDelta[i] !== ' ' && appendDelta[i] !== '\t')
                        break;
                    trailingWhitespace++;
                }
                const priorSourceWhitespace = t.normalized
                    ? cache.content[blockStart + priorRaw.length - 1]
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
                    const sliceTokens = blockTokensOf(markdown.slice(sliceStart), extensions, cache, 'list-descent');
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
                            markdown.slice(t.blockStart, sliceStart) + first.raw,
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
                const prefix = markdown.slice(t.blockStart, t.blockStart + t.prefixLen);
                const volatileSrc = markdown.slice(t.lastRowStart);
                const miniTokens = blockTokensOf(prefix + volatileSrc, extensions, cache, 'table-descent');
                const mini = miniTokens[0];
                if (miniTokens.length === 1 &&
                    mini.type === 'table' &&
                    mini.raw.length === t.prefixLen + volatileSrc.length) {
                    truncateParseBlocksCache(cache, t.rawIndex);
                    // Sealed source bytes + the mini token's own tail raw, for
                    // the same byte-fidelity reason as the list branch.
                    const raw = markdown.slice(t.blockStart, t.lastRowStart) + mini.raw.slice(t.prefixLen);
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
        const tailTokens = blockTokensOf(markdown.slice(offset), extensions, cache, 'append-tail');
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
    const blocks = [];
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
export const createIncrementalLexCache = (observeLex) => ({
    src: '',
    input: '',
    extKey: null,
    completeKey: null,
    tokens: null,
    links: null,
    footnotes: null,
    codeFence: null,
    tableTailUnstable: false,
    tableAppend: null,
    leadingTrim: 0,
    trailingTrim: '',
    lastCodeTextAppend: undefined,
    lastPath: 'none',
    observeLex
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

// Marked recreates every token object on a fallback lex. Svelte then treats
// those fresh objects as changed each-items and re-runs the nested branch tree,
// even when all but the trailing inline token are byte-identical. Reuse only
// position-matched tokens whose complete observable shape is equal. Unknown
// extension fields are compared too. Any object-valued field other than
// `tokens` keeps the new token rather than assuming an extension contract.
const sameTokenFields = (previous, next) => {
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
const reuseUnchangedTokens = (previous, next) => {
    let allReused = previous.length === next.length;
    for (let index = 0; index < next.length; index++) {
        const priorToken = previous[index];
        const nextToken = next[index];
        if (!priorToken || !nextToken || priorToken.type !== nextToken.type) {
            allReused = false;
            continue;
        }
        if (Array.isArray(priorToken.tokens) && Array.isArray(nextToken.tokens)) {
            nextToken.tokens = reuseUnchangedTokens(priorToken.tokens, nextToken.tokens);
        }
        if (sameTokenFields(priorToken, nextToken)) {
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
const mergeTrailingTable = (cachedTable, base, appendDelta, extensions, complete, tableTailUnstable, appendInfo) => {
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
const openFenceSourceEnd = (base, fence, complete) => {
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
const renderOpenFenceRaw = (base, fence, complete, sourceEnd) => {
    if (complete !== defaultIncompleteMarkdown)
        return base;
    const visibleSource = sourceEnd === base.length
        ? base
        : base.slice(0, sourceEnd);
    return visibleSource + '\n' + fence.char.repeat(fence.length);
};
const renderOpenFenceToken = (base, fence, complete) => {
    const sourceEnd = openFenceSourceEnd(base, fence, complete);
    const text = base.slice(fence.bodyStart, sourceEnd);
    return {
        raw: renderOpenFenceRaw(base, fence, complete, sourceEnd),
        text
    };
};
const openFenceLexRecord = (base, token, complete) => {
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
const mergeOpenFence = (head, base, appendDelta, complete, fence) => {
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
    let text;
    let textAppend;
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
export const incrementalLex = (block, extensions = [], cache, complete = null, provenAppend) => {
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
            return cache.tokens;
        }
        const appendDelta = base.length > cache.src.length
            ? appendIsProven
                ? trim.append
                : base.startsWith(cache.src)
                    ? base.slice(cache.src.length)
                    : null
            : null;
        if (cache.tokens.length === 1 &&
            appendDelta !== null) {
            const head = cache.tokens[0];
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
            let merged = null;
            let nextTableAppend = null;
            let path = 'full';
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
