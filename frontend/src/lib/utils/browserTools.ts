// The browser tools an agent drives, and which machines have them.
//
// The MCP server's name is stated in `internal/browser/types.go` as
// `ServerName`, and repeated here because a tool row's `meta.mcp.server` is
// the only thing that identifies one. If it changes there, it changes here.
//
// The capability is separate from the name and answers a different question.
// A machine that cannot run a headless browser advertises no `browser` in
// its hello, and every surface that offers to send work there says so first,
// rather than letting a turn discover it by failing.

import { getTransportHelloFor } from '../stores/transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';

/** Mirrors `internal/browser/types.go#ServerName`. */
export const BROWSER_TOOLS_SERVER = 'ao-browser-tools';

/** The hello capability a machine with browser tools advertises. */
export const BROWSER_CAPABILITY = 'browser';

/**
 * Whether that machine can drive a browser.
 *
 * A machine that has not sent a hello yet answers false, in common with
 * every other capability read: a feature degrades rather than being
 * attempted against a backend that may not serve it.
 */
export function backendHasBrowser(key: BackendKey): boolean {
  return getTransportHelloFor(key)?.capabilities.includes(BROWSER_CAPABILITY) ?? false;
}

/**
 * Whether a tool row is one of the browser tools, read from the `meta.mcp`
 * pair both providers' parsers stamp onto the item.
 */
export function isBrowserToolMeta(itemMeta: Record<string, unknown> | null): boolean {
  const mcp = itemMeta?.mcp;
  if (!mcp || typeof mcp !== 'object' || Array.isArray(mcp)) return false;
  return (mcp as Record<string, unknown>).server === BROWSER_TOOLS_SERVER;
}
