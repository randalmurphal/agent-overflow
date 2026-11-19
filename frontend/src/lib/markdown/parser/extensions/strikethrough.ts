/**
 * GFM strikethrough: `~text~` or `~~text~~` renders struck through, as GitHub
 * and GitLab render it. Opt-in: agent chat reads `~text~` as subscript and
 * strikes only `~~text~~` (the engine `del` rule), so prose like `~5s` is
 * never struck.
 *
 * The rule is marked's GFM `del`, which the engine narrows to `~~`: a run of
 * one or two tildes followed by neither white space nor a tilde, then the
 * shortest text up to a run of the same length that follows no white space
 * and is followed by no tilde. A code span binds tighter: one that opens
 * inside the text and closes after the closing run keeps the opening run
 * literal. A run that opens nothing (three or more tildes, or no closer) is
 * one text token, so no rule claims a tilde inside it. It runs ahead of the
 * default extensions, so subscript, which a forge does not have, never
 * claims a tilde.
 */
import type { Lexer, Tokens } from '../engine';
import type { Extension } from '../lexer';

const STRIKETHROUGH = /^(~~?)(?=[^\s~])((?:\\[\s\S]|[^\\])*?(?:\\[\s\S]|[^\s~\\]))\1(?=[^~]|$)/;

/** Whether a code span opening in `src` before `end` closes after it. */
export const codeSpanCrosses = (lexer: Lexer, src: string, start: number, end: number): boolean => {
	for (let at = start; at < end; at++) {
		const code = src.charCodeAt(at);
		if (code === 92 /* \ */) {
			at++;
			continue;
		}
		if (code !== 96 /* ` */) continue;
		const span = lexer.tokenizer.codespan(src.slice(at));
		if (span) {
			if (at + span.raw.length > end) return true;
			at += span.raw.length - 1;
			continue;
		}
		// An unmatched run stays text as a whole.
		while (src.charCodeAt(at + 1) === 96) at++;
	}
	return false;
};

export const markedStrikethrough: Extension = {
	name: 'strikethrough',
	level: 'inline',
	precedesDefaults: true,
	start(src) {
		const index = src.indexOf('~');
		return index === -1 ? undefined : index;
	},
	tokenizer(src) {
		if (src.charCodeAt(0) !== 126 /* ~ */) return undefined;
		const match = STRIKETHROUGH.exec(src);
		const open = match?.[1].length ?? 0;
		if (!match || codeSpanCrosses(this.lexer, src, open, open + match[2].length)) {
			let end = 1;
			while (src.charCodeAt(end) === 126) end++;
			const run: Tokens.Text = { type: 'text', raw: src.slice(0, end), text: src.slice(0, end) };
			return run;
		}
		const del: Tokens.Del = {
			type: 'del',
			raw: match[0],
			text: match[2],
			tokens: this.lexer.inlineTokens(match[2])
		};
		return del;
	}
};
