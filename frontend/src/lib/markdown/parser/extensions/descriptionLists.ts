/**
 * GitLab's description lists: a paragraph followed by lines that open with
 * `:` and a blank is a term with its details.
 *
 *     Fruits
 *     : apple
 *     : orange
 *
 * Opt-in: GitHub, CommonMark and agent chat read the lines as a paragraph.
 *
 * A term is a whole paragraph, so the term run must lex as exactly one
 * paragraph; a heading or list line before a `:` line is not a term. Each
 * `:` line opens a detail. A detail continues on the following lines that
 * neither open a detail nor are a term (a line directly followed by a `:`
 * line), and after blank lines on lines indented by two or more columns.
 * Blank lines between details, and between a detail and the next term, keep
 * one list. A detail with no blank line inside lexes as tight content, as a
 * tight list item does; GitLab renders a detail without a paragraph either
 * way.
 *
 * The tokens are the description list tokens the chat grammar
 * (extensions/dl.ts) also builds. The list spans blank lines, so the rule
 * takes part in block splitting; its term can be a paragraph sealed before
 * the first `:` line arrives, so no sealed boundary holds while it does.
 */
import { Lexer } from '../engine';
import type { Extension } from '../lexer';
import type { DescriptionDetailToken, DescriptionListToken, DescriptionTermToken, DescriptionToken } from './dl';

const DETAIL = /^ {0,3}:(?:[ \t]+|$)/;
const BLANK = /^[ \t]*$/;
const INDENTED = /^(?: {2,}|\t)/;

type Line = { text: string; start: number; end: number };

/** The lines of `src`, each with its offsets; `end` includes the newline. */
const linesOf = (src: string): Line[] => {
	const lines: Line[] = [];
	for (let start = 0; start < src.length; ) {
		const newline = src.indexOf('\n', start);
		const end = newline === -1 ? src.length : newline + 1;
		lines.push({ text: src.slice(start, newline === -1 ? end : newline), start, end });
		start = end;
	}
	return lines;
};

/** The index of the `:` line ending a term run that starts at `from`, or -1. */
const termEnd = (lines: Line[], from: number): number => {
	for (let at = from; at < lines.length; at++) {
		const text = lines[at].text;
		if (BLANK.test(text)) return -1;
		if (DETAIL.test(text)) return at === from ? -1 : at;
	}
	return -1;
};

/** Whether a `:` line follows the first line of `src` before a blank line. */
const mayOpen = (src: string): boolean => {
	let start = src.indexOf('\n') + 1;
	while (start > 0 && start < src.length) {
		const newline = src.indexOf('\n', start);
		const line = src.slice(start, newline === -1 ? src.length : newline);
		if (BLANK.test(line)) return false;
		if (DETAIL.test(line)) return true;
		start = newline + 1;
	}
	return false;
};

export const markedDescriptionLists: Extension = {
	name: 'descriptionLists',
	level: 'block',
	applyInBlockParsing: true,
	reopensSealedBlocks: true,
	tokenizer(src) {
		const code = src.charCodeAt(0);
		if (code === 10 /* \n */ || code === 58 /* : */ || !mayOpen(src)) return undefined;
		const lines = linesOf(src);
		const groups: DescriptionToken[] = [];
		let at = 0;
		let end = 0;
		let more = true;
		while (more) {
			const detailAt = termEnd(lines, at);
			if (detailAt === -1) break;
			const groupStart = lines[at].start;
			const termRaw = src.slice(groupStart, lines[detailAt].start);
			// A throwaway Lexer: the probe's queued inline work is discarded.
			const probe = new Lexer(this.lexer.options).blockTokens(termRaw, []);
			if (probe.length !== 1 || probe[0].type !== 'paragraph' || probe[0].raw !== termRaw) break;
			const term: DescriptionTermToken = {
				type: 'descriptionTerm',
				raw: termRaw,
				tokens: this.lexer.inline(probe[0].text)
			};
			const details: DescriptionDetailToken[] = [];
			at = detailAt;
			more = false;
			while (at < lines.length && DETAIL.test(lines[at].text)) {
				const first = at;
				let content = lines[at].text.replace(DETAIL, '');
				let last = at;
				let loose = false;
				for (at++; at < lines.length; ) {
					const text = lines[at].text;
					if (BLANK.test(text)) {
						let next = at;
						while (next < lines.length && BLANK.test(lines[next].text)) next++;
						if (next === lines.length || !INDENTED.test(lines[next].text)) break;
						content += '\n'.repeat(next - at);
						loose = true;
						at = next;
						continue;
					}
					if (DETAIL.test(text) || termEnd(lines, at) === at + 1) break;
					content += '\n' + text.replace(INDENTED, '');
					last = at;
					at++;
				}
				const top = this.lexer.state.top;
				this.lexer.state.top = loose;
				const tokens = this.lexer.blockTokens(content, []);
				this.lexer.state.top = top;
				end = lines[last].end;
				details.push({ type: 'descriptionDetail', raw: src.slice(lines[first].start, end), tokens });
				// Blank lines before another detail or term stay in this list.
				let next = at;
				while (next < lines.length && BLANK.test(lines[next].text)) next++;
				if (next === lines.length) break;
				if (DETAIL.test(lines[next].text)) at = next;
				else if (termEnd(lines, next) !== -1) {
					at = next;
					more = true;
					break;
				} else break;
			}
			groups.push({ type: 'description', raw: src.slice(groupStart, end), tokens: [term, ...details] });
		}
		if (groups.length === 0) return undefined;
		const raw = src.slice(0, end);
		const list: DescriptionListToken = { type: 'descriptionList', raw, text: raw, tokens: groups };
		return list;
	}
};
