// A tracker's media in ticket text. A Jira image or attachment reaches the
// Markdown as `![alt](media:ID)` or `[name](media:ID)`
// (internal/tracker/adf/render_media.go), which names a file of the ticket
// that no URL here can fetch. On a surface that renders ticket text
// (ChatMarkdown's `trackerMedia`), the destination hook claims every such
// link and image, written in Markdown or as an HTML `<a>` or `<img>`, and
// rewrites it to an image token with a nonce-gated
// `agent-overflow:tracker-media?…` href. The image hosts show that href as
// a chip naming the file (TrackerMediaChip.svelte): nothing is fetched,
// nothing is a link, and nothing reaches the link opener.
//
// A link becomes an image token on purpose: one host decides what a
// ticket's file shows as, whatever kind of file it is.

import type { DestinationHook, Extension } from '../markdown';
import { MARKDOWN_HREF_NONCE } from './markdownHrefNonce';

const MEDIA_SCHEME = 'media:';

/** Prefix of every tracker media href this app mints (see markdownHrefNonce.ts). */
export const TRACKER_MEDIA_HREF_PREFIX = `agent-overflow:tracker-media?nonce=${MARKDOWN_HREF_NONCE}&`;

export function isTrackerMediaHref(href: string): boolean {
  return href.startsWith(TRACKER_MEDIA_HREF_PREFIX);
}

const claim = (id: string): string => `${TRACKER_MEDIA_HREF_PREFIX}id=${encodeURIComponent(id)}`;

const destination: DestinationHook = (token) => {
  if (!token.href.startsWith(MEDIA_SCHEME)) return undefined;
  const href = claim(token.href.slice(MEDIA_SCHEME.length));
  // The label's inline tokens are what the renderer takes the plain name
  // from (render/imageAlt.ts), for a link as for an image.
  return { type: 'image', raw: token.raw, href, title: null, text: token.text, tokens: token.tokens ?? [] };
};

/** The extension a ticket text surface adds: its destination hook alone. */
export const trackerMediaExtension: Extension = {
  name: 'trackerMedia',
  level: 'inline',
  destination,
};
