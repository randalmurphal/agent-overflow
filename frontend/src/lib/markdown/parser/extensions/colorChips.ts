/**
 * Color chips: a code span whose whole text is a color shows a swatch of
 * that color beside the code, as GitHub and GitLab render it. Opt-in, one
 * grammar per forge; agent chat shows plain code.
 *
 * `markedColorChips` accepts GitHub's forms: `#RRGGBB`, `rgb(R, G, B)` and
 * `hsl(H, S%, L%)`, lowercase function names, with no blank around them.
 *
 * `markedCssColorChips` accepts GitLab's (`Banzai::ColorParser`): `#` and
 * 3, 4, 6 or 8 hex digits; `rgb()`/`rgba()` of three 0-255 values or three
 * percentages; `hsl()`/`hsla()` of a hue in degrees or radians and two
 * percentages; each with an optional alpha, function names in any case,
 * blanks around them ignored. A backslash before a color escapes it: the
 * code shows the color without the backslash, and no swatch.
 *
 * The token is the engine's `codespan` with `color` set to the accepted
 * text, which the renderers paint as an inline style; the grammar admits no
 * character that could end the style property.
 */
import type { Tokens } from '../engine';
import type { Extension } from '../lexer';

const GITHUB_COLOR = /^(?:#[0-9a-fA-F]{6}|rgb\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}\s*\)|hsl\(\s*\d{1,3}\s*,\s*\d{1,3}%\s*,\s*\d{1,3}%\s*\))$/;

const ALPHA = String.raw`(?:0(?:\.\d+)?|\.\d+|1(?:\.0+)?)`;
const PERCENT = String.raw`(?:\d{1,2}|100)%`;
const ALPHA_CHANNEL = String.raw`(?:,\s*(?:${ALPHA}|${PERCENT}))?`;
const BITS = String.raw`(?:\d{1,2}|1\d\d|2(?:[0-4]\d|5[0-5]))`;
const HUE = String.raw`(?:-?\d+(?:deg)?|-?(?:\d+(?:\.\d+)?|\.\d+)rad)`;
const GITLAB_COLOR = new RegExp(
	'^(?:' +
		String.raw`#(?:[0-9a-f]{3}|[0-9a-f]{4}|[0-9a-f]{6}|[0-9a-f]{8})` +
		String.raw`|rgba?\((?:(?:${BITS},\s*){2}${BITS}|(?:${PERCENT},\s*){2}${PERCENT})${ALPHA_CHANNEL}\)` +
		String.raw`|hsla?\(${HUE},\s*${PERCENT},\s*${PERCENT}${ALPHA_CHANNEL}\)` +
		')$',
	'i'
);

const chips = (name: string, color: RegExp, lenient: boolean): Extension => ({
	name,
	level: 'inline',
	tokenizer(src) {
		if (src.charCodeAt(0) !== 96 /* ` */) return undefined;
		const span = this.lexer.tokenizer.codespan(src);
		if (!span) return undefined;
		const text = lenient ? span.text.trim() : span.text;
		if (color.test(text)) {
			const chip: Tokens.Codespan = { ...span, color: text };
			return chip;
		}
		if (lenient && text.charCodeAt(0) === 92 /* \ */ && color.test(text.slice(1))) {
			const escaped: Tokens.Codespan = { ...span, text: text.slice(1) };
			return escaped;
		}
		return undefined;
	}
});

export const markedColorChips: Extension = chips('colorChips', GITHUB_COLOR, false);
export const markedCssColorChips: Extension = chips('cssColorChips', GITLAB_COLOR, true);
