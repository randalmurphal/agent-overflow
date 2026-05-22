import { describe, expect, it } from 'vitest';
import { Lexer } from 'marked';

import {
  PATH_LINK_HREF_PREFIX,
  buildPathLinkExtension,
  buildPathLinkHref,
  parsePathLinkHref,
} from './pathLinkExtension';

// Marked exposes its inline-extension surface via Lexer.lex's internal
// options. The streamdown library wires the same shape through
// `parseExtensions`; here we drive Lexer directly to assert the
// tokenizer's contract without pulling in the entire streamdown
// runtime.
function lex(text: string, extension: ReturnType<typeof buildPathLinkExtension>) {
  if (!extension) throw new Error('extension must not be undefined');
  const lexer = new Lexer({
    gfm: true,
    extensions: {
      block: [],
      inline: [extension.tokenizer.bind(extension)],
      childTokens: {},
      renderers: {},
      startBlock: [],
      startInline: [extension.start.bind(extension)],
    },
    // marked's options bag carries more fields, but the inline lexer
    // only reads what we listed above.
  } as never);
  return lexer.lex(text);
}

interface FoundLink {
  raw: string;
  href: string;
  childTypes: string[];
}

function findLinks(tokens: ReturnType<typeof lex>): FoundLink[] {
  const out: FoundLink[] = [];
  const walk = (nodes: unknown[]) => {
    for (const node of nodes) {
      if (!node || typeof node !== 'object') continue;
      const t = node as { type?: string; raw?: string; href?: string; tokens?: unknown[] };
      if (t.type === 'link' && typeof t.raw === 'string' && typeof t.href === 'string') {
        const children = Array.isArray(t.tokens) ? t.tokens : [];
        const childTypes = children
          .map((c) => (c && typeof c === 'object' ? ((c as { type?: string }).type ?? '') : ''))
          .filter((s) => s !== '');
        out.push({ raw: t.raw, href: t.href, childTypes });
      }
      if (Array.isArray(t.tokens)) walk(t.tokens);
    }
  };
  walk(tokens as unknown[]);
  return out;
}

function findCodespans(tokens: ReturnType<typeof lex>): string[] {
  const out: string[] = [];
  const walk = (nodes: unknown[]) => {
    for (const node of nodes) {
      if (!node || typeof node !== 'object') continue;
      const t = node as { type?: string; raw?: string; tokens?: unknown[] };
      if (t.type === 'codespan' && typeof t.raw === 'string') out.push(t.raw);
      if (Array.isArray(t.tokens)) walk(t.tokens);
    }
  };
  walk(tokens as unknown[]);
  return out;
}

describe('buildPathLinkExtension', () => {
  it('returns undefined for empty allowlist (no extension needed)', () => {
    expect(buildPathLinkExtension([], '')).toBeUndefined();
    // Defensive — refs without `path` are filtered out, so an array of
    // those collapses to an empty allowlist.
    expect(buildPathLinkExtension([{ path: '' }], '')).toBeUndefined();
  });

  it('linkifies an allowlisted path in prose', () => {
    const ext = buildPathLinkExtension([{ path: 'src/lib/foo.ts' }], '/workspace');
    const tokens = lex('See src/lib/foo.ts for impl.', ext);
    const links = findLinks(tokens);
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/lib/foo.ts');
    // Bare-form anchors wrap a plain `text` child (not codespan); the
    // wrapped-form tests below pin the opposite shape so a future
    // refactor that swaps childKind cannot pass both suites silently.
    expect(links[0].childTypes).toEqual(['text']);
    expect(links[0].href).toContain('path=src%2Flib%2Ffoo.ts');
    expect(links[0].href).toContain('workspace=%2Fworkspace');
    expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
  });

  it('extracts :line:col suffix from the matched text', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('See src/foo.ts:42:7 here.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/foo.ts:42:7');
    expect(links[0].href).toContain('line=42');
    expect(links[0].href).toContain('col=7');
  });

  it('extracts :line without col', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Line src/foo.ts:99.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/foo.ts:99');
    expect(links[0].href).toContain('line=99');
    expect(links[0].href).not.toContain('col=');
  });

  it('ignores paths NOT in the allowlist (server validation is authoritative)', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('See src/other/bar.ts here.', ext));
    expect(links).toHaveLength(0);
  });

  // Backtick-wrapped allowlisted paths linkify AS code spans — the
  // emitted link token carries a `codespan` child so the rendered DOM
  // is `<a href="agent-overflow:open?…"><code>src/foo.ts</code></a>`.
  // This is the dominant convention for paths in technical prose; the
  // visible monospace pill stays a clickable anchor.
  it('linkifies an allowlisted path wrapped in a code span', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Use `src/foo.ts` for that.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('`src/foo.ts`');
    expect(links[0].childTypes).toEqual(['codespan']);
    expect(links[0].href).toContain('path=src%2Ffoo.ts');
  });

  it('linkifies a wrapped path with :line:col inside the backticks', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Edit `src/foo.ts:42:7` please.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('`src/foo.ts:42:7`');
    expect(links[0].childTypes).toEqual(['codespan']);
    expect(links[0].href).toContain('line=42');
    expect(links[0].href).toContain('col=7');
  });

  // The boundary check on the wrapped form must reject `xfoo.ts`-like
  // shapes where the backtick is itself preceded by an alphanumeric.
  // This matches the bare branch's existing email-shaped guard.
  it('does NOT linkify a wrapped path when the backtick is glued to alphanumeric', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const tokens = lex('See x`src/foo.ts` here.', ext);
    expect(findLinks(tokens)).toHaveLength(0);
    // Built-in codespan still renders the styled pill — the wrapped
    // branch only declines the LINK promotion; the underlying
    // `<code>` survives.
    expect(findCodespans(tokens)).toEqual(['`src/foo.ts`']);
  });

  // Non-allowlisted paths inside code spans must NOT linkify; they
  // remain plain `<code>` rendered by marked's built-in codespan
  // tokenizer. Server-side validation is authoritative, just like for
  // the bare branch.
  it('leaves non-allowlisted code spans untouched (fall through to codespan)', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const tokens = lex('See `src/other/bar.ts` not allowlisted.', ext);
    expect(findLinks(tokens)).toHaveLength(0);
    expect(findCodespans(tokens)).toEqual(['`src/other/bar.ts`']);
  });

  // Pin behaviour for an external `:42` suffix written outside the
  // backticks. The convention agents follow is colon INSIDE the
  // backticks (matched above), so external suffixes are noise that
  // becomes plain text after the link.
  it('emits the wrapped link without consuming a trailing external :line', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Edit `src/foo.ts`:42 (external) please.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('`src/foo.ts`');
    expect(links[0].href).not.toContain('line=');
  });

  // `@`-prefix inside the backticks is the mention shape agents use
  // when referencing a file in a code-styled pill. The wrapped regex
  // captures `(@)?`, so the inner codespan keeps the `@` while the
  // href path stays unprefixed.
  it('keeps the leading @ inside a wrapped code span', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const tokens = lex('Mention `@src/foo.ts` here.', ext);
    const links = findLinks(tokens);
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('`@src/foo.ts`');
    expect(links[0].childTypes).toEqual(['codespan']);
    expect(links[0].href).toContain('path=src%2Ffoo.ts');
    // The @ is presentation-only; it must NOT smuggle into the href.
    expect(links[0].href).not.toContain('path=%40');
  });

  // Mixed bare + wrapped paths in one paragraph exercise the two
  // tokenizer branches sequentially. A future refactor that shares
  // regex state between branches would regress here even if each
  // branch were tested in isolation.
  it('linkifies bare and wrapped paths in the same paragraph', () => {
    const ext = buildPathLinkExtension(
      [{ path: 'src/foo.ts' }, { path: 'internal/router.go' }],
      '',
    );
    const links = findLinks(
      lex('See src/foo.ts and `internal/router.go` together.', ext),
    );
    expect(links).toHaveLength(2);
    expect(links[0].raw).toBe('src/foo.ts');
    expect(links[0].childTypes).toEqual(['text']);
    expect(links[1].raw).toBe('`internal/router.go`');
    expect(links[1].childTypes).toEqual(['codespan']);
  });

  it('linkifies two wrapped paths in the same paragraph', () => {
    const ext = buildPathLinkExtension(
      [{ path: 'src/foo.ts' }, { path: 'internal/router.go' }],
      '',
    );
    const links = findLinks(
      lex('Compare `src/foo.ts` with `internal/router.go` here.', ext),
    );
    expect(links).toHaveLength(2);
    expect(links[0].raw).toBe('`src/foo.ts`');
    expect(links[1].raw).toBe('`internal/router.go`');
    expect(links.every((l) => l.childTypes[0] === 'codespan')).toBe(true);
  });

  it('skips matches inside fenced code blocks', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const tokens = lex('Pre:\n```\nsrc/foo.ts:42\n```\n', ext);
    expect(findLinks(tokens)).toHaveLength(0);
  });

  it('linkifies multiple paths in one paragraph', () => {
    const ext = buildPathLinkExtension(
      [{ path: 'src/foo.ts' }, { path: 'internal/router.go' }],
      '',
    );
    const links = findLinks(lex('Both src/foo.ts and internal/router.go matter.', ext));
    expect(links).toHaveLength(2);
    expect(links[0].raw).toBe('src/foo.ts');
    expect(links[1].raw).toBe('internal/router.go');
  });

  it('widens to include a leading @ when preceded by a boundary', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Mention @src/foo.ts here.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('@src/foo.ts');
  });

  it('does NOT linkify when an alphanumeric immediately precedes (email-like)', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    // `name@src/foo.ts` — the `@` is preceded by `e` (alphanumeric).
    // Old DOM walker rejected this; the marked extension must too.
    const links = findLinks(lex('Email name@src/foo.ts here.', ext));
    expect(links).toHaveLength(0);
  });

  it('does NOT linkify when a path is glued to a longer non-boundary prefix', () => {
    const ext = buildPathLinkExtension([{ path: 'foo.ts' }], '');
    // `xfoo.ts` — the `f` is preceded by `x` which is alphanumeric.
    const links = findLinks(lex('See xfoo.ts here.', ext));
    expect(links).toHaveLength(0);
  });

  it('prefers the longest allowlisted path when paths nest', () => {
    const ext = buildPathLinkExtension(
      [{ path: 'foo.ts' }, { path: 'src/lib/foo.ts' }],
      '',
    );
    const links = findLinks(lex('Edit src/lib/foo.ts please.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/lib/foo.ts');
  });

  it('survives an empty workspacePath without encoding "workspace=" param', () => {
    const ext = buildPathLinkExtension([{ path: 'foo.ts' }], '');
    const links = findLinks(lex('foo.ts here.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].href).not.toContain('workspace=');
  });

  // Marked's built-in emphasis tokenizer can consume `*foo*` as an `em`
  // token before our inline extension sees the rest of the path token.
  // The allowlist can never contain a literal path body with `*` in it
  // (Go's pathlinks regex rejects `*`), so this case decides whether
  // `internal/*foo*.go` linkifies as a single span (it would have to
  // span the em boundary) or splits at the em and leaves the path
  // un-linked. We pin the latter outcome so a future marked /
  // streamdown bump that changes inline ordering does not silently
  // start linkifying a token whose middle has been re-tokenized as
  // emphasis.
  it('does NOT linkify a path whose body crosses a markdown emphasis run', () => {
    const ext = buildPathLinkExtension([{ path: 'internal/foo.go' }], '');
    const tokens = lex('See internal/*foo*.go please.', ext);
    expect(findLinks(tokens)).toHaveLength(0);
  });

  // Path adjacent to (but not crossing) an emphasis run still links —
  // emphasis is consumed independently. This is the partner case to
  // the test above: same input shape, different position, opposite
  // outcome — together they pin the boundary precisely.
  it('linkifies a path that sits next to an unrelated emphasis run', () => {
    const ext = buildPathLinkExtension([{ path: 'internal/foo.go' }], '');
    const links = findLinks(lex('See internal/foo.go *important*.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('internal/foo.go');
  });
});

describe('buildPathLinkHref', () => {
  it('omits line and col when zero or undefined', () => {
    expect(buildPathLinkHref('src/foo.ts', undefined, undefined, '/ws')).toBe(
      `${PATH_LINK_HREF_PREFIX}path=src%2Ffoo.ts&workspace=%2Fws`,
    );
    expect(buildPathLinkHref('src/foo.ts', 0, 0, '/ws')).toBe(
      `${PATH_LINK_HREF_PREFIX}path=src%2Ffoo.ts&workspace=%2Fws`,
    );
  });

  it('preserves a workspace-less href', () => {
    expect(buildPathLinkHref('foo.ts', 1, 2, '')).toBe(
      `${PATH_LINK_HREF_PREFIX}path=foo.ts&line=1&col=2`,
    );
  });

  it('encodes a session nonce that is unguessable from agent prose', () => {
    // The whole point of baking the nonce into the prefix is that
    // Streamdown's URL filter rejects raw agent-written markdown links
    // (`[click](agent-overflow:open?path=…)`) — the agent only sees
    // its own text, never the rendered DOM. Verify the nonce is
    // present and not the empty string.
    const match = /nonce=([0-9a-f]+)/.exec(PATH_LINK_HREF_PREFIX);
    expect(match).not.toBeNull();
    expect(match![1].length).toBeGreaterThanOrEqual(16);
  });
});

describe('parsePathLinkHref', () => {
  it('round-trips the build output', () => {
    const href = buildPathLinkHref('src/foo.ts', 42, 7, '/workspace');
    expect(parsePathLinkHref(href)).toEqual({
      path: 'src/foo.ts',
      line: 42,
      col: 7,
      workspacePath: '/workspace',
    });
  });

  it('defaults line/col to 0 and workspace to empty', () => {
    const href = buildPathLinkHref('a.ts', undefined, undefined, '');
    expect(parsePathLinkHref(href)).toEqual({
      path: 'a.ts',
      line: 0,
      col: 0,
      workspacePath: '',
    });
  });

  it('rejects hrefs that are not ours', () => {
    expect(parsePathLinkHref(null)).toBeNull();
    expect(parsePathLinkHref(undefined)).toBeNull();
    expect(parsePathLinkHref('')).toBeNull();
    expect(parsePathLinkHref('https://example.com')).toBeNull();
    // Right scheme but missing the nonce — this is the
    // raw-markdown-bypass attack surface; must be rejected.
    expect(parsePathLinkHref('agent-overflow:open?path=/etc/passwd')).toBeNull();
    // Right scheme + nonce but no path param.
    expect(parsePathLinkHref(PATH_LINK_HREF_PREFIX)).toBeNull();
  });
});
