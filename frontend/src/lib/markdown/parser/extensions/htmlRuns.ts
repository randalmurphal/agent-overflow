/**
 * Where an element that opens at a position closes, answered for a whole
 * tokenizing run at once.
 *
 * A details run, an HTML region and an inline element pair all ask, at
 * every candidate position, where the elements opening there close.
 * Answered by a search forward from each position, a comment of unclosed
 * or nested openers costs quadratic time. Instead the first question in a
 * run (one `blockTokens` or `inlineTokens` call) measures the rest of the
 * run once and answers every later position of it from that plan.
 *
 * Measuring lexes the rest of the run in a throwaway Lexer with the same
 * options, so what is an HTML block, code or an inline tag is the real
 * grammar's answer. In it these extensions claim no element: they record
 * the HTML blocks (block level) or the tags (inline level) the run itself
 * holds, and the plan pairs those tags on one stack.
 *
 * A plan is bound to the token array of the run it answers. A position's
 * key is its remaining length, which identifies it within the run. The
 * binding of a run this module did not start also checks that the run is
 * the one measured: a run preserves its token raws' total plus its
 * remaining length, and a later run reusing the array (a blockquote's
 * continuation) does not. The inside of an element this module claimed
 * (a details body, a region's inside, an inline element's content) is
 * bound to the same plan, shifted, on an array this module creates.
 */
import { Lexer } from '../engine';
import type { Token, Tokens } from '../engine';
import { OpenElements, scanTags, type HtmlTag } from './htmlTags';

/** A tag's span as plan keys: the remaining length at its `<` and past its `>`. */
type TagSpan = { start: number; end: number };

type Run<Plan> = {
	plan: Plan;
	/** A position's plan key is its remaining length plus this. */
	shift: number;
	/** The run's `blockDepth`; -1 for inline runs. */
	depth: number;
	/** The inside of a region: a region does not nest in its own run. */
	flat: boolean;
	/**
	 * For a run this module did not start: the token raws' total plus the
	 * remaining length, constant across one run. -1 otherwise.
	 */
	total: number;
	/** The leading tokens summed into `sum`; the last token can still grow. */
	counted: number;
	sum: number;
};

const bound = <Plan>(runs: WeakMap<Token[], Run<Plan>>, tokens: Token[], src: string, depth: number): Run<Plan> | undefined => {
	const run = runs.get(tokens);
	if (!run || run.depth !== depth) return undefined;
	if (run.total < 0) return run;
	const last = tokens.length - 1;
	while (run.counted < last) run.sum += tokens[run.counted++].raw.length;
	return run.sum + (last >= run.counted ? tokens[last].raw.length : 0) + src.length === run.total ? run : undefined;
};

const bindMeasured = <Plan>(runs: WeakMap<Token[], Run<Plan>>, tokens: Token[], src: string, depth: number, plan: Plan): Run<Plan> => {
	const counted = Math.max(0, tokens.length - 1);
	let sum = 0;
	for (let i = 0; i < counted; i++) sum += tokens[i].raw.length;
	const total = sum + (tokens.length > counted ? tokens[counted].raw.length : 0) + src.length;
	const run = { plan, shift: 0, depth, flat: false, total, counted, sum };
	runs.set(tokens, run);
	return run;
};

/** Binds `tokens`, a run over `src.slice(?, end)`, to `run`'s plan. */
const bindInside = <Plan>(runs: WeakMap<Token[], Run<Plan>>, tokens: Token[], run: Run<Plan>, src: string, end: number, depth: number, flat: boolean): void => {
	runs.set(tokens, { plan: run.plan, shift: src.length - end + run.shift, depth, flat, total: -1, counted: 0, sum: 0 });
};

/** The offset in `src` of the position whose plan key is `key`. */
const offsetOf = (run: Run<unknown>, src: string, key: number): number => src.length + run.shift - key;

/** `src` with CRLF and CR line endings as LF, as `Lexer.lex` reads it, and the map back. */
const normalizeLineEndings = (src: string): { text: string; toSource: (offset: number) => number } => {
	if (!src.includes('\r')) return { text: src, toSource: (offset) => offset };
	// Text offsets of each `\n` that was `\r\n`.
	const pairs: number[] = [];
	const text = src.replace(/\r\n?/g, (ending: string, at: number) => {
		if (ending.length === 2) pairs.push(at - pairs.length);
		return '\n';
	});
	return {
		text,
		toSource(offset) {
			let low = 0;
			let high = pairs.length;
			while (low < high) {
				const mid = (low + high) >> 1;
				if (pairs[mid] < offset) low = mid + 1;
				else high = mid;
			}
			return offset + low;
		}
	};
};

/**
 * A measuring Lexer writes no state back: its footnote tables are its own,
 * and so are its reference definitions at block level, where registering
 * one changes which definition tokens the real run keeps. Inline lexing
 * only reads them, so an inline measure reads the real table.
 */
const measuringLexer = (lexer: Lexer, readsLinks: boolean): Lexer => {
	const measurer = new Lexer(lexer.options);
	measurer.footnotes = { refs: new Map(), footnotes: new Map() };
	if (readsLinks) measurer.tokens.links = lexer.tokens.links;
	return measurer;
};

// ---------------------------------------------------------------------------
// Block runs
// ---------------------------------------------------------------------------

/**
 * Container elements a forge author wraps Markdown in, and `a`, which links
 * the blocks it wraps (a heading between `<a href>` and `</a>` lines).
 * Elements a following block closes implicitly (`p`, headings) and other
 * inline elements are not tracked: a forge would not wrap the Markdown
 * after them.
 */
const TRACKED = new Set([
	'a', 'div', 'center', 'blockquote', 'details', 'summary', 'figure', 'figcaption',
	'table', 'caption', 'thead', 'tbody', 'tfoot', 'tr', 'td', 'th',
	'ul', 'ol', 'li', 'dl', 'dt', 'dd'
]);
// Start tags whose end tag a tokenizer needs.
const RECORDED_CLOSES = new Set(['details', 'summary']);

type Opener = {
	/** The `<` starts the HTML block, so the engine's html token there is the block. */
	blockStart: boolean;
	/** Plan key past the HTML block holding the `<`. */
	blockEnd: number;
	/** Open elements below the ones opened here. */
	base: number;
	/** Plan key past the HTML block that closes them; -1 when none does. */
	end: number;
};

type BlockPlan = {
	/** By the plan key of a `<` after which an HTML block leaves elements open. */
	openers: Map<number, Opener>;
	/** By the plan key of a recorded start tag: its end tag. */
	closes: Map<number, TagSpan>;
	/** Plan keys past each tracked tag that changed the open elements, descending. */
	tagEnds: number[];
	/** The open element count after each of `tagEnds`. */
	heights: number[];
};

type BlockMeasure = { depth: number; text: string; blocks: number[] };
const blockMeasures = new WeakMap<Lexer, BlockMeasure>();
const blockRuns = new WeakMap<Token[], Run<BlockPlan>>();

export type BlockRun = Run<BlockPlan>;

/** Whether `lexer` is measuring a block run for this module. */
export const isMeasuringBlocks = (lexer: Lexer): boolean => blockMeasures.has(lexer);

/**
 * In a measuring Lexer: the html block at `src`, recorded when it is in the
 * measured run. Claiming it there leaves every block boundary as the
 * engine's html rule draws it.
 */
export const measuredHtmlBlock = (lexer: Lexer, src: string): Tokens.HTML | undefined => {
	const measure = blockMeasures.get(lexer);
	if (!measure || lexer.blockDepth !== measure.depth) return undefined;
	const html = lexer.tokenizer.html(src);
	if (html) {
		const start = measure.text.length - src.length;
		measure.blocks.push(start, start + html.raw.length);
	}
	return html;
};

/**
 * Pairs the tracked tags of the HTML blocks of a run on one stack. An end
 * tag closes the nearest open element of its name and every element opened
 * after it, as an HTML parser does; one with no open element of its name is
 * ignored. A `<` opens a region when its HTML block leaves elements opened
 * at or after it open; the region ends with the first HTML block after which
 * none of them is.
 */
const pairBlocks = (text: string, blocks: number[], toSource: (offset: number) => number, length: number): BlockPlan => {
	const keyOf = (offset: number) => length - toSource(offset);
	const plan: BlockPlan = { openers: new Map(), closes: new Map(), tagEnds: [], heights: [] };
	// Plan keys at each open element's `<`: descending up the stack.
	const open = new OpenElements<number>();
	// Openers not yet closed, bases ascending.
	const pending: Opener[] = [];
	for (let b = 0; b < blocks.length; b += 2) {
		const start = blocks[b];
		const html = text.slice(start, blocks[b + 1]);
		const starts: number[] = [];
		let first = 0;
		while (html.charCodeAt(first) === 32) first++;
		starts.push(keyOf(start + first));
		scanTags(html, (tag: HtmlTag) => {
			const key = keyOf(start + tag.start);
			if (key !== starts[starts.length - 1]) starts.push(key);
			if (!TRACKED.has(tag.name)) return;
			if (tag.closing) {
				const opened = open.close(tag.name);
				if (opened === undefined) return;
				if (RECORDED_CLOSES.has(tag.name)) plan.closes.set(opened, { start: key, end: keyOf(start + tag.end) });
			} else if (!tag.selfClosing) {
				open.push(tag.name, key);
			} else {
				return;
			}
			plan.tagEnds.push(keyOf(start + tag.end));
			plan.heights.push(open.height);
		});
		const blockEnd = keyOf(blocks[b + 1]);
		while (pending.length > 0 && pending[pending.length - 1].base >= open.height) pending.pop()!.end = blockEnd;
		for (const key of starts) {
			// The first open element whose `<` is at or after this one.
			let low = 0;
			let high = open.height;
			while (low < high) {
				const mid = (low + high) >> 1;
				if (open.valueAt(mid) > key) low = mid + 1;
				else high = mid;
			}
			if (low === open.height) continue;
			const opener: Opener = { blockStart: key === starts[0], blockEnd, base: low, end: -1 };
			plan.openers.set(key, opener);
			pending.push(opener);
		}
	}
	return plan;
};

const measureBlocks = (lexer: Lexer, src: string): BlockPlan => {
	const { text, toSource } = normalizeLineEndings(src);
	const measurer = measuringLexer(lexer, false);
	measurer.blockDepth = lexer.blockDepth - 1;
	measurer.align = lexer.align;
	measurer.state.top = lexer.state.top;
	const blocks: number[] = [];
	blockMeasures.set(measurer, { depth: lexer.blockDepth, text, blocks });
	measurer.blockTokens(text, []);
	return pairBlocks(text, blocks, toSource, src.length);
};

/** The plan answering the block run `tokens` is at `src` of; undefined before one is measured. */
export const boundBlockRun = (lexer: Lexer, src: string, tokens: Token[]): BlockRun | undefined =>
	bound(blockRuns, tokens, src, lexer.blockDepth);

/** Measures the rest of the block run from `src` and binds the plan. */
export const measureBlockRun = (lexer: Lexer, src: string, tokens: Token[]): BlockRun =>
	bindMeasured(blockRuns, tokens, src, lexer.blockDepth, measureBlocks(lexer, src));

/** Binds `tokens`, the nested block run over `src` up to `end`, to `run`. */
export const bindBlockInside = (lexer: Lexer, tokens: Token[], run: BlockRun, src: string, end: number, flat: boolean): void =>
	bindInside(blockRuns, tokens, run, src, end, lexer.blockDepth + 1, flat);

const htmlBodies = new WeakMap<Lexer, number>();

/**
 * `lexer.blockTokens` for the Markdown inside an HTML element (a details
 * body, a region's inside). A forge parses that Markdown at the level of
 * the HTML around it, so it adds no `markdownNesting`.
 */
export const htmlBodyTokens = (lexer: Lexer, src: string, tokens: Token[]): Token[] => {
	const bodies = htmlBodies.get(lexer) ?? 0;
	htmlBodies.set(lexer, bodies + 1);
	try {
		return lexer.blockTokens(src, tokens);
	} finally {
		htmlBodies.set(lexer, bodies);
	}
};

/** How many Markdown containers (block quotes, list items) the block being tokenized is inside. */
export const markdownNesting = (lexer: Lexer): number => lexer.blockDepth - 1 - (htmlBodies.get(lexer) ?? 0);

/** The end tag closing the recorded start tag at `src[at]`, as offsets into `src`, within the run. */
export const closeOf = (run: BlockRun, src: string, at: number): { start: number; end: number } | undefined => {
	const close = run.plan.closes.get(src.length - at + run.shift);
	if (!close || close.end < run.shift) return undefined;
	return { start: offsetOf(run, src, close.start), end: offsetOf(run, src, close.end) };
};

/** The open element count after the last tag that ends at or before the plan key `key`. */
const heightAt = (plan: BlockPlan, key: number): number => {
	let low = 0;
	let high = plan.tagEnds.length;
	while (low < high) {
		const mid = (low + high) >> 1;
		if (plan.tagEnds[mid] >= key) low = mid + 1;
		else high = mid;
	}
	return low === 0 ? 0 : plan.heights[low - 1];
};

/**
 * The region whose first `<` is `src[at]`, as offsets into `src`: the end
 * of its first HTML block and its own end. `blockStart` is false when the
 * `<` is inside an HTML block of the measured run, so the caller checks the
 * engine reads an HTML block there of the same length.
 */
export const regionAt = (run: BlockRun, src: string, at: number): { blockStart: boolean; first: number; end: number } | undefined => {
	if (run.flat) return undefined;
	const opener = run.plan.openers.get(src.length - at + run.shift);
	// A first block that reaches the end of the run has nothing to wrap.
	if (!opener || opener.blockEnd <= run.shift) return undefined;
	let end = opener.end;
	if (end < run.shift) {
		// The run ends inside a later HTML block (a details body ends at its
		// `</details>`): the region ends with the run if its elements have
		// closed by then.
		if (heightAt(run.plan, run.shift) > opener.base) return undefined;
		end = run.shift;
	}
	return { blockStart: opener.blockStart, first: offsetOf(run, src, opener.blockEnd), end: offsetOf(run, src, end) };
};

/** Whether the html block at `src` leaves a tracked element open, counted on its own. */
export const htmlBlockOpens = (lexer: Lexer, src: string): boolean => {
	const html = lexer.tokenizer.html(src);
	if (!html) return false;
	// The engine does not read CRLF blank lines: only a measured run sees
	// where this block ends.
	if (html.raw.includes('\r')) return true;
	const open = new OpenElements<null>();
	scanTags(html.raw, (tag) => {
		if (!TRACKED.has(tag.name)) return;
		if (tag.closing) open.close(tag.name);
		else if (!tag.selfClosing) open.push(tag.name, null);
	});
	return open.height > 0;
};

// ---------------------------------------------------------------------------
// Inline runs
// ---------------------------------------------------------------------------

type InlinePlan = {
	/** By the plan key of a start tag: the end tag that closes it. */
	pairs: Map<number, TagSpan>;
};

type InlineMeasure = { tokens: Token[]; tags: { key: number; tag: HtmlTag }[]; names: ReadonlySet<string> };
const inlineMeasures = new WeakMap<Lexer, InlineMeasure>();
const inlineRuns = new WeakMap<Token[], Run<InlinePlan>>();

export type InlineRun = Run<InlinePlan>;

/** Whether `lexer` is measuring an inline run for this module. */
export const isMeasuringInline = (lexer: Lexer): boolean => inlineMeasures.has(lexer);

/** In a measuring Lexer: records `tag`, read at `src`, when `tokens` is the measured run. */
export const recordInlineTag = (lexer: Lexer, src: string, tokens: Token[], tag: HtmlTag): void => {
	const measure = inlineMeasures.get(lexer);
	if (measure && tokens === measure.tokens && measure.names.has(tag.name)) measure.tags.push({ key: src.length, tag });
};

/**
 * Pairs the tags of `names` an inline run holds, by name: an end tag closes
 * the latest unclosed start tag of its name.
 */
const measureInline = (lexer: Lexer, src: string, names: ReadonlySet<string>): InlinePlan => {
	const measurer = measuringLexer(lexer, true);
	measurer.state.inLink = lexer.state.inLink;
	measurer.state.inLinkText = lexer.state.inLinkText;
	measurer.state.inRawBlock = lexer.state.inRawBlock;
	const tokens: Token[] = [];
	const measure: InlineMeasure = { tokens, tags: [], names };
	inlineMeasures.set(measurer, measure);
	measurer.inlineTokens(src, tokens);
	const pairs = new Map<number, TagSpan>();
	const open = new Map<string, number[]>();
	for (const { key, tag } of measure.tags) {
		let stack = open.get(tag.name);
		if (!tag.closing) {
			if (!stack) open.set(tag.name, (stack = []));
			stack.push(key);
			continue;
		}
		const start = stack?.pop();
		if (start !== undefined) pairs.set(start, { start: key, end: key - (tag.end - tag.start) });
	}
	return { pairs };
};

/** The plan answering the inline run `tokens` is at `src` of, measured on first use. */
export const inlineRun = (lexer: Lexer, src: string, tokens: Token[], names: ReadonlySet<string>): InlineRun =>
	bound(inlineRuns, tokens, src, -1) ?? bindMeasured(inlineRuns, tokens, src, -1, measureInline(lexer, src, names));

/** Binds `tokens`, the nested inline run over `src` up to `end`, to `run`. */
export const bindInlineInside = (tokens: Token[], run: InlineRun, src: string, end: number): void =>
	bindInside(inlineRuns, tokens, run, src, end, -1, false);

/** The end tag closing the start tag at the start of `src`, as offsets into `src`, within the run. */
export const pairOf = (run: InlineRun, src: string): { start: number; end: number } | undefined => {
	const close = run.plan.pairs.get(src.length + run.shift);
	if (!close || close.end < run.shift) return undefined;
	return { start: offsetOf(run, src, close.start), end: offsetOf(run, src, close.end) };
};
