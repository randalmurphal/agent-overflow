import { describe, expect, it, vi } from 'vitest';
import { lex, lexFootnoteDefinitions, parseBlocks } from './index';
import { createParseBlocksCache } from './parseBlocks.cache';
import type { ParseBlocksCache } from './parseBlocks.cache';
import { Lexer } from './engine';
import { createDocumentContextCache, documentContextOf, headingSlug } from './documentContext';
import type { DocumentContext, DocumentContextCache } from './documentContext';
import { sameScopeReads } from './documentScope';
import type { DocumentForge, DocumentScope } from './documentScope';
import { lexCapture, markedForgeGrammar } from './lexer';
import type { Extension, StreamdownToken } from './lexer';
import { markedHtmlEntities } from './extensions/htmlEntities';
import { createProvenAppend } from './provenAppend';
import { incrementalLex } from './incrementalLex';
import { createIncrementalLexCache } from './incrementalLex.cache';

// One list for every call: a new one resets a context cache.
const NO_EXTENSIONS: Extension[] = [];

type Walked = { type: string; href?: string; title?: string | null; text?: string; anchor?: string; number?: number; label?: string; tokens?: Walked[] };

/** Every block of `source` lexed under its document scope, as the renderer does. */
function lexDocument(source: string, forge: DocumentForge, extensions: Extension[] = []) {
	const blocks = createParseBlocksCache();
	const strings = parseBlocks(source, extensions, blocks);
	const context = documentContextOf(blocks, extensions, createDocumentContextCache(), forge);
	return { context, tokens: strings.map((block, index) => lex(block, extensions, context?.scope(index) ?? null)) };
}

function all(tokens: readonly StreamdownToken[] | Walked[], type: string): Walked[] {
	const out: Walked[] = [];
	const walk = (list: readonly Walked[] | undefined) => {
		for (const token of list ?? []) {
			if (token.type === type) out.push(token);
			walk(token.tokens);
		}
	};
	walk(tokens as Walked[]);
	return out;
}

const linksOf = (source: string, forge: DocumentForge = null, extensions: Extension[] = []) =>
	all(lexDocument(source, forge, extensions).tokens.flat(), 'link').map((link) => [link.text, link.href, link.title ?? null]);

describe('reference definitions across blocks', () => {
	it.each([null, 'github', 'gitlab'] as const)('resolve a use before or after its definition, in any container (%s)', (forge) => {
		const source = [
			'Before: [full][Some  LABEL] and [collapsed][] and [shortcut].',
			'',
			'> Quoted [inner] and [Collapsed][]',
			'',
			'- Listed [inner]',
			'',
			'[some label]: https://example.invalid/full',
			'[collapsed]: https://example.invalid/collapsed "Title"',
			'[shortcut]: <https://example.invalid/with space>',
			'',
			'> [inner]: https://example.invalid/inner',
			'',
			'After: [shortcut]'
		].join('\n');
		expect(linksOf(source, forge)).toEqual([
			['full', 'https://example.invalid/full', null],
			['collapsed', 'https://example.invalid/collapsed', 'Title'],
			['shortcut', 'https://example.invalid/with space', null],
			['inner', 'https://example.invalid/inner', null],
			['Collapsed', 'https://example.invalid/collapsed', 'Title'],
			['inner', 'https://example.invalid/inner', null],
			['shortcut', 'https://example.invalid/with space', null]
		]);
	});

	it('resolves reference images', () => {
		const images = all(lexDocument('![alt][logo] and ![logo]\n\n[logo]: https://example.invalid/l.png "Logo"', 'github').tokens.flat(), 'image');
		expect(images.map((image) => [image.text, image.href, image.title])).toEqual([
			['alt', 'https://example.invalid/l.png', 'Logo'],
			['logo', 'https://example.invalid/l.png', 'Logo']
		]);
	});

	it('keeps the first definition of a label, wherever the use is', () => {
		const source = 'Use [x].\n\n[X]: https://example.invalid/first\n\nAgain [x].\n\n[x]: https://example.invalid/second\n\nLast [x].';
		expect(linksOf(source).map(([, href]) => href)).toEqual([
			'https://example.invalid/first',
			'https://example.invalid/first',
			'https://example.invalid/first'
		]);
	});

	it('keeps emphasis from closing inside a use defined in another block', () => {
		const { tokens } = lexDocument('*open [x*][ref] close*\n\n[ref]: https://example.invalid/ref', null);
		expect(all(tokens.flat(), 'em').map((em) => em.text)).toEqual(['open [x*][ref] close']);
	});

	it('reads no definition inside fenced code', () => {
		expect(linksOf('Use [x].\n\n```\n[x]: https://example.invalid/code\n```')).toEqual([]);
	});

	it('leaves an undefined reference as its text', () => {
		const { tokens } = lexDocument('[nope][missing] and [also missing]\n\n[other]: https://example.invalid/', null);
		expect(all(tokens.flat(), 'link')).toEqual([]);
		expect(tokens[0][0]).toMatchObject({ type: 'paragraph', text: '[nope][missing] and [also missing]' });
	});

	it('resolves a destination and title through the linkText option', () => {
		const source = 'Use [x].\n\n[x]: https://example.invalid/a&amp;b "T &copy;"';
		expect(linksOf(source, 'gitlab', [markedHtmlEntities])).toEqual([['x', 'https://example.invalid/a&b', 'T ©']]);
	});

	it('wins over an agent chat citation, which still claims an undefined bracket', () => {
		const { tokens } = lexDocument('See [1] and [2].\n\n[1]: https://example.invalid/one', null);
		expect(all(tokens.flat(), 'link').map((link) => link.href)).toEqual(['https://example.invalid/one']);
		expect(all(tokens.flat(), 'inline-citations')).toHaveLength(1);
	});

	it('claims no citation on a forge, which has no citation syntax', () => {
		const { tokens } = lexDocument('[[_TOC_]] and [ref]', 'github', [markedForgeGrammar]);
		expect(all(tokens.flat(), 'inline-citations')).toEqual([]);
		expect(all(tokens.flat(), 'em').map((em) => em.text)).toEqual(['TOC']);
	});
});

describe('the document context across updates', () => {
	it('reads only the blocks an append adds and keeps its identity without a new fact', () => {
		const blocks = createParseBlocksCache();
		const cache = createDocumentContextCache();
		const extensions: Extension[] = [];
		let source = 'Use [x].\n\n[x]: https://example.invalid/x\n\n';
		parseBlocks(source, extensions, blocks);
		const first = documentContextOf(blocks, extensions, cache, null);
		const facts = cache.facts.slice();
		const next = `${source}More prose without definitions.\n\n`;
		parseBlocks(next, extensions, blocks, createProvenAppend(source, next.slice(source.length)));
		source = next;
		expect(documentContextOf(blocks, extensions, cache, null)).toBe(first);
		for (const [index, fact] of facts.entries()) expect(cache.facts[index]).toBe(fact);
		const withDef = `${source}[y]: https://example.invalid/y\n`;
		parseBlocks(withDef, extensions, blocks, createProvenAppend(source, withDef.slice(source.length)));
		const second = documentContextOf(blocks, extensions, cache, null);
		expect(second).not.toBe(first);
		expect(second?.scope(0).links?.y?.href).toBe('https://example.invalid/y');
	});

	it('shares nothing in agent chat without a definition', () => {
		const { context } = lexDocument('# Title\n\nprose [^1]\n\n[^1]: note', null);
		expect(context.scope(0)).toEqual({ forge: null, links: null, footnotes: null, footnoteBase: null, headingIds: null, toc: null, first: true });
		expect(context.footnoteSection).toBeNull();
	});

	it('keeps a block whose reads resolve as before and rebuilds one whose reads change', () => {
		const before = lexDocument('Use [a].\n\nOther [b].\n\n[b]: https://example.invalid/b', null).context!;
		const after = lexDocument('Use [a].\n\nOther [b].\n\n[b]: https://example.invalid/b\n\n[a]: https://example.invalid/a', null).context!;
		const reads = (block: string, context: DocumentContext, index: number) => lexCapture(block, [], null, null, context.scope(index)).reads;
		expect(sameScopeReads(before.scope(0), after.scope(0), reads('Use [a].', before, 0))).toBe(false);
		expect(sameScopeReads(before.scope(1), after.scope(1), reads('Other [b].', before, 1))).toBe(true);
		// A block lexed without a scope recorded nothing it read.
		expect(sameScopeReads(null, after.scope(1), null)).toBe(false);
	});

	it('keeps an open block lexed incrementally until a key it read changes', () => {
		const before = lexDocument('Use [a].\n\n[b]: https://example.invalid/b', null).context;
		const unrelated = lexDocument('Use [a].\n\n[b]: https://example.invalid/b\n\n[c]: https://example.invalid/c', null).context;
		const defined = lexDocument('Use [a].\n\n[a]: https://example.invalid/a', null).context;
		const cache = createIncrementalLexCache();
		const first = incrementalLex('Use [a].', [], cache, null, undefined, before.scope(0));
		expect(incrementalLex('Use [a].', [], cache, null, undefined, unrelated.scope(0))).toBe(first);
		const relexed = incrementalLex('Use [a].', [], cache, null, undefined, defined.scope(0));
		expect(all(relexed, 'link').map((link) => link.href)).toEqual(['https://example.invalid/a']);
	});
});

describe('footnote definitions', () => {
	it('hold lazy and indented continuation lines and interrupt a paragraph', () => {
		const source = [
			'Text[^a][^b].',
			'',
			'[^a]: First line  ',
			'lazy continuation.',
			'[^b]: Block body:',
			'',
			'    ```js',
			'    code();',
			'    ```',
			'',
			'    Second paragraph.',
			'',
			'After the definitions.'
		].join('\n');
		const definitions = lexFootnoteDefinitions(source)!;
		expect(definitions.get('a')?.lines).toEqual(['First line  ', 'lazy continuation.']);
		expect(definitions.get('b')?.lines).toEqual(['Block body:', '', '```js', 'code();', '```', '', 'Second paragraph.']);
		// Neither definition leaks into the rendered blocks.
		const shown = parseBlocks(source).map((block) => lex(block)).flat();
		expect(shown.map((token) => (token as { raw: string }).raw.trim())).toEqual(['Text[^a][^b].', 'After the definitions.']);
	});

	it('number on a forge by first reference, list once each, and leave an undefined reference as text', () => {
		const source = [
			'[^early]: Defined before use.',
			'',
			'One[^b] two[^early] three[^b] four[^missing].',
			'',
			'[^b]: Bee.',
			'[^unused]: Never referenced.'
		].join('\n');
		const { context, tokens } = lexDocument(source, 'github');
		const refs = all(tokens.flat(), 'footnoteRef');
		expect(refs.map((ref) => [ref.label, ref.number, ref.anchor])).toEqual([
			['b', 1, 'fnref:1'],
			['early', 2, 'fnref:2'],
			['b', 1, 'fnref:1:2']
		]);
		expect(all(tokens.flat(), 'paragraph')[0].text).toContain('four[^missing].');
		expect(context?.footnoteSection).toBe('1. Bee. [↩](#fnref:1) [↩²](#fnref:1:2)\n2. Defined before use. [↩](#fnref:2)\n');
	});

	it('continue the occurrence count across blocks', () => {
		const { tokens } = lexDocument('A[^n].\n\nB[^n].\n\n[^n]: Note.', 'gitlab');
		expect(all(tokens.flat(), 'footnoteRef').map((ref) => ref.anchor)).toEqual(['fnref:1', 'fnref:1:2']);
	});

	it('place back-references in a new paragraph after a body ending in another block', () => {
		const { context } = lexDocument('A[^n].\n\n[^n]: Intro:\n\n    ```\n    x\n    ```', 'gitlab');
		expect(context?.footnoteSection).toBe('1. Intro:\n\n   ```\n   x\n   ```\n\n   [↩](#fnref:1)\n');
	});

	it('keep their labels in agent chat', () => {
		const refs = all(lexDocument('A[^note] and [^missing].\n\n[^note]: Note.\n\n[x]: https://example.invalid/', null).tokens.flat(), 'footnoteRef');
		expect(refs.map((ref) => [ref.label, ref.number])).toEqual([['note', undefined], ['missing', undefined]]);
	});
});

describe('heading anchors', () => {
	it.each([
		['Sample Section', 'sample-section'],
		["This'll be a Helpful Section About the Greek Letter Θ!", 'thisll-be-a-helpful-section-about-the-greek-letter-θ'],
		['This heading has 3.5 in it (and parentheses)', 'this-heading-has-35-in-it-and-parentheses'],
		['has  multiple spaces and --- hyphens_and_underscores', 'has--multiple-spaces-and-----hyphens_and_underscores'],
		['Unicode: 한글', 'unicode-한글']
	])('slug %s', (text, slug) => {
		expect(headingSlug(text)).toBe(slug);
	});

	it.each(['github', 'gitlab'] as const)('number repeats and read markup as text (%s)', (forge) => {
		const source = '# Dup\n\n## Dup\n\n> ### Dup\n\n## `code` and [link](https://example.invalid/) _em_\n\n## Rocket :rocket:';
		const anchors = all(lexDocument(source, forge).tokens.flat(), 'heading').map((heading) => heading.anchor);
		expect(anchors).toEqual(['dup', 'dup-1', 'dup-2', 'code-and-link-em', 'rocket-rocket']);
	});

	it('read an image as its alt text on GitLab only', () => {
		const source = '## With ![img](https://example.invalid/i.png)';
		expect(all(lexDocument(source, 'gitlab').tokens.flat(), 'heading')[0].anchor).toBe('with-img');
		expect(all(lexDocument(source, 'github').tokens.flat(), 'heading')[0].anchor).toBe('with');
	});

	it('are absent in agent chat', () => {
		const { tokens } = lexDocument('# Title\n\n[x]: https://example.invalid/', null);
		expect(all(tokens.flat(), 'heading')[0].anchor).toBeUndefined();
	});
});

describe('a heading written with a reference link', () => {
	const SOURCE = '# [Title][ref]\n\n[ref]: https://example.org\n\n[[_TOC_]]';

	it.each(['github', 'gitlab'] as const)('takes its anchor from the link text (%s)', (forge) => {
		const { tokens } = lexDocument(SOURCE, forge);
		const [heading] = all(tokens.flat(), 'heading');
		expect(heading.anchor).toBe('title');
		expect(all(heading.tokens ?? [], 'link').map((link) => [link.text, link.href])).toEqual([['Title', 'https://example.org']]);
	});

	it("lists the link text in GitLab's table of contents", () => {
		const list = lexDocument(SOURCE, 'gitlab').tokens.flat().find((token) => token.type === 'list') as Walked;
		expect(all(list.tokens ?? [], 'link').map((link) => [link.text, link.href])).toEqual([['Title', '#title']]);
	});

	/** A document parsed and folded by appends, as chat streams one. */
	const stream = (forge: DocumentForge) => {
		const blocks = createParseBlocksCache();
		const cache = createDocumentContextCache();
		let source = '';
		return {
			/** Appends `more` and folds the document again. */
			append(more: string): DocumentContext {
				const append = createProvenAppend(source, more);
				source = append.next;
				parseBlocks(source, NO_EXTENSIONS, blocks, append);
				return documentContextOf(blocks, NO_EXTENSIONS, cache, forge);
			},
			/** The heading anchors of the blocks lexed under `context`. */
			anchors: (context: DocumentContext) =>
				all(blocks.blocks.flatMap((block, index) => lex(block, [], context.scope(index))), 'heading').map((heading) => heading.anchor)
		};
	};

	it('takes the link text once a later update defines the reference', () => {
		const document = stream('gitlab');
		expect(document.anchors(document.append('# [Title][ref]\n\nProse.\n\n'))).toEqual(['titleref']);
		expect(document.anchors(document.append('[ref]: https://example.org\n'))).toEqual(['title']);
	});

	it('reads no block again for a definition no block read', () => {
		const document = stream('gitlab');
		const before = document.append('# [Title][ref]\n\n[ref]: https://example.org\n\nProse.\n\n');
		// The fold's lexes, by source.
		const lexSpy = vi.spyOn(Lexer.prototype, 'lex');
		let duplicate: DocumentContext;
		let unrelated: DocumentContext;
		let lexed: string[];
		try {
			// The first definition of a label wins: this one changes nothing.
			duplicate = document.append('[REF]: https://example.invalid/other\n\n');
			unrelated = document.append('[other]: https://example.invalid/other\n');
			lexed = lexSpy.mock.calls.map(([src]) => src);
		} finally {
			lexSpy.mockRestore();
		}
		expect(duplicate).toBe(before);
		expect(unrelated.scope(0).links?.other?.href).toBe('https://example.invalid/other');
		expect(document.anchors(unrelated)).toEqual(['title']);
		// Only the appended definitions were read, never the heading.
		expect(lexed.length).toBeGreaterThan(0);
		expect(lexed.filter((src) => !/^\[(REF|other)\]: /.test(src))).toEqual([]);
	});
});

describe('the document context on appends', () => {
	/** Counts what `run` reads per raw: the raws of `blocks` and their facts in `cache`. */
	const rawReads = (blocks: ParseBlocksCache, cache: DocumentContextCache, run: () => void): number => {
		const { raws } = blocks;
		const { facts } = cache;
		let reads = 0;
		const counted: ProxyHandler<unknown[]> = {
			get(target, key, receiver) {
				if (typeof key === 'string' && /^\d+$/.test(key)) reads++;
				return Reflect.get(target, key, receiver);
			}
		};
		blocks.raws = new Proxy(raws, counted) as string[];
		cache.facts = new Proxy(facts, counted) as typeof facts;
		try {
			run();
		} finally {
			blocks.raws = raws;
			cache.facts = facts;
		}
		return reads;
	};

	it.each([null, 'gitlab'] as const)('reads only the changed tail after 3,000 committed paragraphs (%s)', (forge) => {
		const blocks = createParseBlocksCache();
		const cache = createDocumentContextCache();
		let source = '';
		const commit = (more: string) => {
			const append = createProvenAppend(source, more);
			source = append.next;
			parseBlocks(source, NO_EXTENSIONS, blocks, append);
		};
		commit('Use [x] and [y].\n\n[y]: https://example.invalid/y\n\n');
		let context = documentContextOf(blocks, NO_EXTENSIONS, cache, forge);
		for (let n = 0; n < 3000; n++) {
			commit(`Paragraph ${n}.\n\n`);
			context = documentContextOf(blocks, NO_EXTENSIONS, cache, forge);
		}
		commit('One more.\n\n');
		let next: DocumentContext | undefined;
		expect(rawReads(blocks, cache, () => (next = documentContextOf(blocks, NO_EXTENSIONS, cache, forge)))).toBeLessThan(20);
		expect(next).toBe(context);
		// A block appended after the context was made gets the document's scope.
		expect(next!.scope(blocks.blocks.length - 1).links?.y?.href).toBe('https://example.invalid/y');
		commit('[x]: https://example.invalid/x\n');
		const defined = documentContextOf(blocks, NO_EXTENSIONS, cache, forge);
		expect(defined).not.toBe(context);
		const [use] = lex(blocks.blocks[0], [], defined.scope(0));
		expect(all([use], 'link').map((link) => link.href)).toEqual(['https://example.invalid/x', 'https://example.invalid/y']);
	});

	/** A scope as plain data. */
	const shape = (scope: DocumentScope) => ({
		forge: scope.forge,
		links: scope.links ? Object.entries(scope.links) : null,
		footnotes: scope.footnotes ? [...scope.footnotes] : null,
		footnoteBase: scope.footnoteBase ? [...scope.footnoteBase] : null,
		headingIds: scope.headingIds,
		toc: scope.toc ? [scope.toc.at, scope.toc.key] : null,
		first: scope.first
	});

	const DOCUMENT = [
		'\n# Intro [link][a]\n\nUse [a] and a note[^n].\n\n[[_TOC_]]\n\n',
		'Prose without facts.\n\n- a list\n- of two\n\n[a]: https://example.invalid/a\n\n',
		'## Intro [link][a]\n\nMore prose.\n\n[^n]: The note.\n\n',
		'Later [b] and again[^n].\n\n```\n[b]: https://example.invalid/code\n```\n\n[b]: https://example.invalid/b\n\nEnd.\n'
	].join('');

	it.each([null, 'github', 'gitlab'] as const)('scopes every block as a fresh fold does, at every chunk size (%s)', (forge) => {
		for (const size of [1, 3, 7, 16, 50]) {
			const blocks = createParseBlocksCache();
			const cache = createDocumentContextCache();
			let source = '';
			for (let at = 0; at < DOCUMENT.length; at += size) {
				const append = createProvenAppend(source, DOCUMENT.slice(at, at + size));
				source = append.next;
				parseBlocks(source, NO_EXTENSIONS, blocks, append);
				const context = documentContextOf(blocks, NO_EXTENSIONS, cache, forge);
				const freshBlocks = createParseBlocksCache();
				parseBlocks(source, [], freshBlocks);
				const fresh = documentContextOf(freshBlocks, [], createDocumentContextCache(), forge);
				for (let index = 0; index <= blocks.blocks.length; index++)
					expect(shape(context.scope(index)), `${size} ${JSON.stringify(source)} ${index}`).toEqual(shape(fresh.scope(index)));
				expect(context.footnoteSection).toBe(fresh.footnoteSection);
			}
		}
	});
});

describe("GitLab's table of contents", () => {
	const SOURCE = '[[_TOC_]]\n\n# One\n\n### Deep\n\n## Two\n\n# Three\n\n[toc]\n\nInline [TOC] stays.';

	it('shows the list at the last marker, nested under the nearest lower heading, and drops the others', () => {
		const { tokens } = lexDocument(SOURCE, 'gitlab');
		expect(tokens[0]).toEqual([]);
		const lists = tokens.flat().filter((token) => token.type === 'list');
		expect(lists).toHaveLength(1);
		expect(lists[0].raw).toBe('[toc]');
		const entries = (list: Walked): unknown[] => (list.tokens ?? []).map((item) => {
			const link = all(item.tokens ?? [], 'link')[0];
			const nested = (item.tokens ?? []).find((child) => child.type === 'list');
			return nested ? [link.text, link.href, entries(nested)] : [link.text, link.href];
		});
		expect(entries(lists[0] as Walked)).toEqual([
			['One', '#one', [['Deep', '#deep'], ['Two', '#two']]],
			['Three', '#three']
		]);
		expect(tokens.at(-1)?.[0]).toMatchObject({ type: 'paragraph', text: 'Inline [TOC] stays.' });
	});

	it('shows nothing without headings', () => {
		expect(lexDocument('Before\n\n[[_TOC_]]\n\nAfter', 'gitlab').tokens[1]).toEqual([]);
	});

	it('stays text on GitHub', () => {
		const { tokens } = lexDocument(SOURCE, 'github');
		expect(tokens.flat().filter((token) => token.type === 'list')).toEqual([]);
		expect(tokens[tokens.length - 2][0]).toMatchObject({ type: 'paragraph', text: '[toc]' });
	});
});
