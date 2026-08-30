import { describe, expect, it } from 'vitest';
import { Lexer } from './Lexer';

// Pins the ONE accepted divergence from marked 16.4.2 output
// (AGENTS.md § Provenance): the 17.0.5 link-label ReDoS fix (#3918)
// changes the accepted language, not just the token shapes. 16.4.2's
// label grammar matched an unmatched multi-backtick run mid-label as an
// empty code span (`` ` ``+`` ` ``), through the alternation that also
// backtracked catastrophically on unclosed labels. The ported rule
// refuses to start a backtick alternative mid-run, so such a label no
// longer forms a link — byte-identical to every marked release since
// 17.0.5 (verified against 17.0.5 and 18.x, 2026-08-30). The renderer's
// input is attacker-influenced by construction, so the ReDoS fix wins
// over 16.4.2 parity here.

function inlineShape(input: string): string {
	const tokens = new Lexer().lex(input);
	const first = tokens[0] as { tokens?: { type: string; raw: string }[] };
	return (first.tokens ?? [])
		.map((t) => `${t.type}:${t.raw}`)
		.join(' | ');
}

describe('link-label backtick runs (ported 17.0.5 #3918)', () => {
	it('an unmatched multi-backtick run mid-label breaks the link (the accepted 16.4.2 divergence)', () => {
		expect(inlineShape('[use ``raw](https://example.com)')).toBe(
			'text:[use ``raw]( | link:https://example.com | text:)',
		);
		expect(inlineShape('[a `` b](https://example.com)')).toBe(
			'text:[a `` b]( | link:https://example.com | text:)',
		);
	});

	it('matched code spans and trailing runs still form links, as in 16.4.2', () => {
		expect(inlineShape('[plain `code` label](https://example.com)')).toBe(
			'link:[plain `code` label](https://example.com)',
		);
		expect(inlineShape('[trail ``](https://example.com)')).toBe(
			'link:[trail ``](https://example.com)',
		);
	});
});
