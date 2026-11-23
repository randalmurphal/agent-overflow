import { describe, expect, it } from 'vitest';
import { lex, type Extension, type Tokens } from '../index';
import { markedHtmlEntities } from '../parser/extensions/htmlEntities';
import { imageAlt } from './imageAlt';

const image = (source: string, extensions: Extension[] = []): Tokens.Image => {
	const [paragraph] = lex(source, extensions) as unknown as { tokens: Tokens.Image[] }[];
	return paragraph.tokens.find((token) => token.type === 'image')!;
};

describe('imageAlt', () => {
	it('is the plain text of the label, as CommonMark and both forges write it', () => {
		expect(imageAlt(image('![alt with *emphasis* and `code` \\* ~~gone~~](https://example.invalid/b.png)'))).toBe(
			'alt with emphasis and code * gone'
		);
	});

	it('shows a reference as what it names where references decode', () => {
		expect(imageAlt(image('![&copy; **alt**](https://example.invalid/i.png)', [markedHtmlEntities]))).toBe('© alt');
		expect(imageAlt(image('![&copy; **alt**](https://example.invalid/i.png)'))).toBe('&copy; alt');
	});

	it('takes a nested image as its alt and drops a tag', () => {
		expect(imageAlt(image('![a ![b *c*](https://example.invalid/x.png) <span>d</span>](https://example.invalid/i.png)'))).toBe('a b c d');
	});

	it('keeps the text of an image with no label tokens', () => {
		expect(imageAlt({ text: 'html alt', tokens: [] })).toBe('html alt');
	});
});
