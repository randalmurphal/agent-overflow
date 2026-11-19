/**
 * GitLab's inline diff: `{+ added +}` and `[+ added +]` render as inserted
 * text, `{- removed -}` and `[- removed -]` as deleted text, without their
 * markers. Opt-in: GitHub, CommonMark and agent chat show them as text.
 *
 * GitLab matches the markers in text Markdown left plain, outside code: an
 * opener, the shortest run on its line up to the closer of the same bracket
 * kind, everything between them (spaces included) the content. Content that
 * is not plain text (a code span, emphasis, a link, a reference) is not one
 * text run there, so the markers stay literal here too: a marker is claimed
 * only when its content lexes to text and escapes alone, and nothing that
 * opens inside it (a code span, emphasis, a link) ends after the closer.
 * The second check lexes the rest of the run, so it runs only when the
 * content holds a character such a construct starts with. A `[` opener is
 * also the start of a link when a destination or a defined label follows.
 * An escaped opener is text. The chat grammar's citations are text on a
 * forge, so a citation token counts as text here.
 *
 * Insertion is an `ins` htmlInline token and deletion a `del` token, which
 * every renderer and find's projection already show.
 */
import type { Token, Tokens } from '../engine';
import type { Extension } from '../lexer';
import type { HtmlInlineToken } from './embeddedHtml';

const CLOSERS: Record<string, string> = { '{+': '+}', '[+': '+]', '{-': '-}', '[-': '-]' };
// A construct that can open inside the content and close after it.
const MAY_CROSS = /[`*_~[\]<!$:]/;

/** Set while the rest of a run is lexed to check a marker. */
let checking = false;

const PLAIN = new Set(['text', 'escape', 'inline-citations']);

const plain = (tokens: readonly Token[], end = Infinity): boolean => {
	let offset = 0;
	for (const token of tokens) {
		if (offset >= end) break;
		if (!PLAIN.has(token.type)) return false;
		offset += token.raw.length;
	}
	return true;
};

export const markedInlineDiff: Extension = {
	name: 'inlineDiff',
	level: 'inline',
	// Ahead of the default citation rule, which reads `[+ x +]` as a citation.
	precedesDefaults: true,
	start(src) {
		for (let at = src.indexOf('{'); at !== -1; at = src.indexOf('{', at + 1)) {
			const next = src.charCodeAt(at + 1);
			if (next === 43 /* + */ || next === 45 /* - */) return at;
		}
		return undefined;
	},
	tokenizer(src) {
		const first = src.charCodeAt(0);
		if ((first !== 123 /* { */ && first !== 91 /* [ */) || checking) return undefined;
		const closer = CLOSERS[src.slice(0, 2)];
		if (closer === undefined) return undefined;
		const lineEnd = src.indexOf('\n');
		const close = src.indexOf(closer, 2);
		if (close === -1 || (lineEnd !== -1 && close > lineEnd)) return undefined;
		const text = src.slice(2, close);
		const raw = src.slice(0, close + 2);
		const tokens = this.lexer.inlineTokens(text);
		if (!plain(tokens)) return undefined;
		if (first === 91) {
			const tokenizer = this.lexer.tokenizer;
			if (tokenizer.link(src)?.type === 'link' || tokenizer.reflink(src, this.lexer.tokens.links)?.type === 'link') return undefined;
		}
		if (MAY_CROSS.test(text)) {
			checking = true;
			try {
				if (!plain(this.lexer.inlineTokens(src), raw.length)) return undefined;
			} finally {
				checking = false;
			}
		}
		if (src.charCodeAt(1) === 45 /* - */) {
			const del: Tokens.Del = { type: 'del', raw, text, tokens };
			return del;
		}
		const ins: HtmlInlineToken = { type: 'htmlInline', raw, tag: 'ins', title: null, text, tokens };
		return ins;
	}
};
