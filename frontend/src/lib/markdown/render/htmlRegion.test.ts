import { beforeAll, describe, expect, it, vi, type MockedFunction } from 'vitest';
import type { StreamdownToken } from '../parser/lexer';
import { chatMarkdownTheme } from '../../components/chat/markdown/streamdownTheme';
import { EMBEDDED_HTML_EXTENSIONS } from '../parser/extensions/embeddedHtml';
import type { HtmlRegionToken } from '../parser/extensions/htmlRegion';
import { lex } from '../parser/lexer';
import {
	MEDIA_CLAIM_ATTR,
	htmlRegionNodes,
	parseHtmlRegion,
	sanitizeEmbeddedHtmlToken,
	type HtmlRegionNode,
	type SanitizeEmbeddedHtmlOptions
} from './htmlSanitize';

// An html region sanitized and parsed as one fragment around its islands,
// and both renderers' handling of it.

const regionOf = (src: string): HtmlRegionToken => {
	const token = lex(src, EMBEDDED_HTML_EXTENSIONS).find((each) => each.type === 'htmlRegion');
	expect(token, src).toBeDefined();
	return token as HtmlRegionToken;
};
const sanitized = (options?: SanitizeEmbeddedHtmlOptions) => (token: { raw: string; block?: boolean }) =>
	sanitizeEmbeddedHtmlToken(token, options);

// The nodes with each island as `[n]` and each HTML run as its string.
const outline = (nodes: HtmlRegionNode[]): unknown[] =>
	nodes.map((node) =>
		node.kind === 'island' ? `[${node.index}]` : node.kind === 'html' ? node.html : { [node.tag]: node.attrs, children: outline(node.children) }
	);

describe('parseHtmlRegion', () => {
	it('balances a wrapper across blocks, as a forge parses the whole comment', () => {
		const region = regionOf('<table>\n<tr><th>A</th><th>B</th></tr>\n<tr><td>\n\n**md in cell**\n\n</td><td><code>x</code></td></tr>\n</table>');
		const parsed = parseHtmlRegion(region.parts, sanitized());
		expect([...parsed.islands.values()]).toEqual([0]);
		expect([...parsed.islands.keys()][0].parentElement?.localName).toBe('td');
		expect(outline(htmlRegionNodes(parsed, ['*']))).toEqual([
			{
				table: {},
				children: [
					'\n',
					{
						tbody: {},
						children: [
							'<tr><th>A</th><th>B</th></tr>\n',
							{ tr: {}, children: [{ td: {}, children: ['\n\n', '[0]'] }, '<td><code class="sd-codespan-short">x</code></td>'] },
							'\n'
						]
					}
				]
			}
		]);
	});

	it('keeps script, event handlers and javascript: URLs inert', () => {
		const region = regionOf(
			'<div align="center" onclick="alert(1)">\n<script>alert(2)</script><img src="x" onerror="alert(3)"><a href="javascript:alert(4)">bad</a><blink>odd</blink>\n\n**island**\n\n</div>'
		);
		const parsed = parseHtmlRegion(region.parts, sanitized());
		expect(parsed.root.querySelector('script, blink, [onclick], [onerror]')).toBeNull();
		expect(parsed.root.querySelector('a')?.hasAttribute('href')).toBe(false);
		expect(parsed.root.querySelector('img[src]')).toBeNull();
		expect(parsed.root.textContent).not.toContain('alert(2)');
		expect(parsed.root.textContent).toContain('odd');
		const nodes = htmlRegionNodes(parsed, ['*']);
		expect(nodes).toHaveLength(1);
		expect(nodes[0]).toMatchObject({ kind: 'element', tag: 'div', attrs: { align: 'center' } });
		expect(Object.keys((nodes[0] as { attrs: object }).attrs)).toEqual(['align']);
	});

	it('honors no placeholder an author writes', () => {
		const region = regionOf('<div>\n\n**first**\n\n</div>');
		// A placeholder's nonce is per call: one read from an earlier render
		// is as unknown to the next as a guessed one.
		const earlier = parseHtmlRegion(region.parts, sanitized());
		const nonce = [...earlier.islands.keys()][0].getAttribute('data-markdown-island')!.split(':')[0];
		for (const forged of ['0', 'x:0', `${nonce}:0`]) {
			const authored = regionOf(`<div>\n<div data-markdown-island="${forged}">forged</div>\n\n**real**\n\n</div>`);
			for (const renderHtml of [sanitized(), true] as const) {
				const parsed = parseHtmlRegion(authored.parts, renderHtml);
				expect([...parsed.islands.values()], forged).toEqual([0]);
				const [placeholder] = parsed.islands.keys();
				expect(placeholder.textContent).toBe('');
				const nodes = outline(htmlRegionNodes(parsed, ['*']));
				expect(JSON.stringify(nodes).match(/\[0\]/g)).toHaveLength(1);
				expect(JSON.stringify(nodes)).toContain('forged');
			}
			// The sanitizer strips the authored attribute outright.
			expect(parseHtmlRegion(authored.parts, sanitized()).root.querySelectorAll('[data-markdown-island]')).toHaveLength(1);
		}
	});

	it('runs the destination hooks over the whole region', () => {
		const region = regionOf('<div align="center">\n<img alt="shot" src="/uploads/a.png">\n\n**caption**\n\n</div>');
		const claims: string[] = [];
		const parsed = parseHtmlRegion(
			region.parts,
			sanitized({
				destinations: [(token) => (claims.push(token.href), { ...token, href: `claim:${token.href}` })],
				allowedImagePrefixes: ['claim:']
			})
		);
		expect(claims).toEqual(['/uploads/a.png']);
		const img = parsed.root.querySelector('img')!;
		expect(img.getAttribute(MEDIA_CLAIM_ATTR)).toBe('claim:/uploads/a.png');
		expect(img.hasAttribute('src')).toBe(false);
		const [div] = htmlRegionNodes(parsed, ['*']) as { children: HtmlRegionNode[] }[];
		expect(div.children.find((node) => node.kind === 'html' && node.html.includes(MEDIA_CLAIM_ATTR))).toBeDefined();
	});

	it('drops an island its HTML drops, as the sanitizer drops a picture source', () => {
		const region = regionOf('<div>\n<picture>\n\n**gone**\n\n</picture>\n\n**kept**\n\n</div>');
		expect(region.tokens).toHaveLength(2);
		const parsed = parseHtmlRegion(region.parts, sanitized());
		expect([...parsed.islands.values()]).toEqual([1]);
	});

	it('keeps only allowlisted spine elements and attributes under renderHtml true', () => {
		const region = regionOf('<div onclick="alert(1)" align="right">\n<center>\n\n**x**\n\n</center></div>');
		expect(outline(htmlRegionNodes(parseHtmlRegion(region.parts, true), ['*']))).toEqual([
			{ div: { align: 'right' }, children: ['\n', '\n\n', '[0]'] }
		]);
	});
});

// The compact path's serializer, observed. The test setup already loads the
// renderer, so the mock reaches it only through a fresh module graph.
vi.mock('./staticHtml', async (importOriginal) => {
	const original = await importOriginal<typeof import('./staticHtml')>();
	return { ...original, renderStaticTokenHtml: vi.fn(original.renderStaticTokenHtml) };
});

describe('<Streamdown> html regions', () => {
	const CENTERED = '<div align="center">\n\n**markdown** inside div\n\n</div>\n\nafter';
	const VARIANTS = [
		{ static: true, compactStaticHtml: true },
		{ static: true, compactStaticHtml: false },
		{ parseIncompleteMarkdown: false }
	];
	let fresh: {
		render: typeof import('@testing-library/svelte').render;
		tick: typeof import('svelte').tick;
		Streamdown: typeof import('./Streamdown.svelte').default;
		serialize: MockedFunction<typeof import('./staticHtml').renderStaticTokenHtml>;
		extensions: typeof EMBEDDED_HTML_EXTENSIONS;
		sanitize: typeof sanitizeEmbeddedHtmlToken;
	};
	beforeAll(async () => {
		vi.resetModules();
		fresh = {
			render: (await import('@testing-library/svelte')).render,
			tick: (await import('svelte')).tick,
			Streamdown: (await import('./Streamdown.svelte')).default,
			serialize: vi.mocked((await import('./staticHtml')).renderStaticTokenHtml),
			extensions: (await import('../parser/extensions/embeddedHtml')).EMBEDDED_HTML_EXTENSIONS,
			sanitize: (await import('./htmlSanitize')).sanitizeEmbeddedHtmlToken
		};
		expect(vi.isMockFunction(fresh.serialize)).toBe(true);
	});
	// What the compact serializer returned for the block holding the region.
	const regionStatic = (): (string | null)[] =>
		fresh.serialize.mock.calls.flatMap(([tokens], call) =>
			(tokens as readonly StreamdownToken[]).some((token) => token.type === 'htmlRegion')
				? [fresh.serialize.mock.results[call].value as string | null]
				: []
		);
	const shown = async (props: Record<string, unknown>) => {
		fresh.serialize.mockClear();
		const view = fresh.render(fresh.Streamdown, {
			props: { content: CENTERED, theme: chatMarkdownTheme, extensions: fresh.extensions, ...props }
		});
		await fresh.tick();
		const root = view.container.firstElementChild!;
		const out = {
			centered: root.querySelector('div[align="center"] > p strong')?.textContent ?? null,
			paragraphs: [...root.querySelectorAll('p')].map((p) => p.textContent?.trim())
		};
		view.unmount();
		return out;
	};

	it.each(VARIANTS)('renders its islands and no HTML without renderHtml: %o', async (variant) => {
		expect(await shown(variant)).toEqual({ centered: null, paragraphs: ['markdown inside div', 'after'] });
		// The compact path serves the islands itself.
		if (variant.compactStaticHtml) expect(regionStatic()).toEqual([expect.stringContaining('<strong')]);
	});

	it.each(VARIANTS)('renders the whole region with renderHtml: %o', async (variant) => {
		const renderHtml = (token: { raw: string; block?: boolean }) => fresh.sanitize(token);
		expect(await shown({ ...variant, renderHtml })).toEqual({
			centered: 'markdown',
			paragraphs: ['markdown inside div', 'after']
		});
		// The region needs its component.
		if (variant.compactStaticHtml) expect(regionStatic()).toEqual([null]);
	});
});
