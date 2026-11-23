import { describe, expect, it } from 'vitest';
import { lex, markedForgeGrammar, type Extension } from '../../index';
import { markedCommonmarkList, markedInapplicableTaskList } from './list';
import { markedGithubAlert, markedGitlabAlert } from './alert';
import { markedGithubMath, markedGitlabMath } from './math';
import { EMBEDDED_HTML_EXTENSIONS } from './embeddedHtml';

// Block structure as each forge reads it, on its own surface: list
// numbering, alerts and math. Agent chat (no extension) keeps its own
// readings, held beside each forge case.

type Tok = {
	type: string;
	raw: string;
	text?: string;
	start?: number;
	value?: number | null;
	variant?: string;
	title?: string;
	isInline?: boolean;
	displayMode?: boolean;
	tag?: string;
	href?: string;
	tokens?: Tok[];
	parts?: (string | Tok)[];
};

const GITHUB: Extension[] = [markedForgeGrammar, markedCommonmarkList, markedGithubAlert, ...markedGithubMath];
const GITLAB: Extension[] = [markedForgeGrammar, markedInapplicableTaskList, markedGitlabAlert, ...markedGitlabMath];
const CHAT: Extension[] = [];

const blocks = (source: string, extensions: Extension[]): Tok[] => lex(source, extensions) as unknown as Tok[];
const inline = (source: string, extensions: Extension[]): Tok[] => blocks(source, extensions)[0]?.tokens ?? [];
const rawSum = (tokens: Tok[]) => tokens.map((token) => token.raw).join('');
/** The block types of a lone list's first item. */
const itemBlocks = (source: string, extensions: Extension[]) =>
	(blocks(source, extensions)[0].tokens?.[0].tokens ?? []).map((token) => token.type).filter((type) => type !== 'space');
const lists = (source: string, extensions: Extension[]) =>
	blocks(source, extensions)
		.filter((token) => token.type === 'list')
		.map((list) => ({ start: list.start, values: (list.tokens ?? []).map((item) => item.value) }));

describe('list numbering', () => {
	it.each([
		['GitHub', GITHUB],
		['GitLab', GITLAB]
	] as const)('numbers from the first item on %s, the typed numbers after it ignored', (_name, ext) => {
		expect(lists('1. a\n1. b\n1. c', ext)).toEqual([{ start: 1, values: [null, null, null] }]);
		expect(lists('3. three\n\n7. seven\n8. eight', ext)).toEqual([{ start: 3, values: [null, null, null] }]);
		expect(lists('0. zero\n5. one', ext)).toEqual([{ start: 0, values: [null, null] }]);
	});

	it.each([
		['GitHub', GITHUB],
		['GitLab', GITLAB]
	] as const)('starts a new list at a new bullet character on %s', (_name, ext) => {
		const source = '* star\n+ plus\n- dash\n- dash two';
		const tokens = blocks(source, ext);
		expect(tokens.map((token) => (token.tokens ?? []).length)).toEqual([1, 1, 2]);
		expect(rawSum(tokens)).toBe(source);
		// A delimiter change already started one.
		expect(lists('1. a\n2) b', ext)).toHaveLength(2);
	});

	it('keeps the typed numbers and one list across bullets in agent chat', () => {
		expect(lists('1. a\n1. b', CHAT)).toEqual([{ start: 1, values: [1, 1] }]);
		expect(lists('3. three\n\n7. seven', CHAT)).toEqual([{ start: 3, values: [3, 7] }]);
		expect(blocks('* star\n+ plus\n- dash', CHAT)).toHaveLength(1);
	});

	it('numbers a nested list the same way', () => {
		const [outer] = blocks('1. a\n   4. x\n   4. y\n2. b', GITHUB);
		const nested = (outer.tokens?.[0].tokens ?? []).find((token) => token.type === 'list');
		expect(nested?.start).toBe(4);
		expect(nested?.tokens?.map((item) => item.value)).toEqual([null, null]);
	});
});

describe('alerts', () => {
	const alert = (source: string, ext: Extension[]) => blocks(source, ext).find((token) => token.type === 'alert');

	it('reads a GitHub alert only with its marker alone on the first line of a top-level quote', () => {
		expect(alert('> [!note]\n> Lowercase body.', GITHUB)).toMatchObject({ variant: 'note' });
		expect(alert('> [!NOTE] text on the marker line\n> body', GITHUB)).toBeUndefined();
		expect(alert('- item\n\n  > [!NOTE]\n  > in a list', GITHUB)).toBeUndefined();
		expect(itemBlocks('- item\n\n  > [!NOTE]\n  > in a list', GITHUB)).toEqual(['paragraph', 'blockquote']);
		expect(alert('> > [!NOTE]\n> > nested', GITHUB)).toBeUndefined();
		expect(alert('>>> [!note] title\nbody', GITHUB)).toBeUndefined();
		expect(alert('> **[!NOTE]**\n> decorated', GITHUB)).toBeUndefined();
		expect(alert('> [!UNKNOWN]\n> body', GITHUB)).toBeUndefined();
	});

	it('reads a GitHub alert inside HTML, which a forge parses at the top level', () => {
		const [details] = blocks('<details>\n<summary>s</summary>\n\n> [!WARNING]\n> careful\n\n</details>', [
			...GITHUB,
			...EMBEDDED_HTML_EXTENSIONS
		]);
		expect(details.tokens?.find((token) => token.type === 'alert')).toMatchObject({ variant: 'warning' });
		const [region] = blocks('<div align="center">\n\n> [!TIP]\n> centered\n\n</div>', [...GITHUB, ...EMBEDDED_HTML_EXTENSIONS]);
		expect(region.tokens?.[0]).toMatchObject({ type: 'alert', variant: 'tip' });
	});

	it('reads the text on a GitLab marker line as the title, and a nested alert', () => {
		const titled = alert('> [!warning] Data deletion\n> Body paragraph.', GITLAB);
		expect(titled).toMatchObject({ variant: 'warning', title: 'Data deletion', raw: '> [!warning] Data deletion\n> Body paragraph.' });
		expect(titled?.tokens?.map((token) => [token.type, token.text])).toEqual([['paragraph', 'Body paragraph.']]);
		expect(alert('> [!NOTE]\n> plain', GITLAB)).not.toHaveProperty('title');
		expect(itemBlocks('- item\n\n  > [!NOTE]\n  > in a list', GITLAB)).toEqual(['paragraph', 'alert']);
		expect(alert('> [!UNKNOWN] x\n> body', GITLAB)).toBeUndefined();
	});

	it('keeps agent chat alerts as they were', () => {
		const chat = alert('> [!NOTE] text on the marker line\n> body', CHAT);
		expect(chat).toMatchObject({ variant: 'note' });
		expect(chat).not.toHaveProperty('title');
		expect(chat?.tokens?.[0].text).toContain('text on the marker line');
		expect(alert('- item\n\n  > [!NOTE]\n  > x', CHAT)).toBeUndefined();
		expect(itemBlocks('- item\n\n  > [!NOTE]\n  > x', CHAT)).toEqual(['paragraph', 'alert']);
	});

	it.each([
		['GitHub', GITHUB],
		['GitLab', GITLAB]
	] as const)('consumes exactly its source on %s', (_name, ext) => {
		for (const [source, raw] of [
			['> [!TIP]\n> a\n\nafter', '> [!TIP]\n> a'],
			['> [!NOTE]\nlazy line\n\nafter', '> [!NOTE]\nlazy line']
		]) {
			expect(blocks(source, ext).map((token) => token.raw), source).toEqual([raw, 'after']);
		}
		expect(blocks('> [!warning] Title\n\nafter', GITLAB).map((token) => [token.type, token.raw])).toEqual([
			['alert', '> [!warning] Title'],
			['paragraph', 'after']
		]);
	});
});

describe('forge math', () => {
	const math = (source: string, ext: Extension[]) =>
		inline(source, ext)
			.filter((token) => token.type === 'math')
			.map((token) => [token.text, token.isInline]);

	it.each([
		['GitHub', GITHUB],
		['GitLab', GITLAB]
	] as const)('reads $`…`$ and a math fence as math on %s', (_name, ext) => {
		expect(math('and $`c^2 + \\$4`$.', ext)).toEqual([['c^2 + \\$4', true]]);
		expect(blocks('```math\nx = y\n```', ext)).toMatchObject([{ type: 'math', text: 'x = y', isInline: false }]);
		expect(blocks('~~~ math\nx\n~~~', ext)).toMatchObject([{ type: 'math', isInline: false }]);
		expect(blocks('```mathematica\nx\n```', ext)).toMatchObject([{ type: 'code' }]);
	});

	it.each([
		['GitHub', GITHUB],
		['GitLab', GITLAB]
	] as const)('delimits $…$ as the forges do on %s', (_name, ext) => {
		expect(math('Prices: it costs $5 and then $10 more.', ext)).toEqual([]);
		expect(math('split $</span>100, we calculate $100/2$', ext)).toEqual([['100/2', true]]);
		expect(math('Invalid: $\\frac{1}{$ and $x$', ext)).toEqual([
			['\\frac{1}{', true],
			['x', true]
		]);
		expect(math('a $ x$ and $x $ b', ext)).toEqual([]);
		// Chat reads these as math; the forges do not.
		expect(math('a $ x$ b', CHAT)).toHaveLength(1);
		expect(math('the $PATH$ and', ext)).toEqual([['PATH', true]]);
	});

	it('reads $\\` and \\`$ as math, with GitHub resolving the escapes', () => {
		const source = 'uses $\\` and \\`$ delimiters: $`x`$';
		expect(math(source, GITHUB)).toEqual([
			['` and `', true],
			['x', true]
		]);
		expect(math(source, GITLAB)).toEqual([
			['\\` and \\`', true],
			['x', true]
		]);
		// Agent chat reads the prose between the forms as math.
		expect(math(source, CHAT)).toEqual([['delimiters:', true]]);
	});

	it('reads $$…$$ inside a line as display math in the line on both forges', () => {
		for (const forge of [GITHUB, GITLAB]) {
			expect(inline('inline $$a^2$$ here', forge).find((token) => token.type === 'math')).toMatchObject({ isInline: true, displayMode: true });
		}
		// Agent chat typesets it at text size.
		expect(inline('inline $$a^2$$ here', CHAT).find((token) => token.type === 'math')).toMatchObject({ isInline: true, displayMode: false });
	});

	it('keeps agent chat math as it was', () => {
		expect(blocks('```math\nx\n```', CHAT)).toMatchObject([{ type: 'code', lang: 'math' }]);
		expect(math('and $`c^2`$.', CHAT)).toEqual([]);
	});

	it('sums inline raws to the source', () => {
		for (const ext of [GITHUB, GITLAB]) {
			for (const source of ['a $x$ b $5 c $$y$$ d $`z`$ e $', 'cost $5 and $ x$ y']) {
				expect(rawSum(inline(source, ext))).toBe(source);
			}
		}
	});
});

describe('an html region around Markdown', () => {
	it('links the heading an <a href> wraps', () => {
		const [region] = blocks('<a href="https://example.invalid/">\n\n## Heading inside an anchor\n\n</a>\n', EMBEDDED_HTML_EXTENSIONS);
		expect(region.type).toBe('htmlRegion');
		expect(region.parts?.map((part) => (typeof part === 'string' ? part : part.type))).toEqual([
			'<a href="https://example.invalid/">\n\n',
			'heading',
			'</a>\n'
		]);
	});

	it('leaves an <a> that closes in its own block alone', () => {
		expect(blocks('<a href="https://example.invalid/">\n<img src="https://example.invalid/b.svg">\n</a>\n\n## after', EMBEDDED_HTML_EXTENSIONS).map((token) => token.type)).toEqual([
			'html',
			'heading'
		]);
	});
});

// Every surface: the splitter is the table extension's own.
describe('table cells', () => {
	const cells = (source: string) => {
		const [table] = blocks(source, CHAT);
		const body = (table.tokens ?? []).find((section) => section.type === 'tbody');
		return (body?.tokens ?? []).map((row) => (row.tokens ?? []).map((cell) => cell.text));
	};

	it('reads a lone backtick as text, so the pipe after it splits the row', () => {
		expect(cells('| Name | Character |\n| --- | --- |\n| Backtick | `         |\n| Pipe | \\| |')).toEqual([
			['Backtick', '`'],
			['Pipe', '|']
		]);
		expect(cells('| a | b |\n| - | - |\n| `` ` | c |')).toEqual([['`` `', 'c']]);
	});

	it('keeps a pipe inside a closed code span in its cell', () => {
		expect(cells('| a | b |\n| - | - |\n| `x | y` | z |\n| ``p ` | q`` | r |')).toEqual([
			['`x | y`', 'z'],
			['``p ` | q``', 'r']
		]);
	});
});
