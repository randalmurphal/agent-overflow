/**
 * Theme-specific images, as GitHub shows them. A `<picture>` `<source>`
 * whose `media` is `(prefers-color-scheme: dark)` or `(… light)` is the
 * picture in that scheme, and an image whose URL ends in
 * `#gh-dark-mode-only` or `#gh-light-mode-only` shows only in that scheme.
 *
 * The scheme is the reader's: a surface passes it in (the renderer's and
 * the sanitizer's `colorScheme`). Without one every image paints its own
 * src, which is what GitLab shows for both forms.
 *
 * A `srcset` is read as GitHub's sanitizer keeps it: its first candidate's
 * URL, without a descriptor, so it paints at its natural size as the
 * fallback `src` does. A `<source>` with any other media query is not
 * read: the reader's color scheme is the one question this page answers.
 */
export type ColorScheme = 'light' | 'dark';

/** The src a `<picture>` offers in each scheme, when a source names it. */
export type ColorSchemeSources = Partial<Record<ColorScheme, string>>;

/** What `imageSrcFor` reads off an image token. */
export interface ThemedImage {
	href: string;
	/** A `<picture>`'s per-scheme sources (EmbeddedImageToken). */
	sources?: ColorSchemeSources;
	/** The scheme its authored URL's fragment shows it in, when a host
	 * rewrote `href` and the fragment with it. */
	shownIn?: ColorScheme;
}

const SCHEME_QUERY = /^\s*\(\s*prefers-color-scheme\s*:\s*(light|dark)\s*\)\s*$/i;
const SCHEME_ONLY = /#gh-(light|dark)-mode-only$/;

/** The scheme `href`'s `#gh-*-mode-only` fragment shows it in, or null. */
export const colorSchemeOnly = (href: string): ColorScheme | null =>
	(SCHEME_ONLY.exec(href)?.[1] as ColorScheme | undefined) ?? null;

/** A `srcset`'s first candidate URL (the HTML candidate grammar), or null. */
const firstCandidate = (srcset: string): string | null => {
	const url = /^[\s,]*(\S+)/.exec(srcset)?.[1].replace(/,+$/, '');
	return url ? url : null;
};

/**
 * Each scheme's src: the first `<source>` before the `<img>` whose media is
 * that scheme's query and whose srcset holds a candidate, as the browser
 * picks among a picture's children.
 */
export const pictureSources = (picture: Element): ColorSchemeSources => {
	const sources: ColorSchemeSources = {};
	for (const child of picture.children) {
		if (child.localName === 'img') break;
		if (child.localName !== 'source') continue;
		const scheme = SCHEME_QUERY.exec(child.getAttribute('media') ?? '')?.[1].toLowerCase() as ColorScheme | undefined;
		const src = firstCandidate(child.getAttribute('srcset') ?? '');
		if (scheme && src !== null && sources[scheme] === undefined) sources[scheme] = src;
	}
	return sources;
};

/** The src `image` paints in `scheme`, or null when it is hidden there. */
export const imageSrcFor = (image: ThemedImage, scheme: ColorScheme | undefined): string | null => {
	if (scheme === undefined) return image.href;
	const only = image.shownIn ?? colorSchemeOnly(image.href);
	if (only !== null && only !== scheme) return null;
	return image.sources?.[scheme] ?? image.href;
};
