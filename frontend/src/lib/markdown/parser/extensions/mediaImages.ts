/**
 * GitLab's playable images: a Markdown image whose URL path ends in a video
 * or audio extension is a player, `![clip](demo.mp4)` a `<video>` and
 * `![note](memo.mp3)` an `<audio>` (`Banzai::Filter::VideoLinkFilter`,
 * `AudioLinkFilter`, with `Gitlab::FileTypeDetection`'s extensions,
 * compared case-insensitively). Inside link text it stays an image, as
 * GitLab leaves an image with an `<a>` ancestor, so the link is the one
 * control. Opt-in: GitHub and agent chat show such an image as an image.
 *
 * The token is the engine's image token with `media` set, so the URL
 * policy, the `{width=… height=…}` list after it (imageAttributes.ts) and
 * find's projection treat it as the image it was written as; the image
 * element renders the player.
 */
import type { Tokens } from '../engine';
import type { Extension } from '../lexer';

export type ImageMedia = 'video' | 'audio';

/** An image token a renderer paints as a player. */
export type MediaImageToken = Tokens.Image & { media?: ImageMedia };

const VIDEO = new Set(['mp4', 'm4v', 'mov', 'webm', 'ogv']);
const AUDIO = new Set(['mp3', 'oga', 'ogg', 'spx', 'wav']);

/** The player `href`'s path extension calls for, or null. */
export const mediaOfHref = (href: string): ImageMedia | null => {
	const path = href.split(/[?#]/, 1)[0];
	const dot = path.lastIndexOf('.');
	if (dot === -1 || path.indexOf('/', dot) !== -1) return null;
	const extension = path.slice(dot + 1).toLowerCase();
	return VIDEO.has(extension) ? 'video' : AUDIO.has(extension) ? 'audio' : null;
};

export const markedMediaImages: Extension = {
	name: 'mediaImages',
	level: 'inline',
	tokenizer(src) {
		if (src.charCodeAt(0) !== 33 /* ! */ || src.charCodeAt(1) !== 91 /* [ */) return undefined;
		if (this.lexer.state.inLink) return undefined;
		const tokenizer = this.lexer.tokenizer;
		const image = tokenizer.link(src) ?? tokenizer.reflink(src, this.lexer.tokens.links);
		if (image?.type !== 'image') return undefined;
		const media = mediaOfHref(image.href);
		if (media === null) return undefined;
		const token: MediaImageToken = { ...image, media };
		return token;
	}
};
