import { describe, expect, it } from 'vitest';
import { lex } from '../lexer';
import type { GenericToken } from '../lexer';
import { markedHtmlEntities } from './htmlEntities';

// The named table comes from the platform's HTML parser, so the decode is
// checked against a real browser's parser, not only happy-dom's.

const shown = (src: string): string => {
	const [paragraph] = lex(src, [markedHtmlEntities]) as GenericToken[];
	return (paragraph.tokens as GenericToken[]).map((token) => token.text).join('');
};

describe('markedHtmlEntities in a browser', () => {
	it('decodes the named table, one and two code point names alike', () => {
		expect(shown('&nbsp;&copy;&hellip;&rarr;&CounterClockwiseContourIntegral;&ngE;&NotEqualTilde;&fjlig;')).toBe(
			' ©…→∳≧̸≂̸fj'
		);
	});

	it('leaves unknown names, legacy prefixes and missing semicolons literal', () => {
		for (const src of ['&notanentity;', '&ampx;', '&copy', '&nbsp x', '&ThisIsNotDefined;']) {
			expect(shown(src), src).toBe(src);
		}
	});
});
