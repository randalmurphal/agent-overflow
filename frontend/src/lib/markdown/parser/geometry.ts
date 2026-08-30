/**
 * Source-shape geometry shared by both incremental layers.
 *
 * Nothing here lexes or holds state: these are the byte-level predicates
 * `parseBlocks` (outer block boundaries) and `incrementalLex` (token trees)
 * both need to answer "which bytes of this construct can appended bytes
 * still change?". Keeping them in one leaf module is what stops the two
 * layers from drifting into two different answers for the same shape.
 */
// A one-line paragraph whose first word cannot still become this package's
// alphabetic/Roman list marker is block-stable until a newline arrives. Inline
// punctuation may still change its token tree, but that belongs to
// incrementalLex; parseBlocks only owns the outer raw boundary. Requiring a
// multi-letter non-Roman word is deliberately conservative around partial
// list, HTML, MDX, math, definition, fence, and description-list openers.
const STABLE_PARAGRAPH_WORD = /^ {0,3}([\p{L}]+)/u;
const ROMAN_MARKER_PREFIX = /^[ivxlcdm]+$/i;
export const paragraphAppendSafe = (raw: string): boolean => {
    if (/[\r\n]/.test(raw))
        return false;
    const word = STABLE_PARAGRAPH_WORD.exec(raw)?.[1];
    return word !== undefined && word.length > 1 && !ROMAN_MARKER_PREFIX.test(word);
};
// --- Open-fence append geometry --------------------------------------------
// Marked treats an unclosed fenced block as one token through EOF. Re-lexing
// that token for every streamed word makes a long code answer O(n²), even
// though append-only bytes inside an open fence cannot alter any earlier block
// boundary. Track only the current source line's closer-candidate state. A
// valid closer causes an immediate fallback to the normal lexer, where the
// code block may close and following markdown may open new blocks.
const newFenceLineState = (lineStart: number): FenceLineState => ({
    phase: 'leading',
    indent: 0,
    run: 0,
    lineStart
});
const fenceLineCloses = (state: FenceLineState, fenceLength: number): boolean => (state.phase === 'run' || state.phase === 'trailing') && state.run >= fenceLength;
export const scanFenceBody = (
    source: string,
    start: number,
    fenceChar: string,
    fenceLength: number,
    initialState?: FenceLineState,
    sourceOffset = 0
): { closed: boolean; state: FenceLineState } => {
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
export const openFenceInfo = (source: string): OpenFence | null => {
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
        char: fence[0] as '`' | '~',
        length: fence.length,
        bodyStart,
        state: scan.state
    };
};
// A list token's items: `.tokens` from this package's markedList (the lex
// layer — getLexOptions registers it), `.items` from marked's built-in list
// tokenizer (the block layer — getBlockOptions deliberately does not).
const itemsOf = (list: BlockToken): readonly BlockTokenChild[] | null => {
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
export const sealedLengthOf = (list: BlockToken): number => {
    if (typeof list.sealedLen === 'number' && Number.isInteger(list.sealedLen))
        return list.sealedLen;
    const items = itemsOf(list);
    if (!items)
        return 0;
    let len = 0;
    for (let i = 0; i < items.length - 1; i++)
        len += items[i].raw?.length ?? 0;
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
export const lastLineStartOf = (source: string): number => {
    let end = source.length;
    while (end > 0 && source[end - 1] === '\n')
        end--;
    return source.lastIndexOf('\n', end - 1) + 1;
};
export const tablePrefixLength = (table: BlockToken, source: string): number | null => {
    if (table.type !== 'table')
        return null;
    const thead = Array.isArray(table.tokens) ? table.tokens[0] : undefined;
    const headerRowCount = typeof table.headerRowCount === 'number' && Number.isInteger(table.headerRowCount)
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
export const tableAppendInfo = (table: BlockToken, source: string): TableAppendInfo | null => {
    if (table.hasFooter === true ||
        (table.hasFooter === undefined &&
            Array.isArray(table.tokens) &&
            table.tokens.some((section) => section.type === 'tfoot')))
        return null;
    const tbody = Array.isArray(table.tokens) ? table.tokens[1] : undefined;
    const bodyRowCount = typeof table.bodyRowCount === 'number' && Number.isInteger(table.bodyRowCount)
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
export const tableTailUnsafe = (slice: string): boolean => ROWSPAN_CARET.test(slice) || FOOTER_MARKER_LINE.test(slice);
// A rowspan mutation is retroactive AND revocable while its row streams:
// a cell that momentarily ends with `^` (e.g. a half-arrived footnote ref
// `[^t]`) rowspan-mutates the PREVIOUS row during that tick's full lex,
// and the next characters un-happen it in a fresh parse — but the
// mutation is already baked into the cached sealed rows. Sealed rows are
// therefore only trustworthy when the cached document's tail carries no
// such trigger; a complete `^` row (newline landed) is permanent and
// safe. Matches the trailing cell shape: caret, optional spaces, an
// optional closing pipe, end of document.
export const hasTrailingRowspanCaret = (source: string): boolean => {
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

/**
 * A block token read structurally. marked's `Token` union and this package's
 * extension tokens share no supertype, and this layer only probes for shape —
 * so every field past `type`/`raw` is optional and guarded at the read.
 */
export type BlockToken = {
	type: string;
	raw: string;
	tokens?: readonly BlockTokenChild[];
	items?: readonly BlockTokenChild[];
	sealedLen?: number;
	headerRowCount?: number;
	bodyRowCount?: number;
	hasFooter?: boolean;
};

/**
 * Whatever a block token hangs under `tokens`/`items`: nested block tokens,
 * list items, or a table's sections. Only `type`, `raw` and a nested `tokens`
 * length are ever read here.
 */
type BlockTokenChild = {
	type: string;
	raw?: string;
	tokens?: readonly unknown[];
};
/** Per-line closer-candidate state for an open fence (see scanFenceBody). */
export type FenceLineState = {
	phase: 'leading' | 'run' | 'trailing' | 'invalid';
	indent: number;
	run: number;
	lineStart: number;
};

/** An unclosed fenced block: its delimiter, body offset and closer state. */
export type OpenFence = {
	char: '`' | '~';
	length: number;
	bodyStart: number;
	state: FenceLineState;
};

/** Byte geometry of a trailing table's sealed prefix and volatile last row. */
export type TableAppendInfo = {
	prefixLen: number;
	lastRowStart: number;
	prefix: string;
	lastRow: string;
};
