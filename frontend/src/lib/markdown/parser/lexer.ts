/**
 * The lexer seam: the extension registries, the cached options objects, and
 * the lex entry points everything else in this tree builds on.
 *
 * The grammar itself lives in `engine/` (marked 16.4.2's lexing half, absorbed
 * as first-party source); the rule overrides this module used to apply by
 * mutating `Lexer.rules` at import time are literals in `engine/rules.ts` now.
 */
import { Lexer } from './engine';
import type { LexerOptions, MarkedToken, Token, TokenizerStartFunction, TokenizerThis, Tokens, TokensList } from './engine';
import type { AlertToken } from './extensions/alert';
import type { Footnote, FootnoteMaps, FootnoteToken } from './extensions/footnotes';
import type { MathToken } from './extensions/math';
import type { SubSupToken } from './extensions/subsup';
import type { ListItemToken, ListToken } from './extensions/list';
import type { BrToken } from './extensions/br';
import type { HrToken } from './extensions/hr';
import type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './extensions/table';
import type { DescriptionDetailToken, DescriptionListToken, DescriptionTermToken, DescriptionToken } from './extensions/dl';
import type { AlignToken } from './extensions/align';
import type { CitationToken } from './extensions/citations';
import type { MdxToken } from './extensions/mdx';
import { markedAlert } from './extensions/alert';
import { markedFootnote, markedFootnoteBlock } from './extensions/footnotes';
import { markedMath } from './extensions/math';
import { markedSub, markedSup } from './extensions/subsup';
import { markedList, markedListBlock } from './extensions/list';
import { markedBr } from './extensions/br';
import { markedHr } from './extensions/hr';
import { markedTable } from './extensions/table';
import { markedTableBlock } from './extensions/tableSource';
import { markedDl, markedDlBlock } from './extensions/dl';
import { markedAlign, markedAlignBlock } from './extensions/align';
import { markedCitations } from './extensions/citations';
import { markedMdx, markedMdxBlock } from './extensions/mdx';
import { markedBlockquoteBlock } from './extensions/blockquoteSource';
// Default plugin sets, in registration order. Hoisted so the options object
// (and the regexes/closures inside each plugin) is built once, not per chunk.
// The tokenizers are stateless at creation time — per-document state lives on
// the Lexer instance (e.g. footnotes maps, reference-link defs), so a fresh
// Lexer per call keeps documents isolated while the options are shared.
const DEFAULT_LEX_EXTENSIONS: Extension[] = [
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
const DEFAULT_BLOCK_EXTENSIONS: Extension[] = [
    markedHr,
    markedFootnoteBlock,
    markedBlockquoteBlock,
    markedListBlock,
    markedDlBlock,
    markedTableBlock,
    markedAlignBlock,
    markedMdxBlock
];
const parseExtensions = (...extensions: Extension[]): LexerOptions => {
    const options: LexOptions = {
        gfm: true,
        extensions: {
            block: [],
            inline: [],
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
    // Every bag this returns is shared across Lexer instances via the caches
    // below, and `lexer.options` is a public field — freeze so a write
    // through one instance throws instead of silently reconfiguring every
    // later Lexer built from the same cache entry.
    Object.freeze(options.extensions.block);
    Object.freeze(options.extensions.inline);
    Object.freeze(options.extensions.startBlock);
    Object.freeze(options.extensions.startInline);
    Object.freeze(options.extensions);
    return Object.freeze(options);
};
// Options objects are stateless and reusable across Lexer instances; cache them
// per user-extension array (props are referentially stable across chunks) so the
// hot path skips rebuilding ~20 tokenizer registrations on every streamed chunk.
const DEFAULT_LEX_OPTIONS = parseExtensions(...DEFAULT_LEX_EXTENSIONS);
export const DEFAULT_BLOCK_OPTIONS = parseExtensions(...DEFAULT_BLOCK_EXTENSIONS);
const lexOptionsCache = new WeakMap<Extension[], LexerOptions>();
const blockOptionsCache = new WeakMap<Extension[], LexerOptions>();
const blockExtensionsCache = new WeakMap<Extension[], Extension[] | null>();
export const blockExtensionsOf = (extensions: Extension[]): Extension[] | null => {
    if (extensions.length === 0)
        return null;
    const cached = blockExtensionsCache.get(extensions);
    if (cached !== undefined)
        return cached;
    let blockExtensions: Extension[] | null = null;
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
export const sameExtensionSequence = (left: Extension[] | null, right: Extension[] | null): boolean => {
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
export const getLexOptions = (extensions: Extension[]): LexerOptions => {
    if (extensions.length === 0)
        return DEFAULT_LEX_OPTIONS;
    let options = lexOptionsCache.get(extensions);
    if (!options) {
        options = parseExtensions(...DEFAULT_LEX_EXTENSIONS, ...extensions);
        lexOptionsCache.set(extensions, options);
    }
    return options;
};
export const getBlockOptions = (extensions: Extension[]): LexerOptions => {
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
export const lex = (markdown: string, extensions: Extension[] = []): StreamdownToken[] => {
    return new Lexer(getLexOptions(extensions))
        .lex(markdown)
        .filter((token) => token.type !== 'space' && token.type !== 'footnote') as StreamdownToken[];
};
// The footnote seam (see AGENTS.md § Host seams): `[^label]` → its
// `[^label]: body` definition, for a whole
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
export const lexFootnoteDefinitions = (
    markdown: string,
    extensions: Extension[] = []
): Map<string, Footnote> | null => {
    const lexer = new Lexer(getLexOptions(extensions));
    // Seed BEFORE the run: the footnote extension prefers a seeded lexer's
    // maps over ambient Svelte context, which is what keeps this
    // whole-document lex isolated when it runs during a component's init
    // under some other Streamdown surface.
    lexer.footnotes = { refs: new Map(), footnotes: new Map() };
    lexer.lex(markdown);
    return lexer.footnotes.footnotes.size > 0 ? lexer.footnotes.footnotes : null;
};
// A token the render list keeps: marked's blank-line `space` tokens and
// footnote definitions own source bytes but produce no rendered block.
export const isKeptType = (type: string): boolean => type !== 'space' && type !== 'footnote';
// lex(), but the Lexer is observable: seeded with a prior links table /
// footnote maps before the run, captured after. The public lex() stays a
// pure function; the incremental paths need the per-document state to
// carry sealed-item context into a tail-only re-lex.
export const lexCapture = (
    markdown: string,
    extensions: Extension[],
    seedLinks: LinkTable | null,
    seedFootnotes: FootnoteMaps | null
): LexCapture => {
    const lexer = new Lexer(getLexOptions(extensions));
    if (seedLinks)
        Object.assign(lexer.tokens.links, seedLinks);
    if (seedFootnotes)
        lexer.footnotes = seedFootnotes;
    const tokens = lexer.lex(markdown).filter((token) => isKeptType(token.type)) as StreamdownToken[];
    return {
        tokens,
        links: lexer.tokens.links,
        // In component context the footnote extension uses the shared
        // Streamdown maps and never touches the lexer — null here keeps
        // the cache out of the way and the context authoritative.
        footnotes: lexer.footnotes
    };
};

/**
 * marked's own generic-token contract, restated locally: an extension may hang
 * any field it likes on a token, so `Record<string, any>` is the declared
 * shape rather than a widening added here.
 */
export type GenericToken = {
    type: string;
    raw: string;
} & Record<string, any>;
export type Extension = {
    name: string;
    level: 'block' | 'inline';
    tokenizer: (this: TokenizerThis, src: string, tokens: Token[] | TokensList) => GenericToken | undefined;
    start?: TokenizerStartFunction;
    applyInBlockParsing?: boolean;
};
export type StreamdownToken = Exclude<MarkedToken, Tokens.List | Tokens.ListItem | Tokens.Table> | ListToken | ListItemToken | MathToken | AlertToken | FootnoteToken | SubSupToken | BrToken | HrToken | TableToken | THead | TBody | TFoot | THeadRow | TRow | TH | TD | DescriptionListToken | DescriptionToken | DescriptionDetailToken | DescriptionTermToken | AlignToken | CitationToken | MdxToken;
export type { TableToken, THead, TBody, TFoot, THeadRow, TRow, TH, TD } from './extensions/table';

/** marked's per-Lexer reference-link table. */
export type LinkTable = TokensList['links'];

/** What `lexCapture` reports back from an observable lex run. */
export type LexCapture = {
	tokens: StreamdownToken[];
	links: LinkTable;
	footnotes: FootnoteMaps | null;
};

/** The options object `parseExtensions` builds, with its arrays non-optional. */
type LexOptions = LexerOptions & {
	extensions: {
		block: NonNullable<NonNullable<LexerOptions['extensions']>['block']>;
		inline: NonNullable<NonNullable<LexerOptions['extensions']>['inline']>;
		startBlock: NonNullable<NonNullable<LexerOptions['extensions']>['startBlock']>;
		startInline: NonNullable<NonNullable<LexerOptions['extensions']>['startInline']>;
	};
};
export type { MathToken, AlertToken, FootnoteToken, SubSupToken, BrToken, HrToken, AlignToken, CitationToken, MdxToken };
