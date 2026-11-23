import { describe, expect, it, vi } from 'vitest';
import { Lexer } from './Lexer';
import { Tokenizer } from './Tokenizer';
import type { Tokens } from './tokens';
import { defaultOptions } from './options';
import { getLexOptions } from '../lexer';
import { markedDescriptionLists } from '../extensions/descriptionLists';

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

describe('Lexer.loneParagraph', () => {
	it('stops only its own lex early', () => {
		const lexer = new Lexer();
		expect(lexer.loneParagraph('paragraph')?.text).toBe('paragraph');
		expect(lexer.loneParagraph('# Heading\n\nparagraph')).toBeUndefined();
		const tokens = lexer.lex('# Heading\n\nRemaining paragraph');
		expect(tokens.map((token) => token.type)).toEqual(['heading', 'paragraph']);
	});
});

describe('setext headings in content lexed line by line', () => {
	/** Where `lheading` can scan in `src`: up to the first blank line. */
	const reach = (src: string): number => {
		const blank = src.search(/\n\s*?\n/);
		return blank === -1 ? src.length : blank;
	};

	/** The characters `lheading` may scan while `run` lexes. */
	const setextReach = (run: () => void): number => {
		const original = Tokenizer.prototype.lheading;
		let total = 0;
		const spy = vi.spyOn(Tokenizer.prototype, 'lheading').mockImplementation(function (this: Tokenizer, src: string) {
			total += reach(src);
			return original.call(this, src);
		});
		try {
			run();
		} finally {
			spy.mockRestore();
		}
		return total;
	};

	const LINES = 'continuation\n'.repeat(2000);
	it.each([
		['a tight list item (the engine rule)', undefined, '- item\n' + LINES, 'list'],
		['a tight list item (the list extension)', getLexOptions([]), '- item\n' + LINES, 'list'],
		['a tight GitLab description', getLexOptions([markedDescriptionLists]), 'Term\n: detail\n' + LINES, 'descriptionList']
	] as const)('scans %s with many lazy lines once', (_name, options, source, type) => {
		const [token] = new Lexer(options).lex(source);
		expect(token.type).toBe(type);
		// One scan of the run at most; from every line it is quadratic.
		expect(setextReach(() => new Lexer(options).lex(source))).toBeLessThanOrEqual(source.length);
	});

	it.each([
		['an underline ending the run', '- a\n  b\n  c\n  ===\n', [1, 'a\nb\nc']],
		['an underline after a blank line', '- a\n  b\n\n  ---\n', null]
	] as const)('still reads %s', (_name, source, heading) => {
		const [list] = new Lexer().lex(source) as unknown as Tokens.List[];
		const found = list.items[0].tokens.find((token) => token.type === 'heading') as Tokens.Heading | undefined;
		expect(found ? [found.depth, found.text] : null).toEqual(heading);
	});
});
