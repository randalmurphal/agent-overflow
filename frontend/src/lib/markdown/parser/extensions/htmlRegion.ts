/**
 * An HTML region: an HTML block that leaves elements open, the Markdown and
 * HTML after it, through the HTML block that closes them.
 *
 * A forge renders Markdown to one HTML string and parses it whole, so
 * `<td>\n\n**x**\n\n</td>` puts the paragraph inside the cell. Lexed block by
 * block, the open tag's block would render as an empty element and the
 * Markdown after it. A region keeps them one token whose HTML is sanitized
 * and parsed as one fragment around its Markdown islands
 * (`render/htmlSanitize.ts#parseHtmlRegion`).
 *
 * Which elements an HTML block opens and closes, and where a region ends,
 * comes from one pairing of the run's HTML blocks (htmlRuns.ts): only the
 * container elements it tracks count, and only tags in HTML blocks the
 * block grammar reads, so a tag in a paragraph, a code span, a fence, a
 * comment or a script neither opens nor closes a region. A block that
 * leaves nothing open, or whose elements never close, is not claimed and
 * keeps the engine's html token. A region nested in another is part of it:
 * the inside of a region is lexed with regions off, so each run is paired
 * once however deep its HTML nests.
 */
import type { Extension } from '../lexer';
import type { Lexer, Token } from '../engine';
import {
	bindBlockInside,
	boundBlockRun,
	htmlBlockOpens,
	htmlBodyTokens,
	isMeasuringBlocks,
	measureBlockRun,
	measuredHtmlBlock,
	regionAt,
	type BlockRun
} from './htmlRuns';

export type HtmlRegionToken = {
	type: 'htmlRegion';
	/** Exactly the consumed source. */
	raw: string;
	/** In source order: HTML source strings and Markdown island tokens. */
	parts: (string | Token)[];
	/** The island tokens of `parts`, in order. */
	tokens: Token[];
};

// Tokens that render nothing; an island must render something.
const SHOWS_NOTHING = new Set(['space', 'footnote', 'def', 'align']);

/** The offset of the `<` an HTML block at `src` opens with, after up to three spaces; -1 otherwise. */
export const htmlBlockStart = (src: string): number => {
	let at = 0;
	while (at < 3 && src.charCodeAt(at) === 32) at++;
	return src.charCodeAt(at) === 60 /* < */ ? at : -1;
};

/**
 * The plan for the block run `src` is in, measured when a position first
 * needs one. Without a plan yet, an html block that leaves nothing open is
 * answered on its own.
 */
const runFor = (lexer: Lexer, src: string, tokens: Token[]): BlockRun | undefined =>
	boundBlockRun(lexer, src, tokens) ?? (htmlBlockOpens(lexer, src) ? measureBlockRun(lexer, src, tokens) : undefined);

export const markedHtmlRegion: Extension = {
	name: 'htmlRegion',
	level: 'block',
	// The document splitter must keep a region one block: it spans blank
	// lines, and a split at the first would strand its closing tags.
	applyInBlockParsing: true,
	reopensSealedBlocks: true,
	tokenizer(src, tokens) {
		const at = htmlBlockStart(src);
		if (at === -1) return undefined;
		if (isMeasuringBlocks(this.lexer)) return measuredHtmlBlock(this.lexer, src);
		const run = runFor(this.lexer, src, tokens as Token[]);
		if (!run) return undefined;
		const region = regionAt(run, src, at);
		if (!region) return undefined;
		// Inside an HTML block of the measured run (after a details run that
		// ended mid-block), the engine must read the same block here.
		if (!region.blockStart && this.lexer.tokenizer.html(src)?.raw.length !== region.first) return undefined;

		const parts: (string | Token)[] = [src.slice(0, region.first)];
		const islands: Token[] = [];
		const inside: Token[] = [];
		bindBlockInside(this.lexer, inside, run, src, region.end, true);
		// The ACTIVE lexer, so every extension (details, alerts) applies, and
		// inline content fills when the outer lex drains.
		for (const child of htmlBodyTokens(this.lexer, src.slice(region.first, region.end), inside)) {
			if (child.type === 'html') {
				const last = parts.length - 1;
				if (typeof parts[last] === 'string') parts[last] += child.raw;
				else parts.push(child.raw);
			} else if (!SHOWS_NOTHING.has(child.type)) {
				parts.push(child);
				islands.push(child);
			}
		}
		return { type: 'htmlRegion', raw: src.slice(0, region.end), parts, tokens: islands } satisfies HtmlRegionToken;
	}
};
