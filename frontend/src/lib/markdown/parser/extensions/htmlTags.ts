/**
 * Reads HTML tags as the HTML tokenizer does, for the forge HTML the
 * Markdown extensions must understand: which elements an HTML block opens
 * and closes, and an inline tag's attributes.
 *
 * A quoted attribute value may hold `>` and `<`; a comment, a declaration
 * and the content of a raw-text element (`<script>`, `<style>`,
 * `<textarea>`, ...) hold no tags; a tag name ends at whitespace, `/` or
 * `>`, so `<img-x>` is not an `<img>`. One divergence keeps every scan
 * linear: a `<` outside a quoted value ends the attempt, where the HTML
 * tokenizer would read it into an attribute.
 */

export type HtmlTag = {
	/** Index of the `<`. */
	start: number;
	/** Index just past the `>`. */
	end: number;
	/** Lowercased. */
	name: string;
	closing: boolean;
	selfClosing: boolean;
	/** In source order, names lowercased, values as written. */
	attrs: [string, string][];
};

const isSpace = (code: number): boolean => code === 32 || code === 9 || code === 10 || code === 12 || code === 13;
const isAlpha = (code: number): boolean => (code >= 65 && code <= 90) || (code >= 97 && code <= 122);

/** The tag whose `<` is `html[at]`, or null when none starts there. */
export const readTag = (html: string, at: number): HtmlTag | null => {
	const length = html.length;
	let i = at + 1;
	const closing = html.charCodeAt(i) === 47; /* / */
	if (closing) i++;
	if (!isAlpha(html.charCodeAt(i))) return null;
	const nameStart = i;
	for (let code = html.charCodeAt(i); i < length && !isSpace(code) && code !== 47 && code !== 62 && code !== 60; code = html.charCodeAt(++i));
	const name = html.slice(nameStart, i).toLowerCase();
	const attrs: [string, string][] = [];
	for (;;) {
		while (i < length && isSpace(html.charCodeAt(i))) i++;
		if (i >= length) return null;
		let code = html.charCodeAt(i);
		if (code === 62 /* > */) return { start: at, end: i + 1, name, closing, selfClosing: false, attrs };
		if (code === 60 /* < */) return null;
		if (code === 47 /* / */) {
			if (html.charCodeAt(i + 1) === 62) return { start: at, end: i + 2, name, closing, selfClosing: true, attrs };
			i++;
			continue;
		}
		// The first character starts the name even when it is `=`.
		const attrStart = i++;
		for (code = html.charCodeAt(i); i < length && !isSpace(code) && code !== 47 && code !== 62 && code !== 61 && code !== 60; code = html.charCodeAt(++i));
		const attrName = html.slice(attrStart, i).toLowerCase();
		while (i < length && isSpace(html.charCodeAt(i))) i++;
		let value = '';
		if (html.charCodeAt(i) === 61 /* = */) {
			i++;
			while (i < length && isSpace(html.charCodeAt(i))) i++;
			code = html.charCodeAt(i);
			if (code === 34 || code === 39) {
				const close = html.indexOf(code === 34 ? '"' : "'", i + 1);
				if (close === -1) return null;
				value = html.slice(i + 1, close);
				i = close + 1;
			} else {
				const valueStart = i;
				for (; i < length && !isSpace(code) && code !== 62 && code !== 60; code = html.charCodeAt(++i));
				value = html.slice(valueStart, i);
			}
		}
		attrs.push([attrName, value]);
	}
};

/** The first value of attribute `name`, as written; null when absent. */
export const attributeOf = (tag: HtmlTag, name: string): string | null => {
	for (const [attrName, value] of tag.attrs) if (attrName === name) return value;
	return null;
};

// Elements whose content is text: `</script>` ends a script, nothing else
// inside it is a tag. `plaintext` never ends.
const RAW_TEXT = new Set(['script', 'style', 'textarea', 'title', 'xmp', 'iframe', 'noembed', 'noframes']);
const rawTextEnds = new Map<string, RegExp>();
const COMMENT_END = /--!?>/g;

/** The index past the first `>` at or after `from`; -1 when there is none. */
const pastGreaterThan = (html: string, from: number): number => {
	const at = html.indexOf('>', from);
	return at === -1 ? -1 : at + 1;
};

/**
 * Calls `visit` with each tag of `html` in order, skipping comments,
 * declarations, processing instructions and raw text. `visit` returning
 * false stops the scan.
 */
export const scanTags = (html: string, visit: (tag: HtmlTag) => boolean | void): void => {
	let i = 0;
	while (i < html.length) {
		const lt = html.indexOf('<', i);
		if (lt === -1) return;
		const next = html.charCodeAt(lt + 1);
		if (next === 33 /* ! */) {
			if (html.startsWith('<!--', lt)) {
				// `<!-->` and `<!--->` are whole comments.
				if (html.charCodeAt(lt + 4) === 62) i = lt + 5;
				else if (html.startsWith('->', lt + 4)) i = lt + 6;
				else {
					COMMENT_END.lastIndex = lt + 4;
					const end = COMMENT_END.exec(html);
					if (end === null) return;
					i = end.index + end[0].length;
				}
			} else {
				i = pastGreaterThan(html, lt + 2);
			}
			if (i === -1) return;
			continue;
		}
		if (next === 63 /* ? */ || (next === 47 && !isAlpha(html.charCodeAt(lt + 2)))) {
			// A processing instruction or `</ ...>` is a bogus comment; `</>` is nothing.
			i = pastGreaterThan(html, lt + 2);
			if (i === -1) return;
			continue;
		}
		const tag = readTag(html, lt);
		if (tag === null) {
			i = lt + 1;
			continue;
		}
		if (visit(tag) === false) return;
		i = tag.end;
		if (tag.closing) continue;
		if (tag.name === 'plaintext') return;
		if (RAW_TEXT.has(tag.name)) {
			let end = rawTextEnds.get(tag.name);
			if (!end) {
				end = new RegExp(`</${tag.name}(?=[\\t\\n\\f\\r />])`, 'gi');
				rawTextEnds.set(tag.name, end);
			}
			end.lastIndex = i;
			const match = end.exec(html);
			if (match === null) return;
			i = match.index;
		}
	}
};

/**
 * The open elements of a tag sequence, each with a value. An end tag whose
 * name is not open is ignored without a walk down the stack, and one that
 * is open unwinds each element above it once, so pairing a sequence is
 * linear in its tags. `walked` counts the elements unwound.
 */
export class OpenElements<T> {
	private readonly names: string[] = [];
	private readonly values: T[] = [];
	private readonly counts = new Map<string, number>();
	walked = 0;

	get height(): number {
		return this.names.length;
	}

	valueAt(index: number): T {
		return this.values[index];
	}

	push(name: string, value: T): void {
		this.names.push(name);
		this.values.push(value);
		this.counts.set(name, (this.counts.get(name) ?? 0) + 1);
	}

	/**
	 * Closes the nearest open `name` and every element opened after it, as an
	 * HTML parser does. Returns that element's value; undefined, closing
	 * nothing, when no `name` is open.
	 */
	close(name: string): T | undefined {
		if (!this.counts.get(name)) return undefined;
		for (;;) {
			const top = this.names.pop()!;
			const value = this.values.pop()!;
			this.counts.set(top, this.counts.get(top)! - 1);
			this.walked++;
			if (top === name) return value;
		}
	}
}
