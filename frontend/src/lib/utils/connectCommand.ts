// connectCommand.ts builds the operator-side `agent-overflow --connect`
// shell command for a saved remote endpoint. The launch path takes a
// URL with the token embedded as a query param, and the resulting
// command is meant to be pasted into a shell — so the URL portion must
// be wrapped in single quotes to keep shell metacharacters inside the
// token or query string from splitting the command at the shell.
//
// Without quoting, tokens or URL params containing `&`, `;`, `$`,
// backtick, `(`, `)`, `!`, etc. would split the command. At best the
// launch fails noisily; at worst the user pastes a token containing
// `&& rm -rf` into bash and we've shipped an injection. This module is
// the single source of truth for the assembly so the unit tests pin
// the quoting contract and any caller (settings panel, future CLI
// helper) shares the same implementation.

/**
 * shellSingleQuote wraps a string for safe POSIX-shell pasting. Single
 * quotes preserve everything literally except embedded single quotes,
 * which we escape via the canonical `'\''` close-reopen-escape-reopen
 * dance.
 *
 * This is the only escape we need: pure single-quote wrapping with the
 * close-escape-reopen pattern works everywhere POSIX `sh` runs (bash,
 * zsh, dash, ash, busybox sh). Double quotes would still expand `$` and
 * backtick, defeating the purpose.
 */
export function shellSingleQuote(s: string): string {
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

/**
 * buildLaunchCommand renders the operator-side `agent-overflow --connect`
 * string for a stored endpoint URL + token. The token is appended via
 * the URL query because that's what main.go's `--connect` parser
 * expects. The resulting URL is single-quoted so shell metacharacters
 * in the token or query params can't split the command.
 *
 * Falls back to manual concatenation if URL parsing fails — the
 * single-quote wrapping below still keeps the resulting command
 * shell-safe even on malformed input. We don't reject malformed URLs
 * here because the caller (settings UI) has its own validator and
 * we'd rather render *something* useful than throw on a stale row.
 */
export function buildLaunchCommand(url: string, token: string): string {
  let urlString: string;
  try {
    const parsed = new URL(url);
    parsed.searchParams.set('token', token);
    urlString = parsed.toString();
  } catch {
    const sep = url.includes('?') ? '&' : '?';
    urlString = `${url}${sep}token=${encodeURIComponent(token)}`;
  }
  return `agent-overflow --connect ${shellSingleQuote(urlString)}`;
}
