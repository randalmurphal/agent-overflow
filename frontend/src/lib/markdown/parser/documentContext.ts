/**
 * Document-level facts for a document rendered block by block.
 *
 * Each block a `parseBlocks` cache holds is read, when its source changes,
 * for what other blocks need from it: reference definitions (in any
 * container, never inside code, as the grammar decides), which register
 * during block tokenization, and, on a forge surface, footnote definitions
 * and references, headings and GitLab's table of contents markers. Those
 * come from a lex under the document's folded definitions, as the block
 * renders, and are read again when a definition that lex read resolves
 * differently. The fold over those facts is a `DocumentContext`: one
 * `DocumentScope` per rendered block (documentScope.ts), handed to that
 * block's lex. An update that changes no fact returns the previous context,
 * so nothing downstream re-lexes; when the parse's changed tail
 * (`ParseBlocksCache.changedFrom`) holds no fact and held none, that costs
 * only the tail.
 *
 * Forge rules, from each forge's render: a heading's anchor is its text
 * lowercased with everything but letters, marks, numbers, `_`, `-` and spaces
 * removed and spaces turned into hyphens, a repeat taking the next free
 * `-1`, `-2` (GitLab reads an image's alt text, GitHub does not). Footnotes
 * are numbered by first reference among the labels that have a definition;
 * the section lists them in that order with one back-reference per
 * reference, and an unused definition is not listed. GitLab's table of
 * contents lists every heading, nested under the nearest earlier heading of
 * a lower level, at the last marker; every other marker shows nothing.
 */
import { Lexer } from './engine';
import type { Links, Token } from './engine';
import { getLexOptions, lex } from './lexer';
import type { Extension } from './lexer';
import type { ParseBlocksCache } from './parseBlocks.cache';
import { forEachBlockToken, installDocumentScope, isTocMarker, sameScopeReads } from './documentScope';
import type { DocumentForge, DocumentScope, DocumentToc, ForgeFactReader } from './documentScope';
import { footnoteRefAnchor } from './extensions/footnotes';
import type { Footnote } from './extensions/footnotes';

export interface DocumentContext {
	readonly forge: DocumentForge;
	/** The scope of rendered block `index` (a `parseBlocks` block index). */
	scope(index: number): DocumentScope;
	/** The context for content rendered after the document: definitions
	 * and numbers, nothing positional. */
	readonly after: DocumentContext;
	/** The forge's footnotes section as Markdown, or null when no footnote
	 * is referenced. Rendered under `after`. */
	readonly footnoteSection: string | null;
}

type Heading = { readonly slug: string; readonly text: string; readonly depth: number };

type Def = readonly [string, Links[string]];

/** What a forge lex of one block under the document's definitions found. */
interface InlineFacts {
	readonly footnotes: readonly [string, readonly string[]][];
	readonly refs: readonly string[];
	readonly headings: readonly Heading[];
	readonly markers: number;
	/** A scope its read keys resolve under as they did when it lexed, and
	 * those keys. Rebased onto each fold's scope it still stands under, so
	 * only the current definitions stay alive. */
	scope: DocumentScope;
	readonly reads: ReadonlySet<string>;
}

/** What one raw holds for the rest of its document. */
interface BlockFacts {
	readonly defs: readonly Def[];
	/** On a forge, its lex under the folded definitions; null until made. */
	inline: InlineFacts | null;
}

export interface DocumentContextCache {
	extensions: Extension[] | null;
	forge: DocumentForge | undefined;
	raws: string[];
	facts: (BlockFacts | null)[];
	key: string;
	/** The raw index of the document's first block (`firstRawIndex`). */
	lead: number;
	/** The rendered block index of `lead`, or -1. */
	leadBlockIndex: number;
	context: DocumentContext | null;
	/** The block parse, and its `generation`, that `raws` were last read from. */
	blocks: ParseBlocksCache | null;
	generation: number;
	/** The scopes a block's forge lex runs under (`first` false, true): the
	 * folded definitions of `context`. */
	factScopes: readonly [DocumentScope, DocumentScope] | null;
	/** `context` under the host's facts last asked for, and those facts. */
	withFacts: { readonly base: DocumentContext; readonly facts: ForgeFactReader; readonly context: DocumentContext } | null;
}

export const createDocumentContextCache = (): DocumentContextCache => ({
	extensions: null,
	forge: undefined,
	raws: [],
	facts: [],
	key: '',
	lead: -1,
	leadBlockIndex: -1,
	context: null,
	blocks: null,
	generation: 0,
	factScopes: null,
	withFacts: null
});

const FORGE_GATE = /\]:|\[\^|#|\[toc\]|\[\[_toc_\]\]|\n[ \t>]*[=-]/i;
// Chat needs reference definitions only, and every one holds `]:`.
const mayHoldFacts = (source: string, forge: DocumentForge): boolean =>
	forge ? FORGE_GATE.test(source) : source.includes(']:');

/** Whether `source` can hold a fact a `DocumentContext` folds: a cheap
 * over-approximation that lets a document without one skip the block parse. */
export const mayNeedDocumentContext = (source: string, forge: DocumentForge): boolean =>
	forge ? FORGE_GATE.test(source) || source.includes('](#') || /href=["']?#/i.test(source) : source.includes(']:');

type TextToken = Token & { text?: string; tokens?: TextToken[] };

/** A heading's text: `alt` reads images' alt text, `source` reads a token
 * this module does not know by what it was written as. */
const headingText = (tokens: readonly TextToken[] | undefined, alt: boolean, source: boolean): string => {
	let out = '';
	for (const token of tokens ?? []) {
		switch (token.type) {
			case 'text':
				out += token.tokens && token.tokens.length > 0 ? headingText(token.tokens, alt, source) : token.text ?? '';
				break;
			// An emoji shortcode is an escape token showing its glyph; the
			// anchor reads the shortcode as written.
			case 'escape':
				out += (source && token.raw.charCodeAt(0) === 58 /* : */ ? token.raw : token.text) ?? '';
				break;
			case 'codespan':
				out += token.text ?? '';
				break;
			case 'image':
				if (alt) out += token.text ?? '';
				break;
			case 'html':
			case 'br':
			case 'footnoteRef':
				break;
			default:
				if (Array.isArray(token.tokens)) out += headingText(token.tokens, alt, source);
				else out += (source ? token.raw : token.text) ?? '';
		}
	}
	return out;
};

/** The base anchor of a heading with `text`, before repeats are numbered. */
export const headingSlug = (text: string): string =>
	text.trim().toLowerCase().replace(/[^\p{L}\p{M}\p{N}\p{Pc}\- ]/gu, '').replace(/ /g, '-');

/** The index of the first raw that holds more than blank lines, or -1. */
const firstRawIndex = (raws: readonly string[]): number => raws.findIndex((raw) => raw.trim() !== '');

const NO_DEFS: readonly Def[] = [];

/** A block's facts with its definitions read; null when it can hold none. */
const factsOf = (raw: string, extensions: Extension[], forge: DocumentForge, first: boolean): BlockFacts | null => {
	if (!mayHoldFacts(raw, forge)) return null;
	if (!raw.includes(']:')) return { defs: NO_DEFS, inline: null };
	const lexer = new Lexer(getLexOptions(extensions));
	if (forge) {
		// Its own footnote maps, never the ambient Streamdown context's.
		lexer.footnotes = { refs: new Map(), footnotes: new Map() };
		installDocumentScope(lexer, scopeOf(forge, null, null, first));
	}
	// Definitions register during block tokenization.
	lexer.blockTokens(raw.replace(/\r\n?/g, '\n'), []);
	const defs: Def[] = [];
	for (const label in lexer.tokens.links) defs.push([label, lexer.tokens.links[label]]);
	return { defs: defs.length > 0 ? defs : NO_DEFS, inline: null };
};

/** A forge block's facts read from its lex under `scope`, which holds the
 * document's definitions, as the block renders. */
const inlineFactsOf = (raw: string, extensions: Extension[], forge: DocumentForge, scope: DocumentScope): InlineFacts => {
	const lexer = new Lexer(getLexOptions(extensions));
	lexer.footnotes = { refs: new Map(), footnotes: new Map() };
	// The forge's grammar, with every footnote reference read as one.
	installDocumentScope(lexer, scope);
	const tokens = lexer.lex(raw);
	const footnotes: [string, readonly string[]][] = [];
	const headings: Heading[] = [];
	let markers = 0;
	forEachBlockToken(tokens, (token) => {
		if (token.type === 'footnote') footnotes.push([(token as unknown as Footnote).label, (token as unknown as Footnote).lines]);
		else if (token.type === 'heading') {
			const children = (token as TextToken).tokens;
			headings.push({
				slug: headingSlug(headingText(children, forge === 'gitlab', true)),
				text: headingText(children, false, false).trim(),
				depth: (token as Token & { depth: number }).depth
			});
		}
		else if (isTocMarker(token)) markers++;
	});
	const refs: string[] = [];
	if (raw.includes('[^')) {
		const walk = (list: readonly TextToken[]) => {
			for (const token of list) {
				if (token.type === 'footnoteRef') refs.push((token as Token & { label: string }).label);
				else if (Array.isArray(token.tokens)) walk(token.tokens);
			}
		};
		walk(tokens as TextToken[]);
	}
	return { footnotes, refs, headings, markers, scope, reads: lexer.documentReads ?? new Set() };
};

const holdsFact = (facts: BlockFacts | null | undefined): boolean => {
	if (!facts) return false;
	const inline = facts.inline;
	return facts.defs.length > 0 ||
		(inline !== null && (inline.footnotes.length > 0 || inline.refs.length > 0 || inline.headings.length > 0 || inline.markers > 0));
};

const scopeOf = (
	forge: DocumentForge,
	links: Links | null,
	footnotes: ReadonlyMap<string, number> | null,
	first = false
): DocumentScope => ({ forge, links, footnotes, footnoteBase: null, headingIds: null, toc: null, first });

/** `scope` for every block; with `leads`, block 0 opens the document. */
const fixedContext = (scope: DocumentScope, footnoteSection: string | null, leads: boolean): DocumentContext => {
	const first = leads ? { ...scope, first: true } : scope;
	const context: DocumentContext = {
		forge: scope.forge,
		scope: (index) => (index === 0 ? first : scope),
		get after() {
			return context;
		},
		footnoteSection
	};
	return context;
};

// A document with nothing to share still lexes under a scope, so each
// block records what it looked up and only those blocks rebuild when a
// definition arrives.
// Its blocks follow only blank lines or nothing, so block 0 opens the document.
const EMPTY_CONTEXTS = {
	chat: fixedContext(scopeOf(null, null, null), null, true),
	github: fixedContext(scopeOf('github', null, null), null, true),
	gitlab: fixedContext(scopeOf('gitlab', null, null), null, true)
};

/** Backslash-escapes every ASCII punctuation character: `text` as Markdown. */
const literal = (text: string): string => text.replace(/[!-/:-@[-`{-~]/g, '\\$&');

/** GitLab's list: each heading under the nearest earlier one of a lower level. */
const tocSource = (headings: readonly { text: string; depth: number; id: string }[]): string => {
	const stack: number[] = [];
	let out = '';
	for (const heading of headings) {
		while (stack.length > 0 && stack[stack.length - 1] >= heading.depth) stack.pop();
		out += `${'  '.repeat(stack.length)}- [${literal(heading.text)}](#${heading.id})\n`;
		stack.push(heading.depth);
	}
	return out;
};

const SUPERSCRIPT_DIGITS = '⁰¹²³⁴⁵⁶⁷⁸⁹';
/** `n` in superscript digits, which no forge grammar reads as Markdown. */
const superscript = (n: number): string => String(n).replace(/\d/g, (digit) => SUPERSCRIPT_DIGITS[Number(digit)]);

/** The footnotes section: an ordered list, each body followed by its
 * back-references, inside its last paragraph as the forges place them. */
const footnoteSectionSource = (
	numbers: ReadonlyMap<string, number>,
	bodies: ReadonlyMap<string, readonly string[]>,
	counts: ReadonlyMap<string, number>,
	extensions: Extension[],
	scope: DocumentScope
): string => {
	let out = '';
	for (const [label, number] of numbers) {
		const lines = [...(bodies.get(label) ?? [])];
		while (lines.length > 0 && lines[0].trim() === '') lines.shift();
		while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
		const body = lines.join('\n');
		const backrefs = Array.from({ length: counts.get(label) ?? 1 }, (_, index) =>
			`[↩${index > 0 ? superscript(index + 1) : ''}](#${footnoteRefAnchor(number, index + 1)})`).join(' ');
		const inline = body !== '' && lex(body, extensions, scope).at(-1)?.type === 'paragraph';
		const item = body === '' ? backrefs : inline ? `${body} ${backrefs}` : `${body}\n\n${backrefs}`;
		const marker = `${number}. `;
		const indent = ' '.repeat(marker.length);
		out += marker + item.split('\n').map((line, index) => (index === 0 || line === '' ? line : indent + line)).join('\n') + '\n';
	}
	return out;
};

const linksKey = (links: Links): string => {
	let key = '';
	for (const label in links) key += `${label}\u0000${links[label].href}\u0000${links[label].title ?? ''}\u0001`;
	return key;
};

/** `base` with every scope it hands out reading `facts`. */
const contextWithFacts = (base: DocumentContext, facts: ForgeFactReader): DocumentContext => {
	const scopes: DocumentScope[] = [];
	let after: DocumentContext | undefined;
	const context: DocumentContext = {
		forge: base.forge,
		scope: (index) => (scopes[index] ??= { ...base.scope(index), facts }),
		get after() {
			return (after ??= base.after === base ? context : contextWithFacts(base.after, facts));
		},
		footnoteSection: base.footnoteSection
	};
	return context;
};

/**
 * The context for the document `blocks` last parsed. `extensions` are the ones the
 * blocks render with. `facts` is the host's knowledge of what the document
 * names, which every scope hands to its block's lex; a change of it alone
 * folds nothing again, and a block that read none of it keeps its output
 * (`sameScopeReads`). What other blocks need from a block is read without
 * it: a fact changes how a name renders, never a definition, footnote or
 * heading anchor.
 */
export const documentContextOf = (
	blocks: ParseBlocksCache,
	extensions: Extension[],
	cache: DocumentContextCache,
	forge: DocumentForge,
	facts: ForgeFactReader | null = null
): DocumentContext => {
	const base = foldedContextOf(blocks, extensions, cache, forge);
	if (!facts) return base;
	const held = cache.withFacts;
	if (held && held.base === base && held.facts === facts) return held.context;
	const context = contextWithFacts(base, facts);
	cache.withFacts = { base, facts, context };
	return context;
};

const foldedContextOf = (
	blocks: ParseBlocksCache,
	extensions: Extension[],
	cache: DocumentContextCache,
	forge: DocumentForge
): DocumentContext => {
	if (cache.extensions !== extensions || cache.forge !== forge) {
		cache.extensions = extensions;
		cache.forge = forge;
		cache.raws = [];
		cache.facts = [];
		cache.key = '';
		cache.lead = -1;
		cache.leadBlockIndex = -1;
		cache.context = null;
		cache.factScopes = null;
	}
	const raws = blocks.raws;
	// Every raw before `from` is one this cache read last time: the same
	// parse, and nothing before its changed tail differs.
	let from = 0;
	if (cache.context !== null && cache.blocks === blocks) {
		if (blocks.generation === cache.generation) from = raws.length;
		else if (blocks.generation === cache.generation + 1) from = blocks.changedFrom;
	}
	cache.blocks = blocks;
	cache.generation = blocks.generation;
	const lead = firstRawIndex(raws);
	const leadBlockIndex = lead >= 0 ? (blocks.blockIndexes[lead] ?? -1) : -1;
	// Whether the context is folded again: a fact or the first block's position
	// changed, or a fact sits where block indexes may have moved.
	let changed = cache.context === null || lead !== cache.lead || leadBlockIndex !== cache.leadBlockIndex;
	if (lead !== cache.lead) from = 0;
	for (let index = from; index < cache.facts.length; index++) if (holdsFact(cache.facts[index])) changed = true;
	for (let index = from; index < raws.length; index++) {
		if (cache.raws[index] === raws[index] && index < cache.facts.length && (index === lead) === (index === cache.lead)) continue;
		cache.raws[index] = raws[index];
		const facts = factsOf(raws[index], extensions, forge, index === lead);
		cache.facts[index] = facts;
		if (!facts) continue;
		if (facts.defs.length > 0) changed = true;
		// Under unchanged definitions, the scope the context folded.
		else if (forge && !changed && cache.factScopes) {
			facts.inline = inlineFactsOf(raws[index], extensions, forge, cache.factScopes[index === lead ? 1 : 0]);
			if (holdsFact(facts)) changed = true;
		}
	}
	cache.lead = lead;
	cache.leadBlockIndex = leadBlockIndex;
	cache.raws.length = raws.length;
	cache.facts.length = raws.length;
	if (!changed && cache.context) return cache.context;

	const links: Links = Object.create(null);
	let hasLinks = false;
	for (let index = 0; index < raws.length; index++) {
		for (const [label, def] of cache.facts[index]?.defs ?? NO_DEFS) {
			if (label in links) continue;
			links[label] = def;
			hasLinks = true;
		}
	}
	const sharedLinks = hasLinks ? links : null;
	if (forge) {
		const factScopes = [scopeOf(forge, sharedLinks, null, false), scopeOf(forge, sharedLinks, null, true)] as const;
		cache.factScopes = factScopes;
		// A block's facts are read again when a key its lex read resolves
		// differently under these definitions.
		for (let index = 0; index < raws.length; index++) {
			const facts = cache.facts[index];
			if (!facts) continue;
			const scope = factScopes[index === lead ? 1 : 0];
			if (facts.inline === null || !sameScopeReads(facts.inline.scope, scope, facts.inline.reads))
				facts.inline = inlineFactsOf(raws[index], extensions, forge, scope);
			else facts.inline.scope = scope;
		}
	}

	const bodies = new Map<string, readonly string[]>();
	const used = new Set<string>();
	const seen = new Map<string, number>();
	const headingIds: (readonly string[] | undefined)[] = [];
	const markersAt: number[] = [];
	const headings: { text: string; depth: number; id: string }[] = [];
	let lastMarker = -1;
	for (let index = 0; index < raws.length; index++) {
		const facts = cache.facts[index]?.inline;
		if (!facts) continue;
		for (const [label, lines] of facts.footnotes) if (!bodies.has(label)) bodies.set(label, lines);
		if (facts.headings.length > 0) {
			const ids: string[] = [];
			for (const heading of facts.headings) {
				let id = heading.slug;
				let n = seen.get(heading.slug) ?? 0;
				while (used.has(id)) id = `${heading.slug}-${++n}`;
				seen.set(heading.slug, n);
				used.add(id);
				ids.push(id);
				headings.push({ text: heading.text, depth: heading.depth, id });
			}
			headingIds[index] = ids;
		}
		if (facts.markers > 0) {
			markersAt[index] = facts.markers;
			lastMarker = index;
		}
	}
	// Numbers and the references before each block, in document order.
	const numbers = new Map<string, number>();
	const counts = new Map<string, number>();
	const bases: (ReadonlyMap<string, number> | undefined)[] = [];
	if (forge) {
		for (let index = 0; index < raws.length; index++) {
			const facts = cache.facts[index]?.inline;
			if (!facts || facts.refs.length === 0) continue;
			const base = new Map<string, number>();
			for (const label of facts.refs) {
				if (!bodies.has(label)) continue;
				if (!numbers.has(label)) numbers.set(label, numbers.size + 1);
				if (!base.has(label)) base.set(label, counts.get(label) ?? 0);
				counts.set(label, (counts.get(label) ?? 0) + 1);
			}
			if (base.size > 0) bases[index] = base;
		}
	}
	const toc = forge === 'gitlab' && lastMarker !== -1 ? tocSource(headings) : null;

	let key = `${hasLinks ? linksKey(links) : ''}\u0002`;
	for (const [label, number] of numbers) key += `${label}\u0000${number}\u0000${counts.get(label)}\u0000${bodies.get(label)?.join('\n')}\u0001`;
	key += '\u0002';
	for (let index = 0; index < raws.length; index++) {
		const ids = headingIds[index];
		const base = bases[index];
		const markers = markersAt[index] ?? 0;
		if (!ids && !base && markers === 0) continue;
		key += `${index}:${blocks.blockIndexes[index]}:${ids?.join('\u0000') ?? ''}:${base ? [...base].join(',') : ''}:${markers}\u0001`;
	}
	key += `\u0002${toc ?? ''}`;
	// With no facts, every raw before block 0 is blank (a footnote or a
	// definition is a fact), so block 0 opens the document.
	const empty = key === '\u0002\u0002\u0002';
	key += `\u0002${lead}:${blocks.blockIndexes[lead] ?? -1}`;
	if (cache.context && key === cache.key) return cache.context;
	cache.key = key;

	if (empty) return (cache.context = EMPTY_CONTEXTS[forge ?? 'chat']);
	const sharedNumbers = forge ? numbers : null;
	const after = scopeOf(forge, sharedLinks, sharedNumbers);
	let tocList: Token | null | undefined;
	const tocOf = (index: number, markers: number): DocumentToc => {
		if (tocList === undefined) {
			tocList = headings.length > 0 ? (lex(toc ?? '', extensions, scopeOf(forge, null, null))[0] as Token | undefined) ?? null : null;
		}
		return { at: index === lastMarker ? markers - 1 : -1, list: tocList, key: toc ?? '' };
	};
	const scopes: DocumentScope[] = [];
	// The block-to-source mapping this context was folded for.
	const rawIndexes = blocks.blockRawIndexes.slice();
	const section = forge && numbers.size > 0 ? footnoteSectionSource(numbers, bodies, counts, extensions, after) : null;
	const afterContext = fixedContext(after, null, false);
	const context: DocumentContext = {
		forge,
		scope(index) {
			const rawIndex = rawIndexes[index];
			if (rawIndex === undefined) return after;
			let scope = scopes[index];
			if (scope) return scope;
			const ids = headingIds[rawIndex];
			const base = bases[rawIndex];
			const markers = markersAt[rawIndex] ?? 0;
			const first = rawIndex === lead;
			scope = !ids && !base && markers === 0 && !first
				? after
				: {
					forge,
					links: sharedLinks,
					footnotes: sharedNumbers,
					footnoteBase: base ?? null,
					headingIds: ids ?? null,
					toc: toc !== null && markers > 0 ? tocOf(rawIndex, markers) : null,
					first
				};
			scopes[index] = scope;
			return scope;
		},
		after: afterContext,
		footnoteSection: section
	};
	return (cache.context = context);
};
