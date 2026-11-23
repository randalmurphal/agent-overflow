import { describe, expect, it } from 'vitest';
import { Lexer } from './engine';
import { createDocumentContextCache, documentContextOf } from './documentContext';
import { installDocumentScope, readFact, readingFor, renderUnder, sameScopeReads, type ForgeFactReader } from './documentScope';
import { createParseBlocksCache, parseBlocks } from './index';
import type { Extension } from './lexer';

// A host's facts ride on every scope a context hands out, and a block that
// read one compares it by its signature: the library knows nothing of
// what a key or an answer means.

const reader = (answers: Record<string, string>): ForgeFactReader => ({
	get: (key) => answers[key],
	signature: (key) => answers[key] ?? '',
});

const SOURCE = '[a]: https://example.invalid/a\n\nfirst [a]\n\nsecond\n';

const parsed = () => {
	const cache = createParseBlocksCache();
	parseBlocks(SOURCE, [], cache);
	return cache;
};

describe('facts on a document scope', () => {
	it('records a read and answers through the scope\'s reader', () => {
		const lexer = new Lexer();
		installDocumentScope(lexer, { forge: 'gitlab', links: null, footnotes: null, footnoteBase: null, headingIds: null, toc: null, first: false, facts: reader({ k: 'v' }) });
		expect(readFact(lexer, 'k')).toBe('v');
		expect([...lexer.documentReads!]).toEqual(['x:k']);
	});

	it('answers nothing and records nothing without a scope', () => {
		const lexer = new Lexer();
		expect(readFact(lexer, 'k')).toBeUndefined();
		expect(lexer.documentReads).toBeNull();
	});

	it('keeps output that read a key whose signature is unchanged, and only that', () => {
		const base = { forge: 'gitlab' as const, links: null, footnotes: null, footnoteBase: null, headingIds: null, toc: null, first: false };
		const before = { ...base, facts: reader({ a: '1', b: '1' }) };
		const after = { ...base, facts: reader({ a: '1', b: '2' }) };
		expect(sameScopeReads(before, after, new Set(['x:a']))).toBe(true);
		expect(sameScopeReads(before, after, new Set(['x:b']))).toBe(false);
		expect(sameScopeReads(base, after, new Set(['x:a']))).toBe(false);
		expect(sameScopeReads(base, { ...base, facts: null }, new Set(['x:a']))).toBe(true);
	});
});

describe('a render-time reading', () => {
	const base = { forge: 'gitlab' as const, links: null, footnotes: null, footnoteBase: null, headingIds: null, toc: null, first: false };
	const extension: Extension = {
		name: 'probe',
		level: 'inline',
		start: (src) => src.indexOf('#'),
		tokenizer(src) {
			if (src[0] !== '#') return undefined;
			readFact(this.lexer, src.slice(0, 2));
			return { type: 'text', raw: src.slice(0, 2), text: src.slice(0, 2) };
		},
	};
	const render = (token: object, scope: typeof base & { facts: ForgeFactReader }, text: string) => {
		const reading = readingFor(token, scope);
		renderUnder(reading, () => new Lexer().htmlTextTokens(text, [extension], true, reading));
		return reading;
	};

	it('records what HTML text reads, and holds while those keys resolve alike', () => {
		const token = {};
		const first = render(token, { ...base, facts: reader({ '#1': 'a', '#2': 'a' }) }, 'x #1 y');
		expect([...first.reads!]).toEqual(['x:#1']);
		// Another key changed: the same reading.
		expect(readingFor(token, { ...base, facts: reader({ '#1': 'a', '#2': 'b' }) })).toBe(first);
		// The key it read changed: a new one, which records afresh.
		const second = render(token, { ...base, facts: reader({ '#1': 'b', '#2': 'b' }) }, 'x #1 y');
		expect(second).not.toBe(first);
		expect([...second.reads!]).toEqual(['x:#1']);
		// A lexer's own scope is back after the run.
		const lexer = new Lexer();
		lexer.htmlTextTokens('#1', [extension], true, second);
		expect(lexer.document).toBeNull();
		expect(lexer.documentReads).toBeNull();
	});

	it('is read again once a scope arrives after none', () => {
		const token = {};
		const none = readingFor(token, null);
		expect(none.reads).toBeNull();
		expect(readingFor(token, null)).toBe(none);
		expect(readingFor(token, { ...base, facts: reader({}) })).not.toBe(none);
	});
});

describe('documentContextOf with facts', () => {
	const extensions: Extension[] = [];

	it('is the context it always was without facts', () => {
		const blocks = parsed();
		const cache = createDocumentContextCache();
		const plain = documentContextOf(blocks, extensions, cache, 'gitlab');
		expect(documentContextOf(blocks, extensions, cache, 'gitlab', null)).toBe(plain);
		expect('facts' in plain.scope(0)).toBe(false);
	});

	it('hands every scope, the after context\'s included, the facts, and folds nothing again for new ones', () => {
		const blocks = parsed();
		const cache = createDocumentContextCache();
		const plain = documentContextOf(blocks, extensions, cache, 'gitlab');
		const first = reader({ k: '1' });
		const withFirst = documentContextOf(blocks, extensions, cache, 'gitlab', first);
		expect(documentContextOf(blocks, extensions, cache, 'gitlab', first)).toBe(withFirst);
		for (const index of [0, 1, 2, 9]) {
			expect(withFirst.scope(index).facts).toBe(first);
			expect(withFirst.scope(index).links).toBe(plain.scope(index).links);
			expect(withFirst.scope(index).first).toBe(plain.scope(index).first);
		}
		expect(withFirst.scope(1)).toBe(withFirst.scope(1));
		expect(withFirst.after.scope(0).facts).toBe(first);
		expect(withFirst.after.scope(0).links).toBe(plain.after.scope(0).links);
		const second = reader({ k: '2' });
		const withSecond = documentContextOf(blocks, extensions, cache, 'gitlab', second);
		expect(withSecond).not.toBe(withFirst);
		// The fold is the same object: no block was read again.
		expect(cache.context).toBe(plain);
		expect(withSecond.scope(0).facts).toBe(second);
	});

	it('wraps the empty context of a body with nothing to share', () => {
		const cache = createDocumentContextCache();
		const empty = createParseBlocksCache();
		const facts = reader({});
		const context = documentContextOf(empty, extensions, cache, 'github', facts);
		expect(context.scope(0)).toMatchObject({ forge: 'github', first: true, facts });
		expect(context.scope(3)).toMatchObject({ first: false, facts });
		expect(context.after).toBe(context);
	});
});
