import { describe, expect, it } from 'vitest';
import { lex, parseBlocks, createParseBlocksCache, createProvenAppend, type Extension } from '../../index';
import { markedStrikethrough } from './strikethrough';
import { markedInlineDiff } from './inlineDiff';
import { markedMultilineBlockquote } from './multilineBlockquote';
import { markedFrontMatter } from './frontMatter';
import { markedInapplicableTaskList } from './list';
import { markedTableTaskCells } from './tableTaskCells';
import { markedColorChips, markedCssColorChips } from './colorChips';
import { markedDescriptionLists } from './descriptionLists';
import { markedJsonTable } from './jsonTable';

// GitHub's and GitLab's own Markdown syntax, each an opt-in extension: what
// it parses on its surface and what stays as agent chat parses it.

type Tok = {
	type: string;
	raw: string;
	text?: string;
	tag?: string;
	lang?: string;
	color?: string;
	checked?: boolean;
	task?: boolean;
	inapplicable?: boolean;
	tokens?: Tok[];
};

const blocks = (source: string, extensions: Extension[]): Tok[] => lex(source, extensions) as unknown as Tok[];
const inline = (source: string, extensions: Extension[]): Tok[] => blocks(source, extensions)[0]?.tokens ?? [];
const rawSum = (tokens: Tok[]) => tokens.map((token) => token.raw).join('');

describe('GFM strikethrough', () => {
	const ext = [markedStrikethrough];

	it.each([
		['~single~', 'single'],
		['~~double~~', 'double'],
		['~Scratch this.~', 'Scratch this.']
	])('strikes %s', (source, text) => {
		const [del] = inline(source, ext);
		expect(del).toMatchObject({ type: 'del', raw: source, text });
	});

	it('strikes inside a word and leaves a run of three as text', () => {
		const tokens = inline('H~2~O and ~~~three~~~', ext);
		expect(tokens.filter((token) => token.type !== 'text').map((token) => token.raw)).toEqual(['~2~']);
		expect(rawSum(tokens)).toBe('H~2~O and ~~~three~~~');
	});

	it.each(['~ spaced ~', '~~open~', 'a ~ b', 'a ~~~three~~~ b', '~5s or ~10s'])('leaves %s as text', (source) => {
		expect(new Set(inline(source, ext).map((token) => token.type))).toEqual(new Set(['text']));
	});

	it('lets a code span that crosses the closing run win', () => {
		const tokens = inline('~a `b~ c` d', ext);
		expect(tokens.some((token) => token.type === 'del')).toBe(false);
		expect(tokens.find((token) => token.type === 'codespan')?.text).toBe('b~ c');
		expect(rawSum(tokens)).toBe('~a `b~ c` d');
	});

	it('runs ahead of subscript; agent chat keeps subscript and `~~`-only strikethrough', () => {
		expect(inline('~sub~ and ~~del~~', []).map((token) => token.type)).toEqual(['sub', 'text', 'del']);
		expect(inline('~sub~ and ~~del~~', ext).map((token) => token.type)).toEqual(['del', 'text', 'del']);
	});
});

describe('GitLab inline diff', () => {
	const ext = [markedInlineDiff];

	it.each([
		['{+ addition 1 +}', 'htmlInline', ' addition 1 '],
		['[+ addition 2 +]', 'htmlInline', ' addition 2 '],
		['{- deletion 3 -}', 'del', ' deletion 3 '],
		['[- deletion 4 -]', 'del', ' deletion 4 '],
		['{+ Text with escaped \\`backticks\\` inside +}', 'htmlInline', ' Text with escaped \\`backticks\\` inside ']
	])('marks %s', (source, type, text) => {
		const [token] = inline(source, ext);
		expect(token).toMatchObject({ type, raw: source, text });
		if (type === 'htmlInline') expect(token.tag).toBe('ins');
	});

	it('marks a diff mid-sentence and keeps the run raws whole', () => {
		const source = 'we {- removed -} the old and {+ added +} the new.';
		const tokens = inline(source, ext);
		expect(tokens.map((token) => token.type)).toEqual(['text', 'del', 'text', 'htmlInline', 'text']);
		expect(rawSum(tokens)).toBe(source);
	});

	it.each([
		['mixed wrappers', '{+ addition +]'],
		['the other mix', '[- deletion -}'],
		['a code span inside', '{+ Text with `backticks` inside +}'],
		['Markdown inside', '{+ **bold** and [link](https://example.invalid/) inside +}'],
		['emphasis crossing the closer', '{+ a *b +} c*'],
		['a code span crossing the closer', '{+ a `b +} c`'],
		['a link', '[+ text +](https://example.invalid/)'],
		['an escaped opener', '\\{+ escaped +}'],
		['a closer on the next line', '{+ one\ntwo +}']
	])('leaves %s as text', (_name, source) => {
		const types = inline(source, ext).map((token) => token.type);
		expect(types).not.toContain('htmlInline');
		expect(types).not.toContain('del');
	});

	it('is text in a code span and in agent chat', () => {
		expect(inline('`{+ in code +}`', ext).map((token) => token.type)).toEqual(['codespan']);
		expect(inline('{+ added +}', []).map((token) => token.type)).toEqual(['text']);
	});
});

describe('GitLab multiline blockquote', () => {
	const ext = [markedMultilineBlockquote];

	it('quotes everything between the fences, blank lines included', () => {
		const source = '>>>\nIf you paste\n\nthat spans lines,\n>>>\n\nafter';
		const [quote, after] = blocks(source, ext);
		expect(quote.type).toBe('blockquote');
		expect(quote.raw + after.raw).toBe(source);
		expect(quote.raw).toMatch(/^>>>\nIf you paste\n\nthat spans lines,\n>>>\n\n?$/);
		expect(quote.tokens!.filter((token) => token.type === 'paragraph').map((token) => token.text)).toEqual([
			'If you paste',
			'that spans lines,'
		]);
		expect(after).toMatchObject({ type: 'paragraph', text: 'after' });
	});

	it('holds a heading, a list, a fence with a fence line, and a nested quote', () => {
		const source = '>>>\n## Heading\n\n- item\n\n```\n>>>\n```\n\n> nested\n>>>\nafter';
		const [quote, after] = blocks(source, ext);
		expect(quote.tokens!.map((token) => token.type).filter((type) => type !== 'space')).toEqual([
			'heading',
			'list',
			'code',
			'blockquote'
		]);
		expect(quote.tokens!.find((token) => token.type === 'code')?.text).toBe('>>>');
		expect(after.text).toBe('after');
	});

	it('runs to the end without a closing fence', () => {
		const [quote] = blocks('Text.\n\n>>>\nunterminated\n\nstill quoted', ext).slice(1);
		expect(quote.type).toBe('blockquote');
		expect(quote.raw).toBe('>>>\nunterminated\n\nstill quoted');
	});

	it('leaves `>>>` with text after it, and agent chat, to nested quotes', () => {
		expect(blocks('>>> text\n', ext)[0].tokens![0].type).toBe('blockquote');
		const [chat] = blocks('>>>\nquoted\n>>>', []);
		expect(chat.tokens![0].type).toBe('blockquote');
		expect(chat.raw).not.toContain('quoted');
	});
});

describe('GitLab front matter', () => {
	const ext = [markedFrontMatter];

	it.each([
		['---\ntitle: About\nexample:\n  language: yaml\n---\n', 'yaml', 'title: About\nexample:\n  language: yaml'],
		['+++\ntitle = "About"\n+++\n', 'toml', 'title = "About"'],
		[';;;\n{\n  "title": "About"\n}\n;;;\n', 'json', '{\n  "title": "About"\n}'],
		['---toml\nkey = 1\n---\n', 'toml', 'key = 1'],
		['---\nkey: 1\n...\n', 'yaml', 'key: 1']
	])('renders %j as code', (front, lang, text) => {
		const [code, body] = blocks(`${front}\nBody.`, ext);
		// The blank line after a block joins its raw.
		expect(code).toMatchObject({ type: 'code', raw: `${front}\n`, lang, text });
		expect(body).toMatchObject({ type: 'paragraph', text: 'Body.' });
	});

	it.each([
		['not at the start', 'Intro.\n\n+++\nkey = 1\n+++\n'],
		['without a closing line', '---\nkey: 1\n\nBody.'],
		['inside a quote', '> +++\n> key = 1\n> +++\n'],
		['inside a list item', '- +++\n  key = 1\n  +++\n']
	])('is not front matter %s', (_name, source) => {
		const types: string[] = [];
		const visit = (tokens: Tok[]) => {
			for (const token of tokens) {
				types.push(token.type);
				if (token.tokens) visit(token.tokens);
			}
		};
		visit(blocks(source, ext));
		expect(types).not.toContain('code');
	});

	it('is a rule and a heading in agent chat', () => {
		expect(blocks('---\ntitle: About\n---\n', []).map((token) => token.type)).toEqual(['hr', 'heading']);
	});
});

describe('GitLab task lists', () => {
	const ext = [markedInapplicableTaskList];
	const items = (source: string, extensions: Extension[]) => blocks(source, extensions)[0].tokens!;

	it('reads `[~]` as an unchecked task whose text is struck through', () => {
		const [done, upper, inapplicable, open] = items('- [x] done\n- [X] upper\n- [~] skipped\n- [ ] open', ext);
		expect([done, upper, inapplicable, open].map((item) => [item.task, item.checked])).toEqual([
			[true, true],
			[true, true],
			[true, false],
			[true, false]
		]);
		expect(inapplicable.inapplicable).toBe(true);
		const [text] = inapplicable.tokens!;
		expect(text.tokens!.map((token) => [token.type, token.raw])).toEqual([['del', 'skipped']]);
		expect(text.tokens![0].tokens![0]).toMatchObject({ type: 'text', text: 'skipped' });
		expect(open.tokens![0].tokens![0].type).toBe('text');
	});

	it('strikes a nested, an ordered and a loose `[~]` task', () => {
		const nested = items('- [ ] parent\n  - [~] child', ext)[0].tokens!.find((token) => token.type === 'list')!;
		expect(nested.tokens![0].tokens![0].tokens![0].type).toBe('del');
		const ordered = items('1. [x] one\n1. [~] two', ext)[1];
		expect(ordered.tokens![0].tokens![0].type).toBe('del');
		const loose = items('- [~] one\n\n  more\n- two', ext)[0];
		expect(loose.tokens![0].type).toBe('paragraph');
		expect(loose.tokens![0].tokens![0].type).toBe('del');
	});

	it('leaves a thematic break to the rule ahead of which it runs', () => {
		expect(blocks('- - -\n\n* * *\n\n- item', ext).map((token) => token.type)).toEqual(['hr', 'hr', 'list']);
	});

	it('reads a box with no text as a task', () => {
		const [item] = items('- [ ]', ext);
		expect(item).toMatchObject({ task: true, checked: false, text: '' });
	});

	it('leaves both forms as text by default', () => {
		const [inapplicable, empty] = items('- [~] skipped\n- [ ]', []);
		expect(inapplicable.task).toBe(false);
		expect(inapplicable.text).toBe('[~] skipped');
		expect(empty.task).toBe(false);
	});
});

describe('GitLab task boxes in table cells', () => {
	const ext = [markedTableTaskCells];
	const TABLE = '| Done | Task |\n| --- | --- |\n| [x] | one |\n| [ ] | two |\n| [~] | three |\n| [x] more | four |';
	const cells = (extensions: Extension[]) =>
		blocks(TABLE, extensions)[0].tokens!.find((section) => section.type === 'tbody')!.tokens!.map((row) => row.tokens![0].tokens!);

	it('renders a cell holding only a box as a checkbox', () => {
		const [checked, open, inapplicable, more] = cells(ext);
		expect(checked).toEqual([{ type: 'checkbox', raw: '[x]', checked: true }]);
		expect(open).toEqual([{ type: 'checkbox', raw: '[ ]', checked: false }]);
		expect(inapplicable).toEqual([{ type: 'checkbox', raw: '[~]', checked: false }]);
		expect(more.map((token) => token.type)).not.toContain('checkbox');
	});

	it('leaves a box outside a table cell, and every cell by default, as text', () => {
		expect(inline('[x]', ext).map((token) => token.type)).not.toContain('checkbox');
		for (const cell of cells([])) expect(cell.map((token) => token.type)).not.toContain('checkbox');
	});
});

describe('color chips', () => {
	const chip = (source: string, extensions: Extension[]) => inline(source, extensions)[0];

	it.each(['`#0969DA`', '`#ffffff`', '`rgb(9, 105, 218)`', '`hsl(212, 92%, 45%)`', '`rgb(1,2,3)`'])(
		'GitHub: %s gets a swatch',
		(source) => {
			const token = chip(source, [markedColorChips]);
			expect(token).toMatchObject({ type: 'codespan', raw: source, color: source.slice(1, -1) });
		}
	);

	it.each(['`#F00`', '`#F00A`', '`#FF0000AA`', '`RGB(0,255,0)`', '`HSL(540,70%,50%)`', '`#GGGGGG`', '`red`', '`#0969DA `'])(
		'GitHub: %s is plain code',
		(source) => {
			const token = chip(source, [markedColorChips]);
			expect(token.type).toBe('codespan');
			expect(token.color).toBeUndefined();
		}
	);

	it.each([
		['`#F00`', '#F00'],
		['`#F00A`', '#F00A'],
		['`#FF0000AA`', '#FF0000AA'],
		['`RGB(0,255,0)`', 'RGB(0,255,0)'],
		['`RGB(0%,100%,0%)`', 'RGB(0%,100%,0%)'],
		['`RGBA(0,255,0,0.3)`', 'RGBA(0,255,0,0.3)'],
		['`HSL(540,70%,50%)`', 'HSL(540,70%,50%)'],
		['`HSLA(540,70%,50%,0.3)`', 'HSLA(540,70%,50%,0.3)'],
		['`hsl(1.5rad, 10%, 20%)`', 'hsl(1.5rad, 10%, 20%)'],
		['` #F00 `', '#F00']
	])('GitLab: %s gets a swatch', (source, color) => {
		expect(chip(source, [markedCssColorChips])).toMatchObject({ type: 'codespan', raw: source, color });
	});

	it.each(['`#GGGGGG`', '`# F00`', '`red`', '`#12`', '`rgb(256,0,0)`', '`rgb(1, 2, 3);x`'])('GitLab: %s is plain code', (source) => {
		expect(chip(source, [markedCssColorChips]).color).toBeUndefined();
	});

	it('GitLab: a backslash escapes a color and is not shown', () => {
		expect(chip('`\\#FF0000`', [markedCssColorChips])).toEqual({ type: 'codespan', raw: '`\\#FF0000`', text: '#FF0000' });
		expect(chip('`\\#FF0000`', [markedColorChips])).toEqual({ type: 'codespan', raw: '`\\#FF0000`', text: '\\#FF0000' });
	});

	it('is plain code in agent chat and outside code', () => {
		expect(chip('`#0969DA`', []).color).toBeUndefined();
		expect(inline('#FF0000 outside', [markedCssColorChips]).map((token) => token.type)).toEqual(['text']);
	});
});

describe('GitLab description lists', () => {
	const ext = [markedDescriptionLists];
	const SOURCE = 'Fruits\n: apple\n: orange\n\nVegetables\n: broccoli\n: kale\n\nSpaced\n: one\n\n: two\n\nAfter.';
	const text = (token: Tok): string =>
		token.tokens ? token.tokens.map(text).join('') : (token.text ?? '');

	it('reads terms and their details as one list', () => {
		const [list, after] = blocks(SOURCE, ext);
		expect(list.type).toBe('descriptionList');
		expect(list.raw + after.raw).toBe(SOURCE);
		expect(
			list.tokens!.map((group) => group.tokens!.map((token) => [token.type, text(token)]))
		).toEqual([
			[['descriptionTerm', 'Fruits'], ['descriptionDetail', 'apple'], ['descriptionDetail', 'orange']],
			[['descriptionTerm', 'Vegetables'], ['descriptionDetail', 'broccoli'], ['descriptionDetail', 'kale']],
			[['descriptionTerm', 'Spaced'], ['descriptionDetail', 'one'], ['descriptionDetail', 'two']]
		]);
		// A detail renders without a paragraph, as GitLab's does.
		expect(list.tokens![0].tokens![1].tokens![0].type).toBe('text');
		expect(after).toMatchObject({ type: 'paragraph', text: 'After.' });
	});

	it('continues a detail on lazy and indented lines', () => {
		const [list] = blocks('Term\n: first line\nwraps here\n\n  second paragraph\n\nAfter.', ext);
		const [detail] = list.tokens![0].tokens!.slice(1);
		expect(detail.tokens!.filter((token) => token.type === 'paragraph').map((token) => token.text)).toEqual([
			'first line\nwraps here',
			'second paragraph'
		]);
	});

	it.each([
		['a heading', '# Heading\n: detail'],
		['a list', '- item\n: detail'],
		['a line with no blank after the colon', 'Term\n:detail'],
		['a colon line first', ': detail']
	])('is not a term after %s', (_name, source) => {
		expect(blocks(source, ext).map((token) => token.type)).not.toContain('descriptionList');
	});

	it('is a paragraph in agent chat', () => {
		expect(blocks('Fruits\n: apple', []).map((token) => token.type)).toEqual(['paragraph']);
	});
});

describe('GitLab JSON tables', () => {
	const ext = [markedJsonTable];
	const fence = (json: string) => '```json:table\n' + json + '\n```\n';
	const sections = (token: Tok) => token.tokens!;
	const cellText = (cell: Tok): string => cell.tokens!.map((token) => token.text ?? cellText(token)).join('');

	it('renders fields, items, a caption and Markdown cells', () => {
		const source = fence(
			JSON.stringify({
				fields: [{ key: 'a', label: 'AA', sortable: true }, { key: 'b', label: 'BB' }, 'c'],
				items: [{ a: '11', b: '**22**', c: 33 }, { a: '#1', b: '222' }],
				markdown: true,
				caption: 'Custom caption'
			})
		);
		const [table] = blocks(source, ext);
		expect(table).toMatchObject({ type: 'table', raw: source, align: [null, null, null] });
		const [caption, head, body] = sections(table);
		expect(caption.type).toBe('caption');
		expect(cellText(caption)).toBe('Custom caption');
		expect(head.tokens![0].tokens!.map(cellText)).toEqual(['AA', 'BB', 'c']);
		const rows = body.tokens!.map((row) => row.tokens!);
		expect(rows.map((row) => row.map(cellText))).toEqual([
			['11', '22', '33'],
			['#1', '222', '']
		]);
		expect(rows[0][1].tokens![0].type).toBe('strong');
	});

	it('takes the columns from the first item and keeps Markdown as text by default', () => {
		const [table] = blocks(fence('{"items": [{"a": "11", "b": "**22**"}, {"a": "211", "c": "x"}]}'), ext);
		const [head, body] = sections(table);
		expect(head.tokens![0].tokens!.map(cellText)).toEqual(['a', 'b']);
		expect(body.tokens!.map((row) => row.tokens!.map(cellText))).toEqual([
			['11', '**22**'],
			['211', '']
		]);
	});

	it.each([
		['invalid JSON', '{"items": [{"a": 1}],}'],
		['no items', '{"fields": ["a"]}'],
		['not an object', '[1, 2]']
	])('stays a code block with %s', (_name, json) => {
		expect(blocks(fence(json), ext)[0].type).toBe('code');
	});

	it('is a code block in agent chat', () => {
		expect(blocks(fence('{"items": [{"a": 1}]}'), [])[0]).toMatchObject({ type: 'code', lang: 'json:table' });
	});
});

describe('block splitting with the GitLab block syntax', () => {
	const ext: Extension[] = [markedMultilineBlockquote, markedFrontMatter, markedDescriptionLists, markedJsonTable];
	const DOCUMENTS = [
		'---\ntitle: About\n\nmore: yes\n---\n\nBody.\n',
		'Lead.\n\n>>>\nquoted\n\nstill quoted\n>>>\n\nafter\n',
		'Fruits\n: apple\n\n: orange\n\nVegetables\n: kale\n\nAfter.\n',
		'```json:table\n{"items": [{"a": 1}]}\n```\n\n>>>\nopen to the end\n\nmore\n'
	];

	it('holds a construct that spans blank lines in one block', () => {
		expect(parseBlocks(DOCUMENTS[0], ext)[0]).toBe('---\ntitle: About\n\nmore: yes\n---\n\n');
		expect(parseBlocks(DOCUMENTS[1], ext)[1]).toBe('>>>\nquoted\n\nstill quoted\n>>>\n\n');
		expect(parseBlocks(DOCUMENTS[2], ext)[0]).toBe('Fruits\n: apple\n\n: orange\n\nVegetables\n: kale\n\n');
	});

	it('splits an appended document as a fresh parse does, at every chunk size', () => {
		for (const document of DOCUMENTS) {
			for (const size of [1, 2, 3, 7, 16]) {
				const cache = createParseBlocksCache();
				let source = '';
				for (let at = 0; at < document.length; at += size) {
					const append = createProvenAppend(source, document.slice(at, at + size));
					source = append.next;
					expect(parseBlocks(source, ext, cache, append), JSON.stringify(source)).toEqual(parseBlocks(source, ext));
				}
			}
		}
	});

	it('lexes each block as the whole document lexes', () => {
		for (const document of DOCUMENTS) {
			const whole = blocks(document, ext).filter((token) => token.type !== 'space');
			const split = parseBlocks(document, ext).flatMap((block) => blocks(block, ext));
			expect(JSON.stringify(split)).toBe(JSON.stringify(whole));
		}
	});
});
