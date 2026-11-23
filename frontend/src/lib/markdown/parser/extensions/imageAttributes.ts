/**
 * GitLab's image attribute list: `![label](url){width=100 height=100px}`
 * or `{width=75%}` right after an image sizes it, and the list never shows.
 * GitLab's rich text editor writes one when a user resizes an image.
 * Opt-in: GitHub, CommonMark and agent chat show the list as text.
 *
 * GitLab (`Banzai::Filter::AttributesFilter`) applies a list to an image
 * node, however it was written, whose next text starts with the list; a
 * space between them leaves the list as text. Here that is the inline token
 * before a `{`: any `image` token (an engine or reference image, an embedded
 * `<img>` or `<picture>`, an image or video a host extension claimed), so
 * this one rule runs after whichever tokenizer recognized the image.
 *
 * The grammar: a list of 1 to 100 characters on one line, split on white
 * space; each `width=` or `height=` entry, its value optionally in double
 * quotes, sizes the image when the value is 1 to 4 digits with an optional
 * `px` or `%`, and a `.float-left` or `.float-right` entry floats it (GitLab's
 * `glfm-float-*` class), which the token carries as an `align` of `left` or
 * `right`. Every other entry is dropped and the whole list is consumed
 * either way. GitLab's pattern runs to the last `}` within 100 characters;
 * this one ends at the first, so text after a list is never hidden with it.
 *
 * The size replaces the image token's `width` and `height` strings, side by
 * side, as an embedded `<img>`'s carry them (EmbeddedImageToken), so every
 * image renderer paints it through `render/imageSize.ts`. A `px` unit is
 * dropped: a bare number is pixels there. The list's source is a `space`
 * token, which every renderer and find's projection show as nothing, so
 * each token's `raw` stays exactly its own source.
 */
import type { Tokens } from '../engine';
import type { Extension } from '../lexer';

const ATTRIBUTE_LIST = /^\{([^}\n\r]{1,100})\}/;
const SIZE_ENTRY = /^(width|height)="?([\w%]{1,10})"?$/;
const SIZE = /^(\d{1,4})(%|px)?$/;
const FLOAT = /^\.float-(left|right)$/;

export interface ImageAttributes {
	/** The list as written, braces included. */
	raw: string;
	width: string | null;
	height: string | null;
	align: 'left' | 'right' | null;
}

/** The attribute list `src` starts with, or undefined when none does. */
export const readImageAttributes = (src: string): ImageAttributes | undefined => {
	const match = ATTRIBUTE_LIST.exec(src);
	if (!match) return undefined;
	const list: ImageAttributes = { raw: match[0], width: null, height: null, align: null };
	for (const entry of match[1].split(/\s+/)) {
		const float = FLOAT.exec(entry);
		if (float) {
			list.align = float[1] as 'left' | 'right';
			continue;
		}
		const sized = SIZE_ENTRY.exec(entry);
		const size = sized ? SIZE.exec(sized[2]) : null;
		if (!sized || !size) continue;
		list[sized[1] as 'width' | 'height'] = size[2] === '%' ? `${size[1]}%` : size[1];
	}
	return list;
};

/** An attribute list right after an image: sizes it and shows nothing. */
export const markedImageAttributes: Extension = {
	name: 'imageAttributes',
	level: 'inline',
	tokenizer(src, tokens) {
		if (src.charCodeAt(0) !== 123 /* { */) return undefined;
		const at = tokens.length - 1;
		const image = tokens[at] as Tokens.Image | undefined;
		if (image?.type !== 'image') return undefined;
		const list = readImageAttributes(src);
		if (!list) return undefined;
		if (list.width !== null || list.height !== null || list.align !== null) {
			// A new token rather than a write into the old one, which an
			// earlier lex may still hold.
			const sized: Tokens.Image & { width?: string | null; height?: string | null; align?: string | null } = { ...image };
			if (list.width !== null) sized.width = list.width;
			if (list.height !== null) sized.height = list.height;
			if (list.align !== null) sized.align = list.align;
			tokens[at] = sized;
		}
		const space: Tokens.Space = { type: 'space', raw: list.raw };
		return space;
	}
};
