/**
 * Document-level facts for a document rendered block by block.
 *
 * Each block a `parseBlocks` cache holds is read once, when its source
 * changes, for what other blocks need from it: reference definitions (in any
 * container, never inside code, as the grammar decides) and, on a forge
 * surface, footnote definitions and references, headings and GitLab's table
 * of contents markers. A source that only grows reads only its new blocks.
 * The fold over those facts is a `DocumentContext`: one `DocumentScope` per
 * rendered block (documentScope.ts), handed to that block's lex. An update
 * that changes no fact returns the previous context, so nothing downstream
 * re-lexes.
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
import { forEachBlockToken, installDocumentScope, isTocMarker } from './documentScope';
import type { DocumentForge, DocumentScope, DocumentToc } from './documentScope';
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

interface BlockFacts {
	readonly defs: readonly [string, Links[string]][];
	readonly footnotes: readonly [string, readonly string[]][];
	readonly refs: readonly string[];
	readonly headings: readonly Heading[];
	readonly markers: number;
}

export interface DocumentContextCache {
	extensions: Extension[] | null;
	forge: DocumentForge | undefined;
	raws: string[];
	facts: (BlockFacts | null)[];
	key: string;
	/** The raw index of the document's first block (`firstRawIndex`). */
	lead: number;
	context: DocumentContext | null;
}

export const createDocumentContextCache = (): DocumentContextCache => ({
	extensions: null,
	forge: undefined,
	raws: [],
	facts: [],
	key: '',
	lead: -1,
	context: null
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

const factsOf = (raw: string, extensions: Extension[], forge: DocumentForge, first: boolean): BlockFacts | null => {
	if (!mayHoldFacts(raw, forge)) return null;
	const lexer = new Lexer(getLexOptions(extensions));
	// Its own footnote maps, never the ambient Streamdown context's.
	lexer.footnotes = { refs: new Map(), footnotes: new Map() };
	if (!forge) {
		// Definitions register during block tokenization.
		lexer.blockTokens(raw.replace(/\r\n?/g, '\n'), []);
		const defs: [string, Links[string]][] = [];
		for (const label in lexer.tokens.links) defs.push([label, lexer.tokens.links[label]]);
		return defs.length > 0 ? { defs, footnotes: [], refs: [], headings: [], markers: 0 } : null;
	}
	// The forge's grammar, with every footnote reference read as one.
	installDocumentScope(lexer, scopeOf(forge, null, null, first));
	const tokens = lexer.lex(raw);
	const defs: [string, Links[string]][] = [];
	for (const label in lexer.tokens.links) defs.push([label, lexer.tokens.links[label]]);
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
	if (defs.length === 0 && footnotes.length === 0 && refs.length === 0 && headings.length === 0 && markers === 0) return null;
	return { defs, footnotes, refs, headings, markers };
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

/**
 * The context for the document `blocks` last parsed. `extensions` are the ones the
 * blocks render with.
 */
export const documentContextOf = (
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
		cache.context = null;
	}
	const raws = blocks.raws;
	const lead = firstRawIndex(raws);
	for (let index = 0; index < raws.length; index++) {
		if (cache.raws[index] === raws[index] && index < cache.facts.length && (index === lead) === (index === cache.lead)) continue;
		cache.raws[index] = raws[index];
		cache.facts[index] = factsOf(raws[index], extensions, forge, index === lead);
	}
	cache.lead = lead;
	cache.raws.length = raws.length;
	cache.facts.length = raws.length;

	const links: Links = Object.create(null);
	let hasLinks = false;
	const bodies = new Map<string, readonly string[]>();
	const used = new Set<string>();
	const seen = new Map<string, number>();
	const headingIds: (readonly string[] | undefined)[] = [];
	const headings: { text: string; depth: number; id: string }[] = [];
	let lastMarker = -1;
	for (let index = 0; index < raws.length; index++) {
		const facts = cache.facts[index];
		if (!facts) continue;
		for (const [label, def] of facts.defs) {
			if (label in links) continue;
			links[label] = def;
			hasLinks = true;
		}
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
		if (facts.markers > 0) lastMarker = index;
	}
	// Numbers and the references before each block, in document order.
	const numbers = new Map<string, number>();
	const counts = new Map<string, number>();
	const bases: (ReadonlyMap<string, number> | undefined)[] = [];
	if (forge) {
		for (let index = 0; index < raws.length; index++) {
			const facts = cache.facts[index];
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
		const markers = cache.facts[index]?.markers ?? 0;
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
	const sharedLinks = hasLinks ? links : null;
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
			const markers = cache.facts[rawIndex]?.markers ?? 0;
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
