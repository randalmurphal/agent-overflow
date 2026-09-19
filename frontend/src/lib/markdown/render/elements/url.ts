import { FILE_SCHEME, isOpenableScheme, urlScheme } from './urlSchemes';

// WHATWG parsing strips leading and trailing C0 controls and spaces before
// reading the scheme; the scheme checks here see the same string.
const trimUrl = (url: string): string =>
    url.replace(/^[\u0000-\u0020]+|[\u0000-\u0020]+$/g, '');

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
    const trimmed = trimUrl(url);
    if (trimmed.startsWith('//'))
        return parseUrl(`https:${trimmed}`);
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
 */
export type LinkHrefClass =
    | { kind: 'anchor'; href: string }
    | { kind: 'reference'; title: string }
    | { kind: 'blocked'; title: string };

export const classifyLinkHref = (
    href: unknown,
    allowedPrefixes: string[]
): LinkHrefClass => {
    const transformed = transformUrl(href, allowedPrefixes);
    if (transformed !== null) return { kind: 'anchor', href: transformed };
    const raw = typeof href === 'string' ? href : '';
    const trimmed = trimUrl(raw);
    const scheme = trimmed.startsWith('//') ? 'https' : urlScheme(trimmed);
    if (scheme === null || scheme === FILE_SCHEME) {
        return { kind: 'reference', title: raw };
    }
    return { kind: 'blocked', title: `Blocked URL: ${raw}` };
};
