/**
 * Opt-in embedded-HTML support for forge-authored markdown (PR/MR
 * descriptions, review-thread comments, bot reports).
 *
 * Deliberately NOT in the default extension sets: agent chat keeps
 * `renderHtml={false}` and renders no HTML at all. A surface that opts in
 * registers BOTH extensions below and passes
 * `render/htmlSanitize.ts#sanitizeEmbeddedHtmlToken` as `renderHtml`, so
 * every html token has exactly one of three fates:
 *
 *  - a STRUCTURAL token rendered by our own elements — `<details>` /
 *    `<summary>` become container tokens whose children are ordinary
 *    markdown tokens (the content inside a forge dropdown is markdown),
 *    and the safe inline pairs map onto the native inline tokens
 *    (`<b>` → strong, `<a href>` → link, `<img>` → image), which is what
 *    routes their hrefs/srcs through the existing render-layer URL policy;
 *  - a SANITIZED fragment (raw HTML tables, badge rows) via the
 *    sanitizer; or
 *  - escaped literal text, so unrecognized HTML is never silently
 *    dropped. Only comments render as nothing, which every renderer
 *    agrees on.
 */
import type { Extension } from '../lexer';
import type { Token } from '../engine';

export type DetailsToken = {
	type: 'details';
	raw: string;
	/** The authored `open` attribute: render expanded. */
	open: boolean;
	/** `[detailsSummary?, ...body block tokens]`. */
	tokens: Token[];
};

export type DetailsSummaryToken = {
	type: 'detailsSummary';
	raw: string;
	text: string;
	tokens: Token[];
};

const DETAILS_OPEN = /^<details\b([^>\n]*)>/i;
// One scan regex per line: open or close tags, in document order.
const DETAILS_TAG = /<(\/?)details\b[^>\n]*>/gi;
const FENCE_LINE = /^ {0,3}(`{3,}|~{3,})/;
const SUMMARY_OPEN = /^\s*<summary\b[^>\n]*>/i;
const SUMMARY_CLOSE = /<\/summary\s*>/i;
const OPEN_ATTR = /(?:^|\s)open(?:\s*=|\s|$)/i;
// A token the render list keeps (mirrors lexer.ts#isKeptType; restated
// here to avoid an import cycle through the barrel).
const kept = (type: string): boolean => type !== 'space' && type !== 'footnote';

/**
 * Find the index just past the `</details>` that closes the tag opening
 * this source, nesting-aware and skipping fenced code (a fence inside a
 * dropdown routinely quotes literal `</details>`). Returns -1 when the
 * run never closes.
 */
const scanDetailsEnd = (src: string): number => {
	let depth = 0;
	let fence: string | null = null;
	let pos = 0;
	while (pos <= src.length) {
		const lineEnd = src.indexOf('\n', pos);
		const end = lineEnd === -1 ? src.length : lineEnd;
		const line = src.slice(pos, end);
		const fenceMatch = FENCE_LINE.exec(line);
		if (fence !== null) {
			if (fenceMatch && fenceMatch[1][0] === fence[0] && fenceMatch[1].length >= fence.length) {
				fence = null;
			}
		} else if (fenceMatch) {
			fence = fenceMatch[1];
		} else {
			DETAILS_TAG.lastIndex = 0;
			let tag: RegExpExecArray | null;
			while ((tag = DETAILS_TAG.exec(line)) !== null) {
				if (tag[1] === '/') {
					depth--;
					if (depth === 0) return pos + tag.index + tag[0].length;
				} else {
					depth++;
				}
			}
		}
		if (lineEnd === -1) break;
		pos = end + 1;
	}
	return -1;
};

export const markedDetails: Extension = {
	name: 'details',
	level: 'block',
	// The document→blocks splitter must consume a whole details run as ONE
	// block: the dropdown's body spans blank lines, and a split at the
	// first one would strand the closing tag in a later block.
	applyInBlockParsing: true,
	tokenizer(src) {
		if (src.charCodeAt(0) !== 60 /* < */) return undefined;
		const open = DETAILS_OPEN.exec(src);
		if (!open) return undefined;
		const endIndex = scanDetailsEnd(src);
		// Unclosed (truncated comment): claim nothing. The engine's html
		// block rule takes the line run and the sanitizer degrades it.
		if (endIndex === -1) return undefined;
		let raw = src.slice(0, endIndex);
		// Consume the rest of the closing tag's line so no stray html
		// token is minted for trailing whitespace.
		const trailing = /^[ \t]*(?:\r?\n|$)/.exec(src.slice(endIndex));
		if (trailing) raw = src.slice(0, endIndex + trailing[0].length);

		// The scanner's end index is past a close tag of variable spelling
		// (`</details >`); anchor the body on the tag's actual start.
		let inner = src.slice(open[0].length, endIndex);
		const closeStart = inner.toLowerCase().lastIndexOf('</details');
		if (closeStart !== -1) inner = inner.slice(0, closeStart);

		const tokens: Token[] = [];
		const summaryOpen = SUMMARY_OPEN.exec(inner);
		if (summaryOpen) {
			const rest = inner.slice(summaryOpen[0].length);
			const summaryClose = SUMMARY_CLOSE.exec(rest);
			if (summaryClose) {
				const summaryText = rest.slice(0, summaryClose.index).trim();
				tokens.push({
					type: 'detailsSummary',
					raw: inner.slice(0, summaryOpen[0].length + summaryClose.index + summaryClose[0].length),
					text: summaryText,
					// Deferred inline lexing on the ACTIVE lexer, like the
					// paragraph tokenizer: the array fills when lex() drains.
					tokens: this.lexer.inline(summaryText, [])
				} as DetailsSummaryToken);
				inner = rest.slice(summaryClose.index + summaryClose[0].length);
			}
		}
		for (const child of this.lexer.blockTokens(inner, [])) {
			if (kept(child.type)) tokens.push(child);
		}
		return { type: 'details', raw, open: OPEN_ATTR.test(open[1] ?? ''), tokens } as DetailsToken;
	}
};

// ---------------------------------------------------------------------------
// Inline safe-tag pairs
// ---------------------------------------------------------------------------

// Tag → the native inline token type it maps to. `codespan` targets take
// their inner source as literal text; the rest recurse into inline
// markdown so nesting (`<b><i>…</i></b>`, links inside bold) works.
const INLINE_PAIR_TARGETS: Record<string, 'strong' | 'em' | 'del' | 'codespan' | 'sub' | 'sup'> = {
	b: 'strong',
	strong: 'strong',
	i: 'em',
	em: 'em',
	del: 'del',
	s: 'del',
	strike: 'del',
	code: 'codespan',
	tt: 'codespan',
	kbd: 'codespan',
	samp: 'codespan',
	sub: 'sub',
	sup: 'sup'
};

const INLINE_OPEN = /^<(a|b|strong|i|em|del|s|strike|code|tt|kbd|samp|sub|sup|img)\b((?:[^>"'\n]|"[^"]*"|'[^']*')*)>/i;
const closeRegexCache = new Map<string, RegExp>();
const closeRegexFor = (tag: string): RegExp => {
	let regex = closeRegexCache.get(tag);
	if (!regex) {
		regex = new RegExp(`</${tag}\\s*>`, 'i');
		closeRegexCache.set(tag, regex);
	}
	return regex;
};

const attrOf = (attrs: string, name: string): string | null => {
	const match = new RegExp(`(?:^|\\s)${name}\\s*=\\s*("([^"]*)"|'([^']*)'|([^\\s>]+))`, 'i').exec(attrs);
	if (!match) return null;
	return match[2] ?? match[3] ?? match[4] ?? '';
};

export const markedEmbeddedInlineHtml: Extension = {
	name: 'embeddedInlineHtml',
	level: 'inline',
	tokenizer(src) {
		if (src.charCodeAt(0) !== 60 /* < */) return undefined;
		const open = INLINE_OPEN.exec(src);
		if (!open) return undefined;
		const tag = open[1].toLowerCase();
		const attrs = open[2] ?? '';

		if (tag === 'img') {
			const href = attrOf(attrs, 'src');
			if (href === null) return undefined;
			return {
				type: 'image',
				raw: open[0],
				href,
				title: attrOf(attrs, 'title'),
				text: attrOf(attrs, 'alt') ?? '',
				tokens: []
			};
		}

		const close = closeRegexFor(tag).exec(src.slice(open[0].length));
		// Unpaired: claim nothing; the engine's tag rule takes it and the
		// sanitizer renders it as escaped literal text.
		if (!close) return undefined;
		const inner = src.slice(open[0].length, open[0].length + close.index);
		const raw = src.slice(0, open[0].length + close.index + close[0].length);

		if (tag === 'a') {
			const href = attrOf(attrs, 'href');
			if (href === null) return undefined;
			return {
				type: 'link',
				raw,
				href,
				title: attrOf(attrs, 'title'),
				text: inner,
				tokens: this.lexer.inlineTokens(inner)
			};
		}

		const target = INLINE_PAIR_TARGETS[tag];
		if (target === 'codespan') {
			return { type: 'codespan', raw, text: inner };
		}
		return {
			type: target,
			raw,
			text: inner,
			tokens: this.lexer.inlineTokens(inner)
		};
	}
};

/** The registration set a forge surface passes as `extensions`. */
export const EMBEDDED_HTML_EXTENSIONS: Extension[] = [markedDetails, markedEmbeddedInlineHtml];
