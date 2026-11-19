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

/**
 * `href` with a scheme-relative `//host/x` as the https URL it names, never
 * a URL on the page's origin. Leading C0 controls and spaces are skipped,
 * as URL parsing skips them.
 */
export function canonicalHref(href: string): string {
  let start = 0;
  while (start < href.length && href.charCodeAt(start) <= 0x20) start++;
  return href.startsWith('//', start) ? `https:${href.slice(start).trimEnd()}` : href;
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

/** A bare URL run without the trailing punctuation GFM leaves out, and an unbalanced `)`. */
export function backpedalUrl(run: string): string {
  let previous;
  do {
    previous = run;
    run = inline.gfm._backpedal.exec(run)?.[0] ?? '';
  } while (previous !== run);
  return run;
}
