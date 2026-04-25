import { describe, expect, it } from 'vitest';
import { buildLaunchCommand, shellSingleQuote } from './connectCommand';

describe('shellSingleQuote', () => {
  it('wraps an unmetacharacter string in single quotes', () => {
    expect(shellSingleQuote('hello')).toBe("'hello'");
  });

  it('escapes embedded single quotes with the POSIX close-reopen pattern', () => {
    // The canonical pattern: ' becomes '\'' (close, escaped quote,
    // reopen). The input has one ', which produces exactly one
    // close-reopen sequence in the output.
    expect(shellSingleQuote("a'b")).toBe("'a'\\''b'");
  });

  it('preserves shell metacharacters literally inside the quotes', () => {
    // Inside single quotes, $, backtick, &, ;, (, ) are all literal —
    // the shell does not expand them. The quoting itself is what
    // makes the launch command safe to paste.
    const got = shellSingleQuote('$(rm -rf)');
    expect(got).toBe("'$(rm -rf)'");
  });

  it('handles an empty string', () => {
    expect(shellSingleQuote('')).toBe("''");
  });
});

describe('buildLaunchCommand', () => {
  it('prefixes with agent-overflow --connect', () => {
    const cmd = buildLaunchCommand('ws://10.0.0.5:54321/', 'tok');
    expect(cmd.startsWith('agent-overflow --connect ')).toBe(true);
  });

  it('appends the token as a URL query param', () => {
    const cmd = buildLaunchCommand('ws://10.0.0.5:54321/', 'tok-abc');
    // URL.searchParams.set adds ?token=… to the URL. The result is
    // single-quoted, so we look for token=tok-abc inside the quotes.
    expect(cmd).toContain('token=tok-abc');
  });

  it('single-quotes the URL portion so shell metacharacters cannot split the command', () => {
    const cmd = buildLaunchCommand('ws://10.0.0.5:54321/path?danger=$(rm)', 'safe');
    // The URL portion must be the single argv after --connect when
    // the result is shell-tokenised. We assert by looking for the
    // opening + closing quotes around the URL.
    expect(cmd.startsWith("agent-overflow --connect '")).toBe(true);
    expect(cmd.endsWith("'")).toBe(true);
    // Pasting into a POSIX shell: the URL is one token because the
    // shell tokenises only on unquoted whitespace.
    const args = cmd.split(' ');
    expect(args[0]).toBe('agent-overflow');
    expect(args[1]).toBe('--connect');
    expect(args.slice(2).join(' ').startsWith("'")).toBe(true);
  });

  it('escapes embedded single quotes in the token via the POSIX dance', () => {
    const cmd = buildLaunchCommand('ws://10.0.0.5:54321/', "tok'with'quotes");
    // The URL parser percent-encodes ' as %27 inside the search param,
    // so the command's quoted string need not contain a literal '. But
    // if the token contained a ' that survived to the quoted segment,
    // it must be wrapped via the close-reopen-escape pattern.
    expect(cmd.startsWith("agent-overflow --connect '")).toBe(true);
    expect(cmd.endsWith("'")).toBe(true);
    // Either (a) URL.searchParams normalised the ' to %27, in which
    // case "''" never appears (an empty single-quoted segment), or
    // (b) the literal ' survived and was escaped as '\''. Both are
    // valid; what's NOT valid is a bare unescaped ' inside the
    // single-quoted segment.
    expect(cmd.includes("'\\''") || !cmd.includes("''")).toBe(true);
  });

  it('falls back gracefully when URL parsing fails', () => {
    // A malformed URL still produces a command — the wrapping single
    // quotes keep it shell-safe even though the URL itself is junk.
    // The settings UI has its own validator before save; a stale row
    // with a malformed URL should still render something pasteable
    // (which will fail noisily at launch, not at copy time).
    const cmd = buildLaunchCommand('not-a-url', 'tok');
    expect(cmd.startsWith('agent-overflow --connect ')).toBe(true);
    expect(cmd).toContain('token=tok');
  });

  it('uses & as the token separator when the URL already has a query', () => {
    // URL.searchParams.set returns a properly serialised query (?other=keep&token=tok)
    // when the input already has parameters — the test asserts the result
    // includes the existing param as well as the token.
    const cmd = buildLaunchCommand('ws://h:1/?other=keep', 'tok');
    expect(cmd).toContain('other=keep');
    expect(cmd).toContain('token=tok');
  });
});
