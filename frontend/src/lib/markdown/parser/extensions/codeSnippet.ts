/**
 * A titled excerpt of a file: a header (a paragraph of inline tokens, such
 * as a forge's file and commit links) over the lines, a code block numbered
 * from its first line (`Tokens.Code.firstLine`). Both renderers draw it
 * from these children, so its code is any code block to the code host, the
 * static path and find, and its header any paragraph.
 *
 * It is written inline (a bare permalink in a sentence) and stands as a
 * block. A host's inline tokenizer returns the token it reads as anywhere
 * else (a link) with the snippet as `liftsTo` (`liftToBlock`); the finished
 * lex (`liftInlineBlocks`) splits each paragraph, and each tight list
 * item's text, whose own run holds one, into the text before it, the
 * snippet and the text after. Inside emphasis, link text, a heading, a
 * table cell or an HTML region the inline token stays what it is. Each
 * piece's `raw` is its slice of the token it was split from, so the raws
 * of a container's children still sum to its source.
 */
import type { Lexer, Token, Tokens } from '../engine';

export type CodeSnippetToken = {
	type: 'codeSnippet';
	/** The source it was written as. */
	raw: string;
	tokens: [Tokens.Paragraph, Tokens.Code];
};

type Lifting = Token & { liftsTo?: CodeSnippetToken };

/** `inline`, standing as `block` where it sits alone in a paragraph's run. */
export const liftToBlock = <T extends Token>(lexer: Lexer, inline: T, block: CodeSnippetToken): T => {
	lexer.lifts = true;
	return { ...inline, liftsTo: block };
};

/** Containers whose children are blocks a lifted token may split. */
const BLOCK_CONTAINERS: ReadonlySet<string> = new Set(['blockquote', 'list', 'list_item', 'alert', 'details']);

type RunToken = Tokens.Paragraph | (Tokens.Text & { tokens: Token[] });

/**
 * Splits every paragraph (and tight item text) of `tokens` whose own
 * inline run holds a lifted token around it, in place. Runs only after a
 * lex that set `Lexer.lifts`.
 */
export const liftInlineBlocks = (tokens: Token[]): void => {
	for (let index = 0; index < tokens.length; index++) {
		const token = tokens[index];
		if (token.type === 'paragraph' || (token.type === 'text' && Array.isArray((token as Tokens.Text).tokens))) {
			const pieces = split(token as RunToken);
			if (pieces) {
				tokens.splice(index, 1, ...pieces);
				index += pieces.length - 1;
			}
			continue;
		}
		const children = (token as { tokens?: unknown }).tokens;
		if (BLOCK_CONTAINERS.has(token.type) && Array.isArray(children)) liftInlineBlocks(children as Token[]);
	}
};

const blankText = (token: Token): boolean => token.type === 'br' || (token.type === 'text' && !(token as Tokens.Text).tokens && (token as Tokens.Text).text.trim() === '');

/** `run` without the line breaks and spaces at its edges, which would show
 * as a blank line or an indent beside the block. */
const trimRun = (run: Token[]): Token[] => {
	let start = 0;
	let end = run.length;
	while (start < end && blankText(run[start])) start++;
	while (end > start && blankText(run[end - 1])) end--;
	if (start === end) return [];
	const out = run.slice(start, end);
	const first = out[0] as Tokens.Text;
	if (first.type === 'text' && !first.tokens && first.text !== first.text.trimStart()) out[0] = { ...first, text: first.text.trimStart() };
	const last = out[out.length - 1] as Tokens.Text;
	if (last.type === 'text' && !last.tokens && last.text !== last.text.trimEnd()) out[out.length - 1] = { ...last, text: last.text.trimEnd() };
	return out;
};

const split = (token: RunToken): Token[] | null => {
	const children = token.tokens as Lifting[];
	let lifted = false;
	let length = 0;
	for (const child of children) {
		if (child.liftsTo) lifted = true;
		length += child.raw.length;
	}
	// The run's raws must be its text, and its text the start of its raw,
	// for the slices to be its source.
	if (!lifted || length !== token.text.length || token.raw.slice(0, length) !== token.text) return null;
	const pieces: Token[] = [];
	const pushRun = (run: Token[], raw: string) => {
		if (raw === '') return;
		const kept = trimRun(run);
		if (kept.length === 0) {
			pieces.push({ type: 'space', raw } as Tokens.Space);
			return;
		}
		const text = token.type === 'paragraph' ? raw.replace(/\n+$/, '') : raw;
		pieces.push({ ...token, raw, text, tokens: kept } as Token);
	};
	let runStart = 0;
	let rawStart = 0;
	let at = 0;
	for (let index = 0; index < children.length; index++) {
		const child = children[index];
		if (child.liftsTo) {
			pushRun(children.slice(runStart, index), token.raw.slice(rawStart, at));
			pieces.push({ ...child.liftsTo, raw: child.raw });
			runStart = index + 1;
			rawStart = at + child.raw.length;
		}
		at += child.raw.length;
	}
	pushRun(children.slice(runStart), token.raw.slice(rawStart));
	return pieces;
};
