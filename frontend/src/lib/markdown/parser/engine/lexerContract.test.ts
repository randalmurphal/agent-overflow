import { describe, expect, it } from 'vitest';
import { Lexer } from './Lexer';
import { defaultOptions } from './options';
import { getLexOptions } from '../lexer';

// Pins the per-document contract the 2026-08-30 contract-lens review
// audited: a Lexer is one-shot, and the options bags shared across
// instances are immutable. Both are refusals, not conventions — the
// second-lap failure modes (two documents in one token list, one
// document's mutation reconfiguring every later Lexer) were silent.

describe('Lexer per-document contract', () => {
	it('refuses a second lex on the same instance', () => {
		const lexer = new Lexer();
		lexer.lex('alpha');
		expect(() => lexer.lex('beta')).toThrowError(/one-shot/);
	});

	it('shares one frozen default options bag across instances', () => {
		const first = new Lexer();
		expect(() => {
			(first.options as { pedantic?: boolean }).pedantic = true;
		}).toThrowError(TypeError);
		expect(defaultOptions.pedantic).toBe(false);
	});

	it('freezes the cached extension bags and their dispatch arrays', () => {
		const options = getLexOptions([]);
		expect(Object.isFrozen(options)).toBe(true);
		expect(Object.isFrozen(options.extensions)).toBe(true);
		expect(Object.isFrozen(options.extensions?.block)).toBe(true);
		expect(Object.isFrozen(options.extensions?.inline)).toBe(true);
	});
});
