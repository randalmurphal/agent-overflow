/**
 * What a forge keeps of raw HTML: the one element policy both embedded-HTML
 * paths read, so an element or attribute is never kept on one and dropped
 * on the other. The inline tokenizer (embeddedHtml.ts) turns an element pair
 * in a paragraph into a token; the sanitizer (render/htmlSanitize.ts)
 * renders an HTML block. Read from each forge's render of the same HTML.
 *
 * An element is kept with the attributes its rule lists, removed with its
 * content (`'remove'`), or, unlisted, stripped: its children show in its
 * place. A kept element with an inline token that carries no attributes (a
 * code span, a link's title only, an image's own fields) lists only what
 * that token carries, so a block keeps no more than a paragraph does.
 */
import type { DocumentForge } from '../documentScope';

export type HtmlForge = Exclude<DocumentForge, null>;

/** The attributes a kept element keeps, or removal with its content. */
export type HtmlElementRule = readonly string[] | 'remove';

export interface HtmlPolicy {
	/** Each listed element's rule; an element not listed is stripped. */
	readonly elements: ReadonlyMap<string, HtmlElementRule>;
	/** GFM's disallowed raw HTML rule, which GitHub applies: such a tag
	 * (`<textarea>`, `<iframe>`) shows as its source text. */
	readonly tagFilter: boolean;
	/** An `<img>` class value the forge styles as a float, by the side it
	 * floats to. Only the whole value counts: GitLab drops a `class` that
	 * holds anything else. No `class` is ever kept. */
	readonly imageFloats: ReadonlyMap<string, 'left' | 'right'>;
	/** A line holding one `<img>` tag and nothing else, at a block start,
	 * is a paragraph of that image, each such line of a run its own
	 * (GitHub). Otherwise the line starts an HTML block, as CommonMark
	 * reads it. */
	readonly imageLines: boolean;
	/** An `<img>` its stripped wrappers (`<figure>`) leave at the top of an
	 * HTML block is a paragraph of its own, so text after it starts a line
	 * (GitHub). */
	readonly imageParagraphs: boolean;
}

/** What both forges keep on an element whose rendering can carry it. */
const GLOBAL = ['title', 'dir', 'lang'] as const;
const kept = (...attributes: string[]): readonly string[] => [...GLOBAL, ...attributes];

const rules = (names: readonly string[], rule: HtmlElementRule): [string, HtmlElementRule][] => names.map((name) => [name, rule]);

/** Both forges. */
const COMMON: [string, HtmlElementRule][] = [
	['a', ['href', 'title']],
	['img', ['src', 'alt', 'title', 'width', 'height']],
	['table', kept('width')],
	['td', kept('align', 'colspan', 'rowspan', 'width', 'valign', 'nowrap')],
	['th', kept('align', 'colspan', 'rowspan', 'width', 'valign', 'nowrap')],
	['ol', kept('start')],
	['li', kept('value')],
	['details', kept('open')],
	...rules(['p', 'div', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6'], kept('align')),
	// Literal text, a code span inline.
	...rules(['code', 'tt', 'samp'], []),
	...rules(
		[
			'thead', 'tbody', 'tfoot', 'tr', 'span', 'b', 'strong', 'i', 'em', 'pre', 'sub', 'sup', 'kbd', 'del', 's',
			'strike', 'ins', 'mark', 'q', 'var', 'ruby', 'rt', 'rp', 'br', 'hr', 'ul', 'blockquote', 'dl', 'dt', 'dd',
			'summary', 'section'
		],
		kept()
	),
	...rules(['script', 'style', 'svg', 'iframe'], 'remove')
];

/** Kept by GitLab, stripped by GitHub. */
const GITLAB_ONLY: [string, HtmlElementRule][] = [
	...rules(['small', 'abbr', 'cite', 'dfn', 'time', 'bdo', 'caption', 'figure', 'figcaption'], kept()),
	['wbr', []]
];

/** GitLab's image float classes. */
const GITLAB_IMAGE_FLOATS: [string, 'left' | 'right'][] = [
	['glfm-float-left', 'left'],
	['glfm-float-right', 'right']
];

const policy = (
	entries: [string, HtmlElementRule][],
	rest: { tagFilter: boolean; imageFloats: [string, 'left' | 'right'][]; imageLines: boolean; imageParagraphs: boolean }
): HtmlPolicy => ({ ...rest, elements: new Map(entries), imageFloats: new Map(rest.imageFloats) });

const POLICIES: Record<HtmlForge, HtmlPolicy> = {
	github: policy(COMMON, { tagFilter: true, imageFloats: [], imageLines: true, imageParagraphs: true }),
	gitlab: policy([...COMMON, ...GITLAB_ONLY], { tagFilter: false, imageFloats: GITLAB_IMAGE_FLOATS, imageLines: false, imageParagraphs: false })
};

/**
 * Embedded HTML with no forge: what either forge keeps, so a host that
 * renders HTML outside a forge loses nothing a forge would show.
 */
export const ANY_FORGE_HTML_POLICY: HtmlPolicy = policy([...COMMON, ...GITLAB_ONLY], {
	tagFilter: false,
	imageFloats: GITLAB_IMAGE_FLOATS,
	imageLines: false,
	imageParagraphs: false
});

/** The policy of `forge`'s surface; `ANY_FORGE_HTML_POLICY` without one. */
export const htmlPolicyOf = (forge: DocumentForge): HtmlPolicy => (forge === null ? ANY_FORGE_HTML_POLICY : POLICIES[forge]);

/**
 * The `align` an `<img>` is placed by (render/imageSize.ts): a float class
 * of `policy`, which a forge's stylesheet applies over the presentational
 * attribute, else its `align` attribute.
 */
export const imageAlign = (policy: HtmlPolicy, img: Element): string | null =>
	policy.imageFloats.get(img.getAttribute('class')?.trim() ?? '') ?? img.getAttribute('align');

/** The attributes of `element` that `rule` keeps, as authored. */
export const ruleAttributes = (element: Element, rule: readonly string[]): [name: string, value: string][] => {
	const out: [string, string][] = [];
	for (const name of rule) {
		const value = element.getAttribute(name);
		if (value !== null) out.push([name, value]);
	}
	return out;
};
