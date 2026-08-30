/**
 * Absolute-URL parse only, by design. Upstream took a `defaultOrigin`
 * base here, which resolved path-relative (`/x`, `docs/a`) and
 * protocol-relative (`//host/x`) input into passable URLs BEFORE the
 * prefix check — reopening the security boundary (see AGENTS.md
 * § Security boundary) for any caller that supplied one. No base
 * parameter exists now, so relative input fails closed structurally
 * instead of by every caller remembering not to pass it. That also
 * means every URL leaving transformUrl is absolute: upstream's
 * relative-output reconstruction path is gone with the base.
 */
export const parseUrl = (url: unknown): URL | null => {
    if (typeof url !== 'string')
        return null;
    try {
        return new URL(url);
    }
    catch {
        return null;
    }
};
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
    // Wildcard allows any http(s) URL.
    if (allowedPrefixes.includes('*') &&
        (parsedUrl.protocol === 'https:' || parsedUrl.protocol === 'http:')) {
        return parsedUrl.href;
    }
    return null;
};
