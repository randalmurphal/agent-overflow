import { FILE_SCHEME, isOpenableScheme, urlScheme } from './urlSchemes';
import { canonicalHref } from '../../parser/engine/destination';

// WHATWG parsing strips leading and trailing C0 controls and spaces before
// reading the scheme; the scheme checks here see the same string.
const trimUrl = (url: string): string => {
    let start = 0;
    let end = url.length;
    while (start < end && url.charCodeAt(start) <= 0x20)
        start += 1;
    while (end > start && url.charCodeAt(end - 1) <= 0x20)
        end -= 1;
    return url.slice(start, end);
};

/**
 * Absolute-URL parse only, by design. Upstream took a `defaultOrigin`
 * base here, which resolved path-relative (`/x`, `docs/a`) input into
 * passable URLs BEFORE the prefix check, reopening the security boundary
 * (see AGENTS.md § URL and HTML boundary) for any caller that supplied
 * one. No base parameter exists now, so path-relative input fails closed
 * structurally instead of by every caller remembering not to pass it.
 *
 * The one relative form that resolves is the protocol-relative
 * `//host/x`: it names a real host, so it becomes `https://host/x`,
 * never a URL on the app origin.
 *
 * A one-letter scheme (`C:\x`) is a Windows drive path and is not a URL.
 */
export const parseUrl = (url: unknown): URL | null => {
    if (typeof url !== 'string')
        return null;
    const trimmed = trimUrl(canonicalHref(url));
    if (urlScheme(trimmed) === null)
        return null;
    try {
        return new URL(trimmed);
    }
    catch {
        return null;
    }
};

/**
 * The absolute href a link or image may render with, or null.
 *
 * Explicit prefixes match first, exactly as written: a protocol-only
 * prefix (`https://`, `data:image/`, `agent-overflow:open?nonce=…&`)
 * matches by string prefix, a full URL prefix by origin plus prefix.
 * The `*` wildcard then admits any URL whose scheme is openable
 * (`urlSchemes.ts`): every scheme except the deny-list and `file:`.
 */
export const transformUrl = (
    url: unknown,
    allowedPrefixes: string[]
): string | null => {
    const parsedUrl = parseUrl(url);
    if (!parsedUrl)
        return null;
    if (allowedPrefixes.some((prefix) => {
        // Protocol-only prefixes (e.g. 'https://', 'http://', 'mailto:') allow any
        // URL using that protocol. They are not valid absolute URLs on their own
        // (new URL('https://') throws), so we match them with a simple prefix check.
        if (prefix.endsWith('://') || (prefix.endsWith(':') && !prefix.includes('//'))) {
            return parsedUrl.href.startsWith(prefix);
        }
        const parsedPrefix = parseUrl(prefix);
        if (!parsedPrefix) {
            return false;
        }
        if (parsedPrefix.origin !== parsedUrl.origin) {
            return false;
        }
        return parsedUrl.href.startsWith(parsedPrefix.href);
    })) {
        return parsedUrl.href;
    }
    if (allowedPrefixes.includes('*')) {
        const scheme = parsedUrl.protocol.slice(0, -1);
        // `streamdown:` is the parser's own incomplete-construct sentinel,
        // never a URL; the renderers branch on it by exact value.
        if (scheme !== 'streamdown' && isOpenableScheme(scheme)) {
            if (scheme === 'http' || scheme === 'https') {
                return parsedUrl.host ? parsedUrl.href : null;
            }
            return parsedUrl.href;
        }
    }
    return null;
};

/**
 * The src an authored image, video or audio paints, or null: what
 * `transformUrl` admits that a surface can paint. The `*` wildcard admits
 * http(s) only, never on the page's own origin, which would make the app
 * request itself (its transport and API); any other scheme paints only
 * through an explicit prefix (`data:image/`, a host's claimed href). Every
 * candidate and final src of an authored image passes here, in both
 * renderers and the HTML sanitizer, so a src that cannot paint never wins
 * over one that can. Media the app mints itself (a fetched attachment's or
 * local image's bytes) never does. Links are not fetches and use
 * `transformUrl`.
 */
export const transformMediaUrl = (
    url: unknown,
    allowedPrefixes: string[]
): string | null => {
    const admitted = transformUrl(url, allowedPrefixes);
    if (admitted === null) return null;
    if (!/^https?:/i.test(admitted))
        return transformUrl(url, allowedPrefixes.filter((prefix) => prefix !== '*'));
    const page = globalThis.location?.origin;
    return page !== undefined && new URL(admitted).origin === page ? null : admitted;
};

/**
 * How a link token renders. One answer for both renderers
 * (`Link.svelte` and `staticHtml.ts`), so they cannot fork.
 *
 * - `anchor`: a live `<a href>`.
 * - `reference`: not navigable from here, but not a withheld URL either.
 *   Schemeless paths (`docs/guide.md`, `/abs/file`, `#frag`), Windows
 *   drive paths and unclaimed `file:` URLs, the ordinary content of
 *   PR bodies and agent prose. Renders as text with the href as hover
 *   title and no tag.
 * - `blocked`: an absolute URL the policy refused (a denied scheme, or a
 *   prefix list without `*` that it did not match). Renders tagged.
 * - `fragment`: with `fragments`, a `#name` link to an anchor in the same
 *   rendered body. Renders as an anchor the host's click handler resolves
 *   within that body (`data-md-fragment`).
 */
/** The text a blocked link's span shows after the link text. */
export const BLOCKED_LINK_SUFFIX = ' [blocked]';

/** The href the incomplete-markdown pass gives a link still being typed;
 * it renders as an anchor with no target. */
export const INCOMPLETE_LINK_HREF = 'streamdown:incomplete-link';

/** Whether a table cell renders: a cell another merged up into (rowspan
 * 0, extensions/table.ts) does not. Both renderers and find's
 * projection ask this. */
export const tableCellRenders = (cell: { rowspan: number }): boolean => cell.rowspan > 0;

/** The text a link shows after its own: BLOCKED_LINK_SUFFIX for an href
 * the policy blocked, nothing for an anchor, an untagged reference or the
 * incomplete-link sentinel. Both renderers and find's projection ask this. */
export const linkTagText = (href: unknown, allowedPrefixes: string[]): string =>
    href !== INCOMPLETE_LINK_HREF && classifyLinkHref(href, allowedPrefixes).kind === 'blocked'
        ? BLOCKED_LINK_SUFFIX
        : '';

/** A path, fragment, drive path or `file:` URL: a place this page cannot go, not a refused URL. */
const isReference = (href: string): boolean => {
    const scheme = urlScheme(trimUrl(canonicalHref(href)));
    return scheme === null || scheme === FILE_SCHEME;
};

/**
 * An image the gate refused, as both renderers and the HTML sanitizer show
 * it: a chip, not a broken image, titled with what it named. Chrome, not
 * the message's text, so find skips it (`data-find-skip`).
 */
export const UNAVAILABLE_IMAGE_CLASS = 'inline-block rounded bg-surface-2 px-3 py-1 text-sm text-secondary';
export const unavailableImageText = (alt: string): string => `[Image unavailable: ${alt || 'No description'}]`;
export const unavailableImageTitle = (href: string): string =>
    isReference(href) ? `Cannot load from this surface: ${href}` : `Blocked URL: ${href}`;

export type LinkHrefClass =
    | { kind: 'anchor'; href: string }
    | { kind: 'reference'; title: string }
    | { kind: 'blocked'; title: string }
    | { kind: 'fragment'; href: string };

export const classifyLinkHref = (
    href: unknown,
    allowedPrefixes: string[],
    fragments = false
): LinkHrefClass => {
    if (fragments && typeof href === 'string' && href.startsWith('#')) return { kind: 'fragment', href };
    const transformed = transformUrl(href, allowedPrefixes);
    if (transformed !== null) return { kind: 'anchor', href: transformed };
    const raw = typeof href === 'string' ? href : '';
    if (isReference(raw)) {
        return { kind: 'reference', title: raw };
    }
    return { kind: 'blocked', title: `Blocked URL: ${raw}` };
};
