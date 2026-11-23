import { describe, expect, it } from 'vitest';
import { lex } from '../../index';
import { markedImageAttributes } from './imageAttributes';
import { markedMediaImages, mediaOfHref } from './mediaImages';

type Tok = { type: string; raw: string; href?: string; media?: string; width?: string; tokens?: Tok[] };
const inline = (source: string): Tok[] =>
	((lex(source, [markedMediaImages, markedImageAttributes]) as unknown as Tok[])[0]?.tokens ?? []) as Tok[];
const walk = (tokens: Tok[]): Tok[] => tokens.flatMap((token) => [token, ...walk(token.tokens ?? [])]);
const images = (source: string): Tok[] => walk(inline(source)).filter((token) => token.type === 'image');

describe('mediaOfHref', () => {
	it.each([
		['https://example.invalid/clip.mp4', 'video'],
		['https://example.invalid/clip.MOV', 'video'],
		['img/clip.webm?raw=true#t=3', 'video'],
		['https://example.invalid/a.m4v', 'video'],
		['https://example.invalid/a.ogv', 'video'],
		['https://example.invalid/sound.mp3', 'audio'],
		['https://example.invalid/sound.Ogg', 'audio'],
		['/uploads/a.oga', 'audio'],
		['https://example.invalid/a.spx', 'audio'],
		['https://example.invalid/a.wav', 'audio'],
		['https://example.invalid/a.png', null],
		['https://example.invalid/a.mp4/page', null],
		['https://example.invalid/page?file=a.mp4', null],
		['https://example.invalid/mp4', null],
		['https://example.invalid/a.mkv', null]
	])('reads %s', (href, media) => {
		expect(mediaOfHref(href)).toBe(media);
	});
});

describe('markedMediaImages', () => {
	it('marks an image of a video or audio file playable, raw exactly its source', () => {
		const source = 'A ![clip](https://example.invalid/clip.mp4 "Title") and ![memo][m].\n\n[m]: https://example.invalid/memo.wav';
		const tokens = inline(source);
		expect(tokens.map((token) => [token.type, token.raw])).toEqual([
			['text', 'A '],
			['image', '![clip](https://example.invalid/clip.mp4 "Title")'],
			['text', ' and '],
			['image', '![memo][m]'],
			['text', '.']
		]);
		expect(tokens[1]).toMatchObject({ href: 'https://example.invalid/clip.mp4', media: 'video' });
		expect(tokens[3]).toMatchObject({ href: 'https://example.invalid/memo.wav', media: 'audio' });
	});

	it('leaves a picture an image', () => {
		expect(images('![a](https://example.invalid/a.png)')[0]).not.toHaveProperty('media');
	});

	it('leaves an image in link text an image', () => {
		const [image] = images('[![Linked video](https://example.invalid/clip.mp4)](https://example.invalid/page)');
		expect(image).toMatchObject({ href: 'https://example.invalid/clip.mp4' });
		expect(image).not.toHaveProperty('media');
	});

	it('takes the attribute list after it as an image does', () => {
		expect(images('![v](https://example.invalid/clip.mp4){width=320 height=180}')[0]).toMatchObject({
			media: 'video',
			width: '320',
			height: '180'
		});
	});
});
