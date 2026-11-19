/**
 * Where a link or image goes: the one rule every producer of a destination
 * runs (the engine's inline, reference and autolink tokens, an extension's
 * HTML `<a>` and `<img>`, and the HTML sanitizer), so a destination is read
 * the same way however it was written.
 */
import { inline } from './rules';
import type { Tokens } from './tokens';

/** What a destination hook knows of where the token sits. */
export interface DestinationContext {
  /** Inside link text or an `<a>`: only an image is claimed there. */
  readonly inLink: boolean;
}

/**
 * A host's claim on a destination: the token to emit in place of `token`
 * (a resolved href, an app href with its title, a preview), or undefined
 * to leave it to the next hook. `token.href` is already canonical
 * (`canonicalHref`). Pure: tokenizers call it speculatively.
 */
export type DestinationHook = (
  token: Tokens.Link | Tokens.Image,
  context: DestinationContext,
) => Tokens.Link | Tokens.Image | undefined;

/** How bare URLs autolink on a surface (an extension's `bareUrls`). */
export interface BareUrlRule {
  /** Whether a bare URL with this lower-case scheme links (`www.` is http). */
  readonly links: (scheme: string) => boolean;
  /** The URL a matched run holds, without its trailing punctuation. */
  readonly end: (run: string) => string;
}

const EDGE_CONTROLS = /^[\x00-\x20]+|[\x00-\x20]+$/g;
const TAB_OR_NEWLINE = /[\t\n\r]/g;
const HTTP_SCHEME = /^https?:/i;
/** `http(s)://`, an authority without userinfo, and no backslash. */
const PLAIN_HTTP = /^https?:\/\/[^\\/?#@]*(?:[/?#][^\\]*)?$/i;

/**
 * `href` as the URL a renderer would fetch, so a hook never classifies a
 * spelling the URL parser reads as another URL. A scheme-relative `//host/x`
 * is the https URL it names, never a URL on the page's origin. An http(s)
 * URL not written plainly (one slash, a backslash, a tab or newline, edge
 * controls or spaces, userinfo) is its parsed `href`; a plain one keeps its
 * spelling. Anything else is returned as written.
 */
export function canonicalHref(href: string): string {
  const url = href.replace(TAB_OR_NEWLINE, '').replace(EDGE_CONTROLS, '');
  if (url.startsWith('//')) return canonicalHref(`https:${url}`);
  if (!HTTP_SCHEME.test(url)) return href;
  if (url === href && PLAIN_HTTP.test(url)) return href;
  try {
    return new URL(url).href;
  } catch {
    return href;
  }
}

/** `token` with its canonical href, claimed by the first of `hooks` that claims it. */
export function decideDestination(
  token: Tokens.Link | Tokens.Image,
  hooks: readonly DestinationHook[] | undefined,
  context: DestinationContext,
): Tokens.Link | Tokens.Image {
  const href = canonicalHref(token.href);
  const canonical = href === token.href ? token : { ...token, href };
  if (hooks) {
    for (const hook of hooks) {
      const claimed = hook(canonical, context);
      if (claimed) return claimed;
    }
  }
  return canonical;
}

/**
 * A bare URL run without the trailing punctuation GFM leaves out, and an
 * unbalanced `)`: `_backpedal` applied until it changes nothing. `run`
 * starts as a bare URL does, with a letter. One pass reads it forward; what
 * later passes take is read from the end (`backpedalEnd`).
 */
export function backpedalUrl(run: string): string {
  const url = inline.gfm._backpedal.exec(run)?.[0] ?? '';
  return url.slice(0, backpedalEnd(url, url.length));
}

// What `_backpedal` leaves off the end of a URL it otherwise matches.
const TRAILING = new Set(['?', '!', '.', ',', ':', ';', '*', '_', "'", '"', '~', ')']);

const isAlphanumeric = (code: number): boolean =>
  (code >= 48 && code <= 57) || (code >= 65 && code <= 90) || (code >= 97 && code <= 122);

/** Whether the `)` at `at` closes a group: the nearest paren before it opens. */
const closesGroup = (url: string, at: number): boolean => {
  for (let i = at - 1; i >= 0; i--) {
    if (url[i] === '(') return true;
    if (url[i] === ')') return false;
  }
  return false;
};

/**
 * Where `_backpedal`, applied until it changes nothing, ends
 * `url.slice(0, end)`, which is one whole match of it (as a match, or a match
 * cut before a `]` or `}` at its end, is). A pass then drops a trailing entity
 * reference (`&amp;`), or the last character of trailing punctuation unless
 * it is a `)` closing a group; each character is read a bounded number of
 * times.
 */
export function backpedalEnd(url: string, end: number): number {
  while (end > 0) {
    const last = url[end - 1];
    if (last === ';') {
      let at = end - 2;
      while (at >= 0 && isAlphanumeric(url.charCodeAt(at))) at--;
      if (at >= 0 && at < end - 2 && url[at] === '&') {
        end = at;
        continue;
      }
    }
    if (!TRAILING.has(last) || (last === ')' && closesGroup(url, end - 1))) return end;
    end--;
  }
  return end;
}
