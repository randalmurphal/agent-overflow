/**
 * GitLab's multiline blockquote: a line of three or more `>` opens a quote
 * that runs to a line of at least as many `>`, so pasted text is quoted
 * without a `>` on every line. Opt-in: GitHub, CommonMark and agent chat
 * read the line as nested empty quotes.
 *
 * Each fence line holds only its `>` run, indented up to three spaces, with
 * optional trailing blanks. A fence line inside a fenced code block in the
 * quote does not close it. With no closing line the quote runs to the end
 * of the source, as GitLab renders it. The body lexes as blocks, so the
 * token is the engine's `blockquote`. The body can span blank lines, so the
 * rule takes part in block splitting, and it runs ahead of the default
 * blockquote rules, which would claim the `>` line first.
 *
 * An alert marker after the opening fence (`>>> [!note] Title`) makes the
 * quote an alert, titled by the rest of that line, as GitLab's `>` alerts
 * are (alert.ts `gitlabAlertMarker`).
 */
import type { Tokens } from '../engine';
import type { Extension } from '../lexer';
import { alertToken, gitlabAlertMarker } from './alert';

const FENCE = /^ {0,3}(>{3,})[ \t]*$/;
const OPENER = /^ {0,3}(>{3,})/;
const CODE_FENCE = /^ {0,3}(`{3,}|~{3,})/;

/** The length of the `>` fence `line` is, or 0. */
const fenceLength = (line: string): number => {
	const match = FENCE.exec(line);
	return match ? match[1].length : 0;
};

export const markedMultilineBlockquote: Extension = {
	name: 'multilineBlockquote',
	level: 'block',
	applyInBlockParsing: true,
	precedesDefaults: true,
	tokenizer(src) {
		let lead = 0;
		while (lead < 3 && src.charCodeAt(lead) === 32) lead++;
		if (src.charCodeAt(lead) !== 62 /* > */ || src.charCodeAt(lead + 2) !== 62) return undefined;
		const firstEnd = src.indexOf('\n');
		const firstLine = firstEnd === -1 ? src : src.slice(0, firstEnd);
		let opener = fenceLength(firstLine);
		let alert: ReturnType<typeof gitlabAlertMarker> = null;
		if (opener === 0) {
			const fence = OPENER.exec(firstLine);
			alert = fence && gitlabAlertMarker(firstLine.slice(fence[0].length));
			if (!fence || !alert) return undefined;
			opener = fence[1].length;
		}
		const bodyStart = firstEnd === -1 ? src.length : firstEnd + 1;
		let at = bodyStart;
		let bodyEnd = src.length;
		let rawEnd = src.length;
		let code: string | null = null;
		while (at < src.length) {
			const newline = src.indexOf('\n', at);
			const lineEnd = newline === -1 ? src.length : newline;
			const line = src.slice(at, lineEnd);
			const next = newline === -1 ? src.length : newline + 1;
			if (code !== null) {
				const run = CODE_FENCE.exec(line)?.[1];
				if (run && run[0] === code[0] && run.length >= code.length && line.trimEnd().endsWith(run)) code = null;
			} else if (fenceLength(line) >= opener) {
				bodyEnd = at;
				rawEnd = next;
				break;
			} else {
				code = CODE_FENCE.exec(line)?.[1] ?? null;
			}
			at = next;
		}
		const text = src.slice(bodyStart, bodyEnd);
		const top = this.lexer.state.top;
		this.lexer.state.top = true;
		const tokens = this.lexer.blockTokens(text, []);
		this.lexer.state.top = top;
		const raw = src.slice(0, rawEnd);
		if (alert) return alertToken(alert, raw, text, tokens);
		const quote: Tokens.Blockquote = { type: 'blockquote', raw, text, tokens };
		return quote;
	}
};
