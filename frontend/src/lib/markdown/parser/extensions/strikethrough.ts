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
 * A tokenizer sees one opener at a time, so it replays the rules from
 * there to the end of the run: the opener strikes up to the closer that
 * takes it, or is text. Openers before it are already decided and change
 * nothing after it. It runs ahead of the default extensions, so
 * subscript, which a forge does not have, never claims a tilde.
 */
import type { Token, Tokens, TokensList } from '../engine';
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
 * Where the opener at 0 closes: the index of the closing run, or -1 when
 * it strikes nothing.
 */
const closerOf = (src: string, masked: string, opener: Run, before: string): number => {
	const openers: Run[] = [opener];
	let at = opener.length;
	for (;;) {
		at = masked.indexOf('~', at);
		if (at === -1) return -1;
		const run = tildeRun(src, masked, at, before);
		at += run.length;
		if (run.length > 2) continue;
		if (run.closes) {
			let index = openers.length - 1;
			while (index >= 0 && oddMatch(openers[index], run)) index--;
			if (index === 0) return openers[0].length === run.length ? run.at : -1;
			if (index > 0) {
				openers.length = index;
				continue;
			}
		}
		if (run.opens) openers.push(run);
	}
};

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
		const before = lastChar(tokens);
		const opener = tildeRun(src, src, 0, before);
		const close = opener.length <= 2 && opener.opens ? closerOf(src, this.lexer.maskInline(src), opener, before) : -1;
		if (close === -1) {
			const run: Tokens.Text = { type: 'text', raw: src.slice(0, opener.length), text: src.slice(0, opener.length) };
			return run;
		}
		const text = src.slice(opener.length, close);
		const del: Tokens.Del = {
			type: 'del',
			raw: src.slice(0, close + opener.length),
			text,
			tokens: this.lexer.inlineTokens(text)
		};
		return del;
	}
};
