/**
 * GFM strikethrough: `~text~` or `~~text~~` renders struck through, as GitHub
 * and GitLab render it. Opt-in: agent chat reads `~text~` as subscript and
 * strikes only `~~text~~` (the engine `del` rule), so prose like `~5s` is
 * never struck.
 *
 * Both forges pair tildes by CommonMark's delimiter-run rules
 * (cmark-gfm's strikethrough extension, comrak on GitLab). A run of one or
 * two tildes opens when it is left-flanking and closes when it is
 * right-flanking; a run of three or more is text. Each closer, left to
 * right, takes the nearest opener before it, skipping one whose lengths
 * sum to a multiple of three when either could also be the other (the
 * "rule of 3"); the pair strikes the text between when their lengths are
 * equal and leaves both as text when not, and any run between them is
 * text. So `~bug ~feature` strikes nothing, and in
 * `~"a", ~"b", x~y, z~w` the third tilde closes the fourth, the second
 * the first. Code spans, links and HTML bind tighter: a tilde inside one
 * is not a delimiter (`Lexer.maskInline`).
 *
 * The tildes of a whole inline run are paired in one left-to-right pass,
 * cmark-gfm's, made once per run (`Lexer.inlineRun`); the tokenizer looks
 * its position up. It runs ahead of the default extensions, so subscript,
 * which a forge does not have, never claims a tilde.
 */
import type { Token, Tokens, TokensList } from '../engine';
import type { InlineRun } from '../engine/Lexer';
import type { Extension } from '../lexer';

const TILDE = 126;
// CommonMark's Unicode whitespace and punctuation (P and S categories).
const WHITESPACE = /[\t\n\f\r\p{Zs}]/u;
const PUNCTUATION = /[\p{P}\p{S}]/u;

interface Run {
	readonly at: number;
	readonly length: number;
	readonly opens: boolean;
	readonly closes: boolean;
}

/** The character before `at` in `src`, or `before` at its start. */
const charBefore = (src: string, at: number, before: string): string => {
	if (at === 0) return before;
	const low = src.charCodeAt(at - 1);
	return low >= 0xdc00 && low <= 0xdfff && at >= 2 ? src.slice(at - 2, at) : src[at - 1];
};

/** The character after `end`, a line end past the run's end. */
const charAfter = (src: string, end: number): string =>
	end >= src.length ? '\n' : String.fromCodePoint(src.codePointAt(end)!);

/** The tilde run at `at`, whose length `masked` gives. */
const tildeRun = (src: string, masked: string, at: number, before: string): Run => {
	let end = at + 1;
	while (masked.charCodeAt(end) === TILDE) end++;
	const prev = charBefore(src, at, before);
	const next = charAfter(src, end);
	const prevSpace = WHITESPACE.test(prev);
	const nextSpace = WHITESPACE.test(next);
	const prevPunct = PUNCTUATION.test(prev);
	const nextPunct = PUNCTUATION.test(next);
	return {
		at,
		length: end - at,
		opens: !nextSpace && (!nextPunct || prevSpace || prevPunct),
		closes: !prevSpace && (!prevPunct || nextSpace || nextPunct)
	};
};

/** Whether `closer` passes over `opener` by the rule of 3. */
const oddMatch = (opener: Run, closer: Run): boolean =>
	(closer.opens || opener.closes) && (opener.length + closer.length) % 3 === 0 && !(opener.length % 3 === 0 && closer.length % 3 === 0);

/** The character the run's tokens so far end with; a line start at none. */
const lastChar = (tokens: Token[] | TokensList): string => {
	const raw = tokens[tokens.length - 1]?.raw;
	return raw ? charBefore(raw, raw.length, '\n') : '\n';
};

/**
 * The strikes of `run`: each opening run's offset to its closing run's
 * offset. A run of one or two tildes that closes takes the nearest opener
 * on the stack it does not pass over by the rule of 3; that opener, those
 * above it and the closer leave the stack, and the pair strikes when their
 * lengths are equal. A run that finds none is pushed when it opens.
 * `bottom` holds, per closer kind (length, and whether it also opens), the
 * stack height below which that kind's last search found nothing, so the
 * pass is linear in the run.
 */
const strikesOf = (run: InlineRun, before: string): Map<number, number> => {
	const { src, masked } = run;
	const strikes = new Map<number, number>();
	const stack: Run[] = [];
	const bottom = [0, 0, 0, 0];
	for (let at = masked.indexOf('~'); at !== -1; ) {
		const tilde = tildeRun(src, masked, at, before);
		at = masked.indexOf('~', at + tilde.length);
		if (tilde.length > 2) continue;
		if (tilde.closes) {
			const kind = (tilde.length - 1) * 2 + (tilde.opens ? 1 : 0);
			let index = stack.length - 1;
			while (index >= bottom[kind] && oddMatch(stack[index], tilde)) index--;
			if (index >= bottom[kind]) {
				if (stack[index].length === tilde.length) strikes.set(stack[index].at, tilde.at);
				stack.length = index;
				for (let k = 0; k < bottom.length; k++) bottom[k] = Math.min(bottom[k], index);
				continue;
			}
			bottom[kind] = stack.length;
		}
		if (tilde.opens) stack.push(tilde);
	}
	return strikes;
};

/** Each inline run's strikes, made at the tokenizer's first call in it. */
const runStrikes = new WeakMap<InlineRun, Map<number, number>>();

export const markedStrikethrough: Extension = {
	name: 'strikethrough',
	level: 'inline',
	precedesDefaults: true,
	start(src) {
		const index = src.indexOf('~');
		return index === -1 ? undefined : index;
	},
	tokenizer(src, tokens) {
		if (src.charCodeAt(0) !== TILDE) return undefined;
		const run = this.lexer.inlineRun;
		if (!run || run.src.length - run.at !== src.length) return undefined;
		let strikes = runStrikes.get(run);
		if (!strikes) {
			// Before the run's start: what its tokens so far end with, read
			// when the tokenizer is at that start.
			strikes = strikesOf(run, run.at === 0 ? lastChar(tokens) : '\n');
			runStrikes.set(run, strikes);
		}
		let length = 1;
		while (src.charCodeAt(length) === TILDE) length++;
		const close = strikes.get(run.at);
		if (close === undefined) {
			const text: Tokens.Text = { type: 'text', raw: src.slice(0, length), text: src.slice(0, length) };
			return text;
		}
		const end = close - run.at;
		const text = src.slice(length, end);
		const del: Tokens.Del = {
			type: 'del',
			raw: src.slice(0, end + length),
			text,
			tokens: this.lexer.inlineTokens(text)
		};
		return del;
	}
};
