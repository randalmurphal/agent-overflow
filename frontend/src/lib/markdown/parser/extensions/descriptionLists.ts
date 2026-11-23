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
import type { BlockRun } from '../engine/Lexer';
import type { Extension } from '../lexer';
import type { DescriptionDetailToken, DescriptionListToken, DescriptionTermToken, DescriptionToken } from './dl';

const DETAIL = /^ {0,3}:(?:[ \t]+|$)/;
const BLANK = /^[ \t]*$/;
const INDENTED = /^(?: {2,}|\t)/;

type Line = { text: string; start: number; end: number };

/** The lines of `src` from offset `from`, with their offsets, split only as
 * far as they are read; `end` includes the newline. */
class Lines {
	private readonly lines: Line[] = [];
	private next: number;

	constructor(private readonly src: string, from: number) {
		this.next = from;
	}

	/** Line `index`, or undefined past the end of `src`. */
	at(index: number): Line | undefined {
		while (this.lines.length <= index && this.next < this.src.length) {
			const start = this.next;
			const newline = this.src.indexOf('\n', start);
			const end = newline === -1 ? this.src.length : newline + 1;
			this.lines.push({ text: this.src.slice(start, newline === -1 ? end : newline), start, end });
			this.next = end;
		}
		return this.lines[index];
	}
}

const isBlank = (line: Line | undefined): boolean => line !== undefined && BLANK.test(line.text);
const isDetail = (line: Line | undefined): boolean => line !== undefined && DETAIL.test(line.text);

/** The index of the `:` line ending a term run that starts at `from`, or -1. */
const termEnd = (lines: Lines, from: number): number => {
	for (let at = from; ; at++) {
		const line = lines.at(at);
		if (line === undefined || BLANK.test(line.text)) return -1;
		if (DETAIL.test(line.text)) return at === from ? -1 : at;
	}
};

/** Per block run, its last scan: run offsets of the line it started on and
 * of the line it stopped at, a `:` line (`detail`) or a blank line or end. */
const scans = new WeakMap<BlockRun, { from: number; to: number; detail: boolean }>();

/**
 * The offset in `src` of the first `:` line after its first line and before
 * a blank line, or -1. In the block loop (`run`) a scan answers for each
 * later position of the run whose second line it read, so a run of lines
 * lexed one at a time (tight content, headings) is read once.
 */
const detailAfterFirstLine = (src: string, run: BlockRun | null): number => {
	const first = src.indexOf('\n') + 1;
	if (first === 0) return -1;
	const scan = run && scans.get(run);
	if (scan && run.at + first >= scan.from && run.at + first <= scan.to) return scan.detail ? scan.to - run.at : -1;
	let start = first;
	let detail = false;
	while (start < src.length) {
		const newline = src.indexOf('\n', start);
		const line = src.slice(start, newline === -1 ? src.length : newline);
		if (DETAIL.test(line)) {
			detail = true;
			break;
		}
		if (BLANK.test(line)) break;
		if (newline === -1) {
			start = src.length;
			break;
		}
		start = newline + 1;
	}
	if (run) scans.set(run, { from: run.at + first, to: run.at + start, detail });
	return detail ? start : -1;
};

// Set while a term run is probed. The run holds no `:` line, so this rule
// cannot open in it and need not scan it again.
let probingTerm = false;

export const markedDescriptionLists: Extension = {
	name: 'descriptionLists',
	level: 'block',
	applyInBlockParsing: true,
	reopensSealedBlocks: true,
	tokenizer(src) {
		const code = src.charCodeAt(0);
		if (probingTerm || code === 10 /* \n */ || code === 58 /* : */) return undefined;
		const firstDetail = detailAfterFirstLine(src, this.lexer.blockRun);
		if (firstDetail === -1) return undefined;
		// Line 0 is the first group's `:` line.
		const lines = new Lines(src, firstDetail);
		const groups: DescriptionToken[] = [];
		let groupStart = 0;
		let at = 0;
		let end = 0;
		let more = true;
		while (more) {
			const termRaw = src.slice(groupStart, lines.at(at)!.start);
			// A throwaway Lexer: the probe's queued inline work is discarded.
			let paragraph;
			probingTerm = true;
			try {
				paragraph = new Lexer(this.lexer.options).loneParagraph(termRaw);
			} finally {
				probingTerm = false;
			}
			if (!paragraph) break;
			const term: DescriptionTermToken = {
				type: 'descriptionTerm',
				raw: termRaw,
				tokens: this.lexer.inline(paragraph.text)
			};
			const details: DescriptionDetailToken[] = [];
			let nextGroup = groupStart;
			more = false;
			while (isDetail(lines.at(at))) {
				const first = at;
				let content = lines.at(at)!.text.replace(DETAIL, '');
				let last = at;
				let loose = false;
				for (at++; ; ) {
					const line = lines.at(at);
					if (line === undefined) break;
					if (BLANK.test(line.text)) {
						let next = at;
						while (isBlank(lines.at(next))) next++;
						const after = lines.at(next);
						if (after === undefined || !INDENTED.test(after.text)) break;
						content += '\n'.repeat(next - at);
						loose = true;
						at = next;
						continue;
					}
					// A line directly followed by a `:` line is the next term.
					if (DETAIL.test(line.text) || isDetail(lines.at(at + 1))) break;
					content += '\n' + line.text.replace(INDENTED, '');
					last = at;
					at++;
				}
				const top = this.lexer.state.top;
				this.lexer.state.top = loose;
				const tokens = this.lexer.blockTokens(content, []);
				this.lexer.state.top = top;
				end = lines.at(last)!.end;
				details.push({ type: 'descriptionDetail', raw: src.slice(lines.at(first)!.start, end), tokens });
				// Blank lines before another detail or term stay in this list.
				let next = at;
				while (isBlank(lines.at(next))) next++;
				if (lines.at(next) === undefined) break;
				if (isDetail(lines.at(next))) {
					at = next;
					continue;
				}
				const nextDetail = termEnd(lines, next);
				if (nextDetail === -1) break;
				nextGroup = lines.at(next)!.start;
				at = nextDetail;
				more = true;
				break;
			}
			groups.push({ type: 'description', raw: src.slice(groupStart, end), tokens: [term, ...details] });
			groupStart = nextGroup;
		}
		if (groups.length === 0) return undefined;
		const raw = src.slice(0, end);
		const list: DescriptionListToken = { type: 'descriptionList', raw, text: raw, tokens: groups };
		return list;
	}
};
