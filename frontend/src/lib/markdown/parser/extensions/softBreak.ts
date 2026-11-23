/**
 * A soft line break inside a paragraph renders as `<br>`, as GitHub renders
 * PR bodies and comments. Opt-in: CommonMark, GitLab and agent chat render
 * a soft break as a space.
 *
 * This is marked's `breaks: true` grammar as an extension, because the
 * engine options are shared by every lexer. The rule is the engine's own
 * (`inline.breaks.br`): spaces or a backslash, then a newline not followed
 * by only whitespace, so a paragraph's last newline stays plain. `raw`
 * keeps the spaces before the newline.
 */
import { inline } from '../engine';
import type { Extension } from '../lexer';

const SOFT_BREAK = inline.breaks.br;

export const markedSoftBreak: Extension = {
	name: 'softBreak',
	level: 'inline',
	// Stop the text rule before the spaces that lead into a newline, as the
	// `breaks` text rule does. A backslash already stops it.
	start(src) {
		const newline = src.indexOf('\n');
		if (newline === -1) return undefined;
		let start = newline;
		while (start > 0 && src.charCodeAt(start - 1) === 32 /* space */) start--;
		return start;
	},
	tokenizer(src) {
		const first = src.charCodeAt(0);
		if (first !== 10 /* \n */ && first !== 32 /* space */ && first !== 92 /* \ */) return undefined;
		const match = SOFT_BREAK.exec(src);
		return match ? { type: 'br', raw: match[0] } : undefined;
	}
};
