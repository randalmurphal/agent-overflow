import { describe, expect, it } from 'vitest';
import { blockAlignment, createParseBlocksCache, lex, lexCapture, markedForgeGrammar, parseBlocks } from './index';
import type { Extension } from './index';
import { createDocumentContextCache, documentContextOf } from './documentContext';
import type { DocumentForge } from './documentScope';
import { sameScopeReads } from './documentScope';
import { markedMultilineBlockquote } from './extensions/multilineBlockquote';
import { markedGitlabAlert } from './extensions/alert';
import { markedFrontMatter } from './extensions/frontMatter';

// What a forge surface's grammar leaves out or reads where agent chat reads
// otherwise: chat's own constructs are text, GitLab's `>>>` quote takes an
// alert marker, front matter opens only the document, and an autolink keeps
// its backslash.

type Tok = { type: string; raw: string; text?: string; href?: string; title?: string; variant?: string; lang?: string; tokens?: Tok[] };

const FORGE: Extension[] = [markedForgeGrammar];
const CHAT: Extension[] = [];

const types = (tokens: readonly Tok[]): string[] => tokens.flatMap((token) => [token.type, ...types(token.tokens ?? [])]);
const lexed = (source: string, extensions: Extension[]): Tok[] => lex(source, extensions) as unknown as Tok[];

describe('agent chat grammar on a forge surface', () => {
	it.each([
		[':+1: nice work', 'descriptionList'],
		['- [ ] :construction: task', 'descriptionList'],
		['Power x^2^ here', 'sup'],
		['Water H~2~O', 'sub'],
		['Cited [1] here.', 'inline-citations'],
		['<Note kind="x">inside</Note>', 'mdx'],
		['Price a $ x$ b', 'math'],
		['[center]\nCentered\n[/center]', 'align'],
		['> **[!NOTE]**\n> body', 'alert']
	])('reads %j as text, where chat reads a %s', (source, chatType) => {
		expect(types(lexed(source, CHAT))).toContain(chatType);
		const forge = lexed(source, FORGE);
		expect(types(forge)).not.toContain(chatType);
		expect(forge.map((token) => token.raw).join('')).toBe(source);
	});

	it('splits blocks as it lexes them, and aligns nothing', () => {
		// A heading ends a paragraph without a blank line, so `[center]\n`
		// is a block of its own, shaped as chat's marker.
		const source = '[center]\n# Centered\n[/center]\n\n:+1: one\n:-1: two\n\n<Note>\n\nx\n\n</Note>\n';
		const cache = createParseBlocksCache();
		const blocks = parseBlocks(source, FORGE, cache);
		expect(blocks).toEqual(lexed(source, FORGE).map((token) => token.raw));
		expect(blocks.map((_, index) => blockAlignment(cache, index))).toEqual(blocks.map(() => null));
		// Chat aligns the block between the markers.
		const chat = createParseBlocksCache();
		parseBlocks(source, CHAT, chat);
		expect([0, 1, 2].map((index) => blockAlignment(chat, index))).toEqual(['marker', 'center', 'marker']);
	});
});

describe("GitLab's >>> alert", () => {
	const ext = [markedForgeGrammar, markedMultilineBlockquote, markedGitlabAlert];

	it('takes a marker and a title on the opening line', () => {
		const quote = '>>> [!note] Things to consider\nYou should consider:\n\n1. one\n1. two\n>>>\n';
		const [alert, after] = lexed(`${quote}\nafter`, ext);
		expect(alert).toMatchObject({ type: 'alert', variant: 'note', title: 'Things to consider', raw: `${quote}\n` });
		expect(alert.tokens!.map((token) => token.type)).toEqual(['paragraph', 'list']);
		expect(after).toMatchObject({ type: 'paragraph', text: 'after' });
		expect(parseBlocks(`${quote}\nafter`, ext)).toEqual([`${quote}\n`, 'after']);
	});

	it('titles a bare marker by its variant', () => {
		const [alert] = lexed('>>> [!WARNING]\nbody\n>>>', ext);
		expect(alert).toMatchObject({ type: 'alert', variant: 'warning' });
		expect(alert.title).toBeUndefined();
	});

	it('is no fence with other text on the opening line', () => {
		expect(types(lexed('>>> plain\nbody\n>>>', ext))).not.toContain('alert');
	});
});

describe('GitLab front matter', () => {
	const ext = [markedForgeGrammar, markedFrontMatter];

	function contextOf(source: string, forge: DocumentForge = 'gitlab') {
		const blocks = createParseBlocksCache();
		const strings = parseBlocks(source, ext, blocks);
		return { strings, context: documentContextOf(blocks, ext, createDocumentContextCache(), forge) };
	}
	// Each block lexed alone under its document scope, as the renderer does.
	function lexDocument(source: string) {
		const { strings, context } = contextOf(source);
		return strings.map((block, index) => lexCapture(block, ext, null, null, context.scope(index)));
	}

	it('opens only the document', () => {
		expect(types(lexDocument('Intro.\n\n+++\nkey = 1\n+++\n\nafter').flatMap((each) => each.tokens as unknown as Tok[]))).not.toContain('code');
		expect(lexDocument('+++\nkey = 1\n+++\n\nafter')[0].tokens[0]).toMatchObject({ type: 'code', lang: 'toml' });
		expect(lexDocument('\n\n+++\nkey = 1\n+++\n\nafter')[0].tokens[0]).toMatchObject({ type: 'code', lang: 'toml' });
		expect(lexed('\n\n+++\nkey = 1\n+++\n\nafter', ext)[0]).toMatchObject({ type: 'code', lang: 'toml' });
	});

	it('is no heading in the table of contents', () => {
		const lexes = lexDocument('---\ntitle: About\n---\n\n# Real\n\n[[_TOC_]]\n');
		const tokens = lexes.flatMap((each) => each.tokens as unknown as Tok[]);
		expect(tokens[0]).toMatchObject({ type: 'code', lang: 'yaml' });
		const toc = tokens.at(-1)!;
		expect(toc.type).toBe('list');
		expect(types(toc.tokens ?? []).filter((type) => type === 'list_item')).toHaveLength(1);
	});

	it('rebuilds a block whose place as the first changes', () => {
		const [first] = lexDocument('+++\nkey = 1\n+++\n');
		const firstScope = contextOf('+++\nkey = 1\n+++\n').context.scope(0);
		const { context } = contextOf('Intro.\n\n+++\nkey = 1\n+++\n');
		expect(first.reads).toContain('s');
		expect(sameScopeReads(firstScope, context.scope(1), first.reads)).toBe(false);
		expect(sameScopeReads(firstScope, context.scope(0), first.reads)).toBe(true);
	});
});

describe('an autolink with a backslash', () => {
	it.each([
		['<https://example.invalid/a\\b>', 'https://example.invalid/a%5Cb'],
		['see https://example.invalid/a\\b now', 'https://example.invalid/a%5Cb'],
		['see www.example.invalid/a\\b now', 'http://www.example.invalid/a%5Cb']
	])('keeps it in %s as %s', (source, href) => {
		for (const extensions of [CHAT, FORGE]) {
			const link = lexed(source, extensions)[0].tokens!.find((token) => token.type === 'link')!;
			expect(link.href).toBe(href);
			expect(link.text).toContain('a\\b');
		}
	});
});
