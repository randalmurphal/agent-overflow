/** Longest inline code text, in UTF-16 units, that a table cell keeps on one line. */
export const SHORT_CODESPAN_MAX_LENGTH = 32;

/**
 * Marks inline code short enough to stay on one line. The table cell rule in
 * app.css keys on it; every emitter of an inline `<code>` goes through
 * `codespanClass` so the render paths cannot disagree.
 */
export const SHORT_CODESPAN_CLASS = 'sd-codespan-short';

/** Class list for an inline `<code>` holding `text`. */
export function codespanClass(base: string, text: string): string {
	if (text.length > SHORT_CODESPAN_MAX_LENGTH) return base;
	return base === '' ? SHORT_CODESPAN_CLASS : `${base} ${SHORT_CODESPAN_CLASS}`;
}
