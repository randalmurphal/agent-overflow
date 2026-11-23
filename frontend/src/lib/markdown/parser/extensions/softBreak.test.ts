import { describe, expect, it } from 'vitest';
import { Lexer } from '../engine';
import { getLexOptions, lex } from '../lexer';
import type { GenericToken } from '../lexer';
import { markedSoftBreak } from './softBreak';

const lexBreaks = (src: string) => lex(src, [markedSoftBreak]) as GenericToken[];

// Every token type in document order, containers walked.
const typesOf = (tokens: GenericToken[] | undefined): string[] =>
	(tokens ?? []).flatMap((token) => [token.type, ...typesOf(token.tokens), ...typesOf(token.items)]);

const brCount = (tokens: GenericToken[]) => typesOf(tokens).filter((type) => type === 'br').length;

const paragraphInline = (src: string): GenericToken[] => {
	const [paragraph] = lexBreaks(src);
	expect(paragraph.type).toBe('paragraph');
	return paragraph.tokens as GenericToken[];
};

describe('markedSoftBreak', () => {
	it('turns a soft break in a paragraph into a br', () => {
		const tokens = paragraphInline('line one\nline two\nline three');
		expect(tokens.map((token) => [token.type, token.raw])).toEqual([
			['text', 'line one'],
			['br', '\n'],
			['text', 'line two'],
			['br', '\n'],
			['text', 'line three']
		]);
	});

	it('keeps the spaces before the newline in raw', () => {
		const tokens = paragraphInline('one \ntwo');
		expect(tokens.map((token) => [token.type, token.raw])).toEqual([
			['text', 'one'],
			['br', ' \n'],
			['text', 'two']
		]);
	});

	it('keeps hard breaks one br each', () => {
		expect(paragraphInline('one  \ntwo').map((token) => [token.type, token.raw])).toEqual([
			['text', 'one'],
			['br', '  \n'],
			['text', 'two']
		]);
		expect(paragraphInline('one\\\ntwo').map((token) => [token.type, token.raw])).toEqual([
			['text', 'one'],
			['br', '\\\n'],
			['text', 'two']
		]);
	});

	it('adds no br at the end of a paragraph', () => {
		expect(brCount(lexBreaks('only line\n'))).toBe(0);
		expect(brCount(lexBreaks('only line   \n'))).toBe(0);
		expect(brCount(lexBreaks('first\n\nsecond\n'))).toBe(0);
		expect(brCount(lexBreaks('a\nb\n\nc\n'))).toBe(1);
		// Inline content whose own source ends in a newline, before the
		// closing bracket.
		expect(brCount(lexBreaks('[text\n](https://example.com)'))).toBe(0);
	});

	it('breaks inside a blockquote and a list item', () => {
		expect(typesOf(lexBreaks('> quoted one\n> quoted two'))).toEqual(['blockquote', 'paragraph', 'text', 'br', 'text']);
		const list = lexBreaks('- item one\n  continued\n- item two');
		expect(brCount(list)).toBe(1);
		expect(JSON.stringify(list)).toContain('"raw":"\\n"');
	});

	it('breaks inside emphasis and link text', () => {
		expect(typesOf(lexBreaks('**bold\nstill bold**'))).toEqual(['paragraph', 'strong', 'text', 'br', 'text']);
		expect(typesOf(lexBreaks('[link\ntext](https://example.com)'))).toEqual(['paragraph', 'link', 'text', 'br', 'text']);
	});

	it('leaves code spans and fenced code untouched', () => {
		const tokens = paragraphInline('`code\nspan` after');
		expect(tokens[0].type).toBe('codespan');
		expect(brCount(tokens)).toBe(0);
		const [fence] = lexBreaks('```\na\nb\n```');
		expect(fence.type).toBe('code');
		expect(fence.text).toBe('a\nb');
	});

	it('keeps raw lengths summing to the source', () => {
		const src = 'one\ntwo  \nthree \\\nfour\n\n> q1\n> q2\n\n- a\n  b\n- c\n\ntail \n';
		// The unfiltered token list: lex() drops blank-line space tokens.
		const blocks = new Lexer(getLexOptions([markedSoftBreak])).lex(src) as GenericToken[];
		expect(blocks.reduce((sum, token) => sum + token.raw.length, 0)).toBe(src.length);
		const [paragraph] = blocks;
		expect((paragraph.tokens as GenericToken[]).map((token) => token.raw).join('')).toBe(paragraph.text);
	});

	it('changes nothing for a lexer without the extension', () => {
		const [paragraph] = lex('line one\nline two') as GenericToken[];
		expect(typesOf(paragraph.tokens)).toEqual(['text']);
	});
});
