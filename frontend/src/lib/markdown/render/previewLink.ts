// The one description of a preview anchor, read by BOTH renderers.
//
// A `localhost:<port>` link written by an agent names a listener on the
// machine that agent runs on. Read anywhere else — a browser on a phone, a
// second desktop attached to the first — following it would load whatever
// answers on that port of the READER's machine, or nothing. So the markdown
// parse rewrites those links into anchors that carry where they really point
// (`utils/previewLinkExtension.ts`), and the document click delegate opens
// them through the port gateway (`utils/externalLinks.ts`).
//
// The attributes live here rather than in either renderer because
// `Element.svelte` and `staticHtml.ts` are two realizations of one token
// stream: a difference between them is a silent output fork (see
// markdown/AGENTS.md). Both spell the anchor from `previewAnchorAttributes`
// and the sibling control from `previewAllowAttributes`.

import type { Tokens } from '../parser/engine';

/** Whether a rewritten link goes anywhere, and why not when it does not. */
export type PreviewLinkState = 'open' | 'not-shared' | 'no-address';

/** Everything a renderer needs, decided once during the parse. */
export interface PreviewLinkRender {
  state: PreviewLinkState;
  port: number;
  /** Path, query and fragment of the original URL. Never empty. */
  path: string;
  threadId: string;
  /** What the reader calls the machine the port is on. */
  machine: string;
  /** The machine's registry key, for the sharing control. */
  backend: string;
  /** Whether this session may share the port from here. */
  canAllow: boolean;
}

/** A link token the preview rewrite claimed. */
export interface PreviewLinkToken extends Tokens.Link {
  preview?: PreviewLinkRender;
}

/** The preview payload on a token, or undefined for an ordinary link. */
export function previewOfToken(token: Tokens.Link): PreviewLinkRender | undefined {
  return (token as PreviewLinkToken).preview;
}

/**
 * The attributes that make an anchor a preview anchor.
 *
 * `data-preview-port` is present in every state, including the two that go
 * nowhere: it is what the click delegate matches on, and a click on a link
 * that goes nowhere still has to be swallowed rather than followed.
 *
 * `data-preview-via` is the visible marker. It is rendered by a CSS
 * `::after` on `.preview-link` rather than by an element, which keeps the
 * anchor a single node: markdown links stream, and a second child per link
 * is a second node to build, lay out and throw away on every reveal.
 */
export function previewAnchorAttributes(
  preview: PreviewLinkRender,
): Record<string, string> {
  return {
    'data-preview-state': preview.state,
    'data-preview-port': String(preview.port),
    'data-preview-path': preview.path,
    'data-preview-thread': preview.threadId,
    'data-preview-machine': preview.machine,
    'data-preview-via': `via ${preview.machine}`,
  };
}

/** The sentence the anchor carries, per state. */
export function previewLinkTitle(preview: PreviewLinkRender): string {
  switch (preview.state) {
    case 'open':
      return `Opens on ${preview.machine}.`;
    case 'no-address':
      return `${preview.machine} has no address this page can reach it on.`;
    default:
      return `Port ${preview.port} is not shared from ${preview.machine}.`;
  }
}

/** The class every preview anchor carries, on top of the theme's link class. */
export const PREVIEW_LINK_CLASS = 'preview-link';

/** That class appended to whatever the theme names an ordinary link. */
export function previewAnchorClass(base: string | undefined): string {
  return base ? `${base} ${PREVIEW_LINK_CLASS}` : PREVIEW_LINK_CLASS;
}

/** The class of the sharing control beside an unshared one. */
export const PREVIEW_ALLOW_CLASS = 'preview-link-allow';

export const PREVIEW_ALLOW_LABEL = 'Allow port';

/**
 * Whether to render the sharing control beside the anchor.
 *
 * Only for `not-shared`: on `no-address` there is nowhere to serve the
 * preview from, so allowing the port would change nothing, and offering it
 * would be a control that does not work.
 */
export function previewAllowVisible(preview: PreviewLinkRender): boolean {
  return preview.state === 'not-shared' && preview.canAllow;
}

/** The attributes of that control. Its click is handled by the delegate. */
export function previewAllowAttributes(
  preview: PreviewLinkRender,
): Record<string, string> {
  return {
    'data-preview-allow': String(preview.port),
    'data-preview-backend': preview.backend,
  };
}
