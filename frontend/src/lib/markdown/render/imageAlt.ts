/**
 * An image's alt text: the plain text of its label, as CommonMark and both
 * forges write it. `![&copy; **alt** \`x\`](u)` is `© alt x`: emphasis and
 * code show their text, a reference shows what it names (the token the
 * entity extension decodes it to), a nested image its own alt, and a tag
 * nothing. An image with no label tokens (an `<img alt>`, a host's claim)
 * keeps its `text`, which is already plain.
 */
import type { Tokens } from '../parser/engine';

type Leaf = { type: string; text?: string; tokens?: Leaf[] };

const plain = (tokens: readonly Leaf[]): string => {
	let out = '';
	for (const token of tokens) {
		if (token.type === 'html') continue;
		if (token.type === 'br') out += '\n';
		else if (token.type === 'image') out += imageAlt(token as Tokens.Image);
		else if (Array.isArray(token.tokens) && token.tokens.length > 0) out += plain(token.tokens);
		else out += token.text ?? '';
	}
	return out;
};

export const imageAlt = (image: Pick<Tokens.Image, 'text' | 'tokens'>): string =>
	Array.isArray(image.tokens) && image.tokens.length > 0 ? plain(image.tokens as Leaf[]) : image.text;
