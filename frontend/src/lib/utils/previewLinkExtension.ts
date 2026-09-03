// Marked inline extension that turns a `localhost:<port>` link into a
// preview anchor, DURING the parse.
//
// Why token time, and not a `link` snippet or a post-render walker: the
// same reasons `pathLinkExtension.ts` gives. A snippet would take ownership
// of every link on the surface and push both render paths off the fixed-tag
// static one (`markdown/render/staticHtml.ts` bails whenever a link snippet
// exists), so a thread with one dev-server link would pay component
// rendering for all of its prose. At token time the anchor is part of the
// first paint of the chunk that contains it, and the static path keeps
// serializing everything as before.
//
// What it claims, and only this:
//   - `[label](http://localhost:5173/x)`   markdown links
//   - `http://localhost:5173/x`            gfm bare autolinks
//   - `<http://localhost:5173/x>`          pointy autolinks
// on `localhost`, `127.x.x.x`, `0.0.0.0`, `[::1]` and `[::]`, with an
// EXPLICIT port. A URL with no port names a well-known service rather than
// a dev server, and previewing it is not a thing anyone asked for.
//
// The wildcard bind addresses are accepted here and refused by
// `loopbackDevServerURL` in `utils/externalLinks.ts`, which is not a
// contradiction: that helper answers "can this browser navigate there", and
// nothing navigates to a preview anchor. The click mints a URL on the port
// gateway instead, so `0.0.0.0:5173` in agent prose is simply how that agent
// spelled port 5173 on its own machine.
//
// Register this AHEAD of the path-link extension. That one claims every
// `[…](…)` link it is handed — it returns the token unchanged when the href
// is not path-shaped — so behind it this extension would never see one.

import type { Token, Tokens, TokensList } from '../markdown';
import type { PreviewLinkRender, PreviewLinkToken } from '../markdown/render/previewLink';
import { previewLinkTitle } from '../markdown/render/previewLink';
import type { PreviewLinkTarget } from '../stores/devServers.svelte';

interface PreviewLinkExtension {
  name: 'previewLink';
  level: 'inline';
  tokenizer(
    this: unknown,
    src: string,
    tokens: Token[] | TokensList,
  ): PreviewLinkToken | undefined;
}

interface PreviewTokenizerContext {
  lexer?: {
    state?: { inLink?: boolean };
    tokenizer?: {
      link?: (src: string) => Tokens.Link | Tokens.Image | undefined;
      url?: (src: string) => Tokens.Link | Tokens.Text | undefined;
      autolink?: (src: string) => Tokens.Link | Tokens.Text | undefined;
    };
  };
}

// Anchored cheap gate, run before anything constructs a URL. Every href this
// extension can claim starts with one of these, and no other href does.
const LOOPBACK_PREFIX_RE = /^https?:\/\/(?:localhost|127\.|0\.0\.0\.0|\[::)/i;

const CHAR_BRACKET = 0x5b; // [
const CHAR_LT = 0x3c; // <
const CHAR_H_LOWER = 0x68;
const CHAR_H_UPPER = 0x48;

/**
 * Split a loopback dev-server URL into the port and the path a preview would
 * be minted for, or null when it is not one.
 *
 * Exported for the delegate's own re-validation and for tests: the shapes
 * accepted here are the contract, not the regex above it.
 */
export function parsePreviewTarget(
  href: unknown,
): { port: number; path: string } | null {
  if (typeof href !== 'string' || !LOOPBACK_PREFIX_RE.test(href)) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
  if (!isPreviewHostname(url.hostname)) return null;
  // An empty `port` is the scheme's default, which means the URL named no
  // dev server. `Number('')` is 0, so the range check below refuses it.
  const port = Number(url.port);
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) return null;
  const path = `${url.pathname}${url.search}${url.hash}`;
  return { port, path: path === '' ? '/' : path };
}

function isPreviewHostname(hostname: string): boolean {
  const host = hostname.toLowerCase();
  if (host === 'localhost' || host === '0.0.0.0') return true;
  if (host === '[::1]' || host === '[::]') return true;
  // A full dotted quad, not a `127.` prefix: `127.example.com` is a
  // resolvable public name. Shorthand forms (`127.1`) never reach here,
  // because URL parsing normalizes an IPv4 host to the dotted quad first.
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host);
}

/**
 * Build the extension for one surface, or undefined to leave its markdown
 * exactly as it is.
 *
 * The caller decides whether there is anything to rewrite;
 * `previewLinkTargetFor` in `stores/devServers` is that decision, and it
 * answers null until the machine has sent a list. Before then a link is left
 * plain rather than rendered as not shared, because an inert link that turns
 * live a moment later is a wrong sentence rather than a slow one.
 */
export function buildPreviewLinkExtension(
  target: PreviewLinkTarget | null | undefined,
): PreviewLinkExtension | undefined {
  if (!target) return undefined;
  return {
    name: 'previewLink',
    level: 'inline',
    // No `start`. Marked's gfm inline `text` rule already stops at `[`,
    // at `<` and at `protocol://`, so the tokenizer below is invoked at
    // every position one of these links can begin — and a `start` would
    // add a scan of the remaining tail at every OTHER position for nothing.
    tokenizer(this: unknown, src) {
      const context = this as PreviewTokenizerContext;
      const tokenizer = context.lexer?.tokenizer;
      if (!tokenizer) return undefined;

      let token: Tokens.Link | Tokens.Image | Tokens.Text | undefined;
      const code = src.charCodeAt(0);
      if (code === CHAR_BRACKET) {
        token = tokenizer.link?.(src);
      } else if (code === CHAR_LT) {
        token = tokenizer.autolink?.(src);
      } else if (code === CHAR_H_LOWER || code === CHAR_H_UPPER) {
        // Mirrors the lexer's own guard: a bare URL inside a link label is
        // the label's text, not a second link.
        if (context.lexer?.state?.inLink === true) return undefined;
        token = tokenizer.url?.(src);
      } else {
        return undefined;
      }
      if (!token || token.type !== 'link') return undefined;

      const parsed = parsePreviewTarget(token.href);
      if (!parsed) return undefined;

      const preview: PreviewLinkRender = {
        state: target.resolve(parsed.port).kind,
        port: parsed.port,
        path: parsed.path,
        threadId: target.threadId,
        machine: target.machine,
        backend: target.backend,
        canAllow: target.canAllow,
      };
      // The href stays the URL the agent wrote: copying the link, or
      // reading it in devtools, should say what the agent said. Where the
      // click goes is decided from the data attributes instead.
      return { ...token, title: previewLinkTitle(preview), preview };
    },
  };
}
