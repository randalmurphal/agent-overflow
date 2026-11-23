/**
 * Front matter: metadata between `---` (YAML), `+++` (TOML) or `;;;` (JSON)
 * lines at the very start of the source renders as a code block in that
 * language, as GitLab renders it. Opt-in: GitHub, CommonMark and agent chat
 * read the lines as Markdown (a rule, a setext heading, a paragraph).
 *
 * GitLab's rule (`Gitlab::FrontMatter`): the opening delimiter, optionally
 * followed by a language that replaces the default one (`---toml`), then
 * the shortest run of lines up to the same delimiter or `...` on a line of
 * its own. Without a closing line nothing is front matter. Only the
 * document's first block can be front matter, after blank lines at most: a
 * block lexed alone under a document scope asks the scope whether it opens
 * the document; without one, the lexer's source is the document.
 *
 * It runs ahead of the default extensions, whose rule claims `---` first,
 * and a closing line that arrives later turns sealed blocks into front
 * matter, so no sealed boundary holds while it takes part.
 */
import type { Token, Tokens } from '../engine';
import type { Extension } from '../lexer';
import { readDocument } from '../documentScope';

const FRONT_MATTER = /^(---|\+\+\+|;;;)[ \t]*(\S*)\n((?:[^\n]*\n)*?)(?:\1|\.\.\.)[ \t]*(?:\n|$)/;
const LANGUAGE: Record<string, string> = { '---': 'yaml', '+++': 'toml', ';;;': 'json' };
const blank = (tokens: readonly Token[]): boolean => tokens.every((token) => token.type === 'space');

export const markedFrontMatter: Extension = {
	name: 'frontMatter',
	level: 'block',
	applyInBlockParsing: true,
	reopensSealedBlocks: true,
	precedesDefaults: true,
	tokenizer(src, tokens) {
		const first = src.charCodeAt(0);
		if (first !== 45 /* - */ && first !== 43 /* + */ && first !== 59 /* ; */) return undefined;
		const lexer = this.lexer;
		if (lexer.blockDepth !== 1 || !blank(tokens as Token[]) || !blank(lexer.tokens)) return undefined;
		if (lexer.document) {
			readDocument(lexer, 's');
			if (!lexer.document.first) return undefined;
		}
		const match = FRONT_MATTER.exec(src);
		if (!match) return undefined;
		const code: Tokens.Code = {
			type: 'code',
			raw: match[0],
			lang: match[2] || LANGUAGE[match[1]],
			text: match[3].replace(/\n$/, '')
		};
		return code;
	}
};
