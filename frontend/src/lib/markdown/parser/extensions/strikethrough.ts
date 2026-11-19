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
 * The tildes of a whole inline run are paired in one left-to-right pass
 * of CommonMark's delimiter algorithm (cmark's `process_emphasis`) over its
 * `*`, `_` and `~` runs together, made once per run (`Lexer.inlineRun`);
 * the tokenizer looks its position up. A match removes every delimiter
 * between the pair, so a tilde inside emphasis that closes first is no
 * delimiter, nor emphasis inside a strike that closes first. The pass
 * predicts emphasis by the specification's algorithm only to decide which
 * tildes strike; the engine's `emStrong` still builds the emphasis. A tilde
 * inside a token another rule took (math, an extension) is no delimiter
 * either: finding one pairs the rest again. It runs ahead of the default
 * extensions, so subscript, which a forge does not have, never claims a
 * tilde.
 */
import type { Token, Tokens, TokensList } from '../engine';
import type { InlineRun } from '../engine/Lexer';
import type { Extension } from '../lexer';

const TILDE = 126;
const STAR = 42;
const UNDERSCORE = 95;
// CommonMark's Unicode whitespace and punctuation (P and S categories).
const WHITESPACE = /[\t\n\f\r\p{Zs}]/u;
const PUNCTUATION = /[\p{P}\p{S}]/u;

/** A delimiter run: `~`, `*` or `_` (`char`), read in the mask. */
interface Run {
	readonly char: number;
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

/** The run of `masked`'s character at `at`, flanked by `src`'s characters. */
const delimiterRun = (src: string, masked: string, at: number, before: string): Run => {
	const char = masked.charCodeAt(at);
	let end = at + 1;
	while (masked.charCodeAt(end) === char) end++;
	const prev = charBefore(src, at, before);
	const next = charAfter(src, end);
	const prevSpace = WHITESPACE.test(prev);
	const nextSpace = WHITESPACE.test(next);
	const prevPunct = PUNCTUATION.test(prev);
	const nextPunct = PUNCTUATION.test(next);
	const left = !nextSpace && (!nextPunct || prevSpace || prevPunct);
	const right = !prevSpace && (!prevPunct || nextSpace || nextPunct);
	const length = end - at;
	// An underscore opens or closes inside a word only beside punctuation.
	if (char === UNDERSCORE) return { char, at, length, opens: left && (!right || prevPunct), closes: right && (!left || nextPunct) };
	return { char, at, length, opens: left, closes: right };
};

/** Whether `closer` passes over `opener` by the rule of 3. */
const oddMatch = (opener: Run, closer: Run): boolean =>
	(closer.opens || opener.closes) && (opener.length + closer.length) % 3 === 0 && !(opener.length % 3 === 0 && closer.length % 3 === 0);

/** The character the run's tokens so far end with; a line start at none. */
const lastChar = (tokens: Token[] | TokensList): string => {
	const raw = tokens[tokens.length - 1]?.raw;
	return raw ? charBefore(raw, raw.length, '\n') : '\n';
};

/** Whether `run` can open or close: a tilde run longer than two is text. */
const delimits = (run: Run): boolean => (run.char !== TILDE || run.length <= 2) && (run.opens || run.closes);

/** A closer's kind for `openers_bottom`: its character, whether it also
 * opens, and its length mod 3, the closer's part in every match test. */
const kindOf = (closer: Run): number =>
	(closer.char === TILDE ? 0 : closer.char === STAR ? 6 : 12) + (closer.opens ? 3 : 0) + (closer.length % 3);
const KINDS = 18;

/** An opener on the stack, with the delimiters matches have left it. */
type Opener = { readonly run: Run; count: number };

/** A pass's tilde pairs: strikes by opener offset, and every tilde run a
 * pair took, struck or not. */
type Pairs = { readonly strikes: Map<number, number>; readonly paired: Set<Run> };

/**
 * CommonMark's delimiter algorithm over the runs not `dead`. Closers are
 * taken left to right; each looks down the stack for the nearest opener of
 * its character the rule of 3 does not pass over, and a match removes every
 * delimiter between the two. Emphasis takes two delimiters from each side
 * when both have two, else one, and what remains stays; a tilde pair strikes
 * when its lengths are equal and leaves the stack either way. A closer that
 * finds none is pushed when it opens. `bottom` holds, per closer kind, the
 * stack height below which its last failed search found nothing, so the
 * pass is linear in the runs.
 */
const pairsOf = (runs: readonly Run[], dead: ReadonlySet<Run>): Pairs => {
	const strikes = new Map<number, number>();
	const paired = new Set<Run>();
	const stack: Opener[] = [];
	const bottom: number[] = new Array(KINDS).fill(0);
	const truncate = (height: number) => {
		stack.length = height;
		for (let kind = 0; kind < KINDS; kind++) if (bottom[kind] > height) bottom[kind] = height;
	};
	for (const run of runs) {
		if (!delimits(run) || dead.has(run)) continue;
		let count = run.length;
		if (run.closes) {
			const kind = kindOf(run);
			while (count > 0) {
				let index = stack.length - 1;
				while (index >= bottom[kind] && (stack[index].run.char !== run.char || oddMatch(stack[index].run, run))) index--;
				if (index < bottom[kind]) {
					bottom[kind] = stack.length;
					break;
				}
				const opener = stack[index];
				if (run.char === TILDE) {
					if (opener.run.length === run.length) strikes.set(opener.run.at, run.at);
					paired.add(opener.run).add(run);
					truncate(index);
					count = 0;
					break;
				}
				const used = count >= 2 && opener.count >= 2 ? 2 : 1;
				opener.count -= used;
				count -= used;
				truncate(opener.count > 0 ? index + 1 : index);
			}
		}
		if (count > 0 && run.opens) stack.push({ run, count });
	}
	return { strikes, paired };
};

/** What the tokenizer knows of one inline run's delimiters. */
interface Pairing {
	/** Every delimiter run of the mask, in order. */
	readonly runs: readonly Run[];
	/** Tilde runs the tokenizer was never called at: another token holds
	 * them, so they are not delimiters of this run. */
	readonly dead: Set<Run>;
	pairs: Pairs;
	/** The index in `runs` of the first run the tokenizer has not passed. */
	next: number;
}

/** The delimiter runs of `run`, with `before` the character before its start. */
const runsOf = (run: InlineRun, before: string): Run[] => {
	const { src, masked } = run;
	const runs: Run[] = [];
	for (let at = 0; at < masked.length; ) {
		const char = masked.charCodeAt(at);
		if (char !== TILDE && char !== STAR && char !== UNDERSCORE) {
			at++;
			continue;
		}
		const delimiter = delimiterRun(src, masked, at, before);
		runs.push(delimiter);
		at += delimiter.length;
	}
	return runs;
};

/**
 * Advances `pairing` to a call at offset `at` and returns the tilde run that
 * starts there, or undefined. The tokenizer is called at every tilde run it
 * can see, in order, so one passed without a call is dead; a dead run a pair
 * took pairs the live runs again (a run no pair took changes no pair). Each
 * pass is linear; only an input alternating paired tildes another token
 * holds with live ones pairs more than once.
 */
const runAt = (pairing: Pairing, at: number): Run | undefined => {
	const { runs } = pairing;
	let died = false;
	while (pairing.next < runs.length && runs[pairing.next].at < at) {
		const run = runs[pairing.next++];
		if (run.char !== TILDE) continue;
		pairing.dead.add(run);
		if (pairing.pairs.paired.has(run)) died = true;
	}
	if (died) pairing.pairs = pairsOf(runs, pairing.dead);
	const run = runs[pairing.next];
	if (run?.at !== at || run.char !== TILDE) return undefined;
	pairing.next++;
	return run;
};

/** Each inline run's pairing, made at the tokenizer's first call in it. */
const pairings = new WeakMap<InlineRun, Pairing>();

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
		let pairing = pairings.get(run);
		if (!pairing) {
			// Before the run's start: what its tokens so far end with, read
			// when the tokenizer is at that start.
			const runs = runsOf(run, run.at === 0 ? lastChar(tokens) : '\n');
			const dead = new Set<Run>();
			pairing = { runs, dead, pairs: pairsOf(runs, dead), next: 0 };
			pairings.set(run, pairing);
		}
		let length = 1;
		while (src.charCodeAt(length) === TILDE) length++;
		const opener = runAt(pairing, run.at);
		const close = opener && pairing.pairs.strikes.get(opener.at);
		if (close === undefined) {
			const text: Tokens.Text = { type: 'text', raw: src.slice(0, length), text: src.slice(0, length) };
			return text;
		}
		// The runs inside the strike are its own run's.
		while (pairing.next < pairing.runs.length && pairing.runs[pairing.next].at <= close) pairing.next++;
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
