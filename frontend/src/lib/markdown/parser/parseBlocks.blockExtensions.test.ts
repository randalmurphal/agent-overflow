import { describe, expect, it } from 'vitest';
import { EMBEDDED_HTML_EXTENSIONS } from './extensions/embeddedHtml';
import { parseBlocks } from './parseBlocks';
import { createParseBlocksCache } from './parseBlocks.cache';
import { createProvenAppend } from './provenAppend';

// A block extension that sets `reopensSealedBlocks` claims runs that cross
// blank lines, so a block the append cache sealed can be the opening of a run
// a later append closes. The cached split must equal a fresh one after every
// append.

const DOCUMENTS = [
	'<div align="center">\n\n**inside**\n\n- one\n- two\n\n</div>\n\nafter\n',
	'lead\n\n<details>\n<summary>More</summary>\n\nfirst\n\nsecond\n\n</details>\n\nafter\n',
	'<div>\n\none\n\n<div>\n\ntwo\n\n</div>\n\nthree\n\n</div>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n',
	'> quote\n\n<p align="center">\n\ntext\n\n</p>\n\n```js\ncode\n```\n'
];

describe('parseBlocks with an extension that reopens sealed blocks', () => {
	it('splits an appended document as a fresh parse does, at every chunk size', () => {
		for (const document of DOCUMENTS) {
			for (const size of [1, 2, 3, 7, 16]) {
				const cache = createParseBlocksCache();
				let source = '';
				for (let at = 0; at < document.length; at += size) {
					const append = createProvenAppend(source, document.slice(at, at + size));
					source = append.next;
					expect(parseBlocks(source, EMBEDDED_HTML_EXTENSIONS, cache, append), JSON.stringify(source)).toEqual(
						parseBlocks(source, EMBEDDED_HTML_EXTENSIONS)
					);
				}
			}
		}
	});

	it('joins a sealed opening block to the closing tag a later append brings', () => {
		const cache = createParseBlocksCache();
		const open = '<div align="center">\n\n**inside**\n\nmore\n\n';
		expect(parseBlocks(open, EMBEDDED_HTML_EXTENSIONS, cache).length).toBeGreaterThan(1);
		const append = createProvenAppend(open, '</div>\n');
		expect(parseBlocks(append.next, EMBEDDED_HTML_EXTENSIONS, cache, append)).toEqual([append.next]);
	});

	it('keeps the append paths without one', () => {
		const cache = createParseBlocksCache();
		parseBlocks('one\n\ntwo', [], cache);
		const append = createProvenAppend('one\n\ntwo', ' words');
		parseBlocks(append.next, [], cache, append);
		expect(cache.lastPath).toBe('paragraph-append');
	});
});
