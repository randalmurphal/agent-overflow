import { describe, expect, it } from 'vitest';
import { lex } from '../lexer';

// A block quote opening left of a list item's content ends the item
// (CommonMark); one indented to the content belongs to it.

const types = (source: string) => lex(source, []).filter((token) => token.type !== 'space').map((token) => token.type);

describe('a block quote after a list item', () => {
	it.each([
		['an ordered item', '1. item\n> quote'],
		['a bullet item', '- item\n> quote'],
		['a nested marker', '- item\n>>> quote'],
		['an indented quote left of the content', '10. item\n  > quote']
	])('ends the list after %s', (_name, source) => {
		expect(types(source)).toEqual(['list', 'blockquote']);
	});

	it('stays in the item when indented to its content', () => {
		const tokens = lex('- item\n  > quote', []);
		expect(tokens.map((token) => token.type)).toEqual(['list']);
		expect(JSON.stringify(tokens)).toContain('"type":"blockquote"');
	});

	it('leaves a lazy paragraph line in the item', () => {
		expect(types('- item\nlazy')).toEqual(['list']);
	});
});
