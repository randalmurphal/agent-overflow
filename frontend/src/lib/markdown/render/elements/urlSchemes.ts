/**
 * URL scheme policy for rendered links.
 *
 * Any absolute URL renders as a live anchor unless its scheme is on the
 * deny-list below. The list names schemes that execute or read something
 * on the reader's machine when followed, not schemes that are merely
 * unfamiliar: `mailto:`, `tel:`, `vscode://`, `obsidian://`, `slack://`
 * and every other handler the OS registers are the reader's to open.
 *
 * `internal/externalurl/schemes.go` carries the same list for the RPC
 * side, and its test reads this file, so the two cannot drift.
 */
export const DENIED_LINK_SCHEMES: ReadonlySet<string> = new Set([
  // Script and document URLs the webview would evaluate itself.
  'javascript',
  'vbscript',
  'data',
  'blob',
  'about',
  // Java archive protocol handler.
  'jar',
  // Windows protocol handlers with remote-code-execution history
  // (Follina and the search-ms / Office URI handler chains).
  'ms-msdt',
  'search-ms',
  'ms-officecmd',
  'ms-cxh',
  'ms-cxh-full',
  // This app's own scheme. Real path links carry a per-page nonce and are
  // admitted by explicit prefix; a bare one in model text is a forgery.
  'agent-overflow',
]);

/**
 * `file:` is neither denied nor openable as a link: a host that can open
 * files claims it during parsing (`utils/pathLinkExtension.ts`) and the
 * click lands on the editor gate. Unclaimed, it renders as a non-navigable
 * reference so the webview never hands a file URL to the OS opener, which
 * on Windows executes the target.
 */
export const FILE_SCHEME = 'file';

/**
 * Lower-cased scheme of an absolute URL string, or null when there is
 * none. A one-letter scheme (`C:\x`, `c:/x`) is a Windows drive path,
 * not a URL, and answers null.
 */
export function urlScheme(url: string): string | null {
  const match = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(url);
  if (!match || match[1].length === 1) return null;
  return match[1].toLowerCase();
}

/**
 * Whether a link with this href may render as a live anchor, scheme-wise.
 * Callers still parse the URL; this answers only the policy question.
 */
export function isOpenableScheme(scheme: string): boolean {
  return scheme !== FILE_SCHEME && !DENIED_LINK_SCHEMES.has(scheme);
}
