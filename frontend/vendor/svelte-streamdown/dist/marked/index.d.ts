import { type MarkedToken, type Token, type TokenizerStartFunction, type TokenizerThis, type Tokens, type TokensList } from 'marked';
import { type AlertToken } from './marked-alert.js';
import { type FootnoteToken } from './marked-footnotes.js';
import { type MathToken } from './marked-math.js';
import { type SubSupToken } from './marked-subsup.js';
import { type ListItemToken, type ListToken } from './marked-list.js';
import { type BrToken } from './marked-br.js';
import { type HrToken } from './marked-hr.js';
import { type TableToken, type THead, type TBody, type TFoot, type THeadRow, type TRow, type TH, type TD } from './marked-table.js';
import { type DescriptionDetailToken, type DescriptionListToken, type DescriptionTermToken, type DescriptionToken } from './marked-dl.js';
import { type AlignToken } from './marked-align.js';
import { type CitationToken } from './marked-citations.js';
import { type MdxToken } from './marked-mdx.js';
export type GenericToken = {
    type: string;
    raw: string;
    tokens?: Token[];
} & Record<string, any>;
export type Extension = {
    name: string;
    level: 'block' | 'inline';
    tokenizer: (this: TokenizerThis, src: string, tokens: Token[] | TokensList) => GenericToken | undefined;
    start?: TokenizerStartFunction;
    applyInBlockParsing?: boolean;
};
export type StreamdownToken = Exclude<MarkedToken, Tokens.List | Tokens.ListItem | Tokens.Table> | ListToken | ListItemToken | MathToken | AlertToken | FootnoteToken | SubSupToken | BrToken | HrToken | TableToken | THead | TBody | TFoot | THeadRow | TRow | TH | TD | DescriptionListToken | DescriptionToken | DescriptionDetailToken | DescriptionTermToken | AlignToken | CitationToken | MdxToken;
export type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './marked-table.js';
export declare const lex: (markdown: string, extensions?: Extension[]) => StreamdownToken[];
declare const provenAppendBrand: unique symbol;
export type ProvenAppend = Readonly<{
    previous: string;
    delta: string;
    next: string;
    [provenAppendBrand]: true;
}>;
export declare const createProvenAppend: (previous: string, delta: string) => ProvenAppend;
export declare const createMaterializedProvenAppend: (previous: string, deltas: readonly string[]) => ProvenAppend;
export declare const matchesProvenAppend: (proof: ProvenAppend | undefined, previous: string, next: string) => proof is ProvenAppend;
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
export declare const createParseBlocksCache: (observeLex?: ParseBlocksLexObserver) => ParseBlocksCache;
export declare const parseBlocks: (markdown: string, extensions?: Extension[], cache?: ParseBlocksCache, provenAppend?: ProvenAppend) => string[];
/** Enable or disable independent strings for cache-owned rendered blocks. */
export declare const updateParseBlockStringMaterialization: (cache: ParseBlocksCache, enabled: boolean) => void;
/**
 * Opaque incremental state for `incrementalLex`. Create one per streamed
 * block (a Block component instance) and pass it on every call. `lastPath`
 * is a debug breadcrumb ('none' | 'full' | 'list-append' | 'table-append')
 * so tests can assert the fast path actually engaged. `links` and
 * `footnotes` carry the last full lex's reference-link table and footnote
 * maps into tail-only re-lexes.
 */
export type IncrementalLexCache = {
    src: string;
    input: string;
    extKey: Extension[] | null;
    completeKey: ((markdown: string) => string) | null;
    tokens: StreamdownToken[] | null;
    links: Record<string, {
        href: string | null;
        title: string | null;
    }> | null;
    footnotes: unknown;
    codeFence: {
        char: '`' | '~';
        length: number;
        bodyStart: number;
        state: {
            phase: 'leading' | 'run' | 'trailing' | 'invalid';
            indent: number;
            run: number;
            lineStart: number;
        };
    } | null;
    /** prior table tail can still revoke a provisional rowspan mutation */
    tableTailUnstable: boolean;
    /** cached table header and volatile-row source offsets */
    tableAppend: {
        prefixLen: number;
        lastRowStart: number;
        prefix: string;
        lastRow: string;
    } | null;
    /** leading source units and trailing source text removed in completion mode */
    leadingTrim: number;
    trailingTrim: string;
    lastCodeTextAppend?: ProvenAppend;
    lastPath: 'none' | 'full' | 'list-append' | 'table-append' | 'code-append';
    /** optional diagnostic observer invoked when incrementalLex does parser work */
    observeLex?: IncrementalLexObserver;
};
export type IncrementalLexPath = Exclude<IncrementalLexCache['lastPath'], 'none'>;
export type IncrementalLexObserver = (path: IncrementalLexPath, inputLength: number) => void;
export declare const createIncrementalLexCache: (observeLex?: IncrementalLexObserver) => IncrementalLexCache;
/**
 * Drop-in replacement for `lex` on streaming content: identical output, but
 * append-only lists/tables re-lex only from the last cached item/source row,
 * and open fences update from their closer state. Sealed content keeps its
 * token references. `complete` (the incomplete-markdown pass)
 * runs inside so the fast path can scope it to the re-lexed slice; pass
 * null to lex verbatim.
 */
export declare const incrementalLex: (block: string, extensions: Extension[] | undefined, cache: IncrementalLexCache, complete?: ((markdown: string) => string) | null, provenAppend?: ProvenAppend) => StreamdownToken[];
export type { MathToken, AlertToken, FootnoteToken, SubSupToken, BrToken, HrToken, AlignToken, CitationToken, MdxToken };
