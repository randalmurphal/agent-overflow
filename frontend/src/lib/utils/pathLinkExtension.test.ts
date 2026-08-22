import { describe, expect, it } from 'vitest';
import { Lexer } from 'marked';

import {
  LOCAL_IMAGE_HREF_PREFIX,
  PATH_LINK_HREF_PREFIX,
  buildPathLinkExtension,
  buildPathLinkHref,
  parseLocalImageHref,
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
      inline: [extension.tokenizer],
      childTokens: {},
      renderers: {},
      startBlock: [],
      startInline: [extension.start],
    },
    // marked's options bag carries more fields, but the inline lexer
    // only reads what we listed above.
  } as never);
  return lexer.lex(text);
}

interface FoundLink {
  raw: string;
  href: string;
  title: string | null;
  childTypes: string[];
}

function findLinks(tokens: ReturnType<typeof lex>): FoundLink[] {
  const out: FoundLink[] = [];
  const walk = (nodes: unknown[]) => {
    for (const node of nodes) {
      if (!node || typeof node !== 'object') continue;
      const t = node as {
        type?: string;
        raw?: string;
        href?: string;
        title?: string | null;
        tokens?: unknown[];
      };
      if (t.type === 'link' && typeof t.raw === 'string' && typeof t.href === 'string') {
        const children = Array.isArray(t.tokens) ? t.tokens : [];
        const childTypes = children
          .map((c) => (c && typeof c === 'object' ? ((c as { type?: string }).type ?? '') : ''))
          .filter((s) => s !== '');
        out.push({ raw: t.raw, href: t.href, title: t.title ?? null, childTypes });
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

function findImages(tokens: ReturnType<typeof lex>): Array<{ raw: string; href: string; text: string }> {
  const out: Array<{ raw: string; href: string; text: string }> = [];
  const walk = (nodes: unknown[]) => {
    for (const node of nodes) {
      if (!node || typeof node !== 'object') continue;
      const token = node as {
        type?: string;
        raw?: string;
        href?: string;
        text?: string;
        tokens?: unknown[];
      };
      if (
        token.type === 'image' &&
        typeof token.raw === 'string' &&
        typeof token.href === 'string'
      ) {
        out.push({ raw: token.raw, href: token.href, text: token.text ?? '' });
      }
      if (Array.isArray(token.tokens)) walk(token.tokens);
    }
  };
  walk(tokens as unknown[]);
  return out;
}

describe('buildPathLinkExtension', () => {
  it('builds an extension for an empty allowlist when a workspace exists (markdown-link half stays active)', () => {
    // Refs without `path` are filtered out, so an array of those
    // collapses to an empty allowlist — same as `[]`.
    for (const ext of [
      buildPathLinkExtension([], '/workspace'),
      buildPathLinkExtension([{ path: '' }], '/workspace'),
    ]) {
      expect(ext).toBeDefined();
      // Prose paths are never invented without the server allowlist…
      expect(findLinks(lex('see src/foo.ts here', ext))).toHaveLength(0);
      // …but a path-shaped markdown-link href is still rewritten.
      const links = findLinks(lex('[notes](/home/user/notes.md)', ext));
      expect(links).toHaveLength(1);
      expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
    }
  });

  it('returns undefined when both halves would be inert (empty allowlist, no workspace)', () => {
    // No prose to linkify and no workspace to anchor href rewriting:
    // callers hand marked no extension at all, which keeps streamdown's
    // extension-identity lex cache on its fast path for those surfaces
    // (PR bodies, review comments).
    expect(buildPathLinkExtension([], '')).toBeUndefined();
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
    expect(links[0].title).toBe('Open src/lib/foo.ts in editor');
  });

  it('extracts :line:col suffix from the matched text', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('See src/foo.ts:42:7 here.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/foo.ts:42:7');
    expect(links[0].href).toContain('line=42');
    expect(links[0].href).toContain('col=7');
    expect(links[0].title).toBe('Open src/foo.ts:42:7 in editor');
  });

  it('extracts :line without col', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Line src/foo.ts:99.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/foo.ts:99');
    expect(links[0].href).toContain('line=99');
    expect(links[0].href).not.toContain('col=');
  });

  // The bare-form failure mode for ranges is softer than the wrapped
  // case: without consuming `-endLine`, bareRe matches `src/foo.ts:18`
  // and leaves `-23` as trailing plain text (visible as a blue pill
  // glued to a gray `-23`). The suffix change consumes the range bound
  // so the full token becomes the link surface, while the href stays
  // at the start line.
  it('linkifies a bare path with a :line-endLine range, opening at start line', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('See src/foo.ts:18-23 for context.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('src/foo.ts:18-23');
    expect(links[0].href).toContain('line=18');
    expect(links[0].href).not.toContain('line=23');
    expect(links[0].href).not.toContain('endLine');
    expect(links[0].href).not.toContain('col=');
  });

  // Wrapped + range is the regression case from the screenshot. With
  // the old suffix, wrappedRe required the closing backtick to land
  // directly after `:line(:col)?`; `-23` blocked the match and marked
  // fell through to its built-in codespan, rendering a plain pill.
  // After the fix the full `:18-23` shape is consumed, the wrapped
  // form succeeds, and the link emits at the start line.
  it('linkifies a wrapped path with a :line-endLine range', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
    const links = findLinks(lex('Edit `src/foo.ts:18-23` please.', ext));
    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('`src/foo.ts:18-23`');
    expect(links[0].childTypes).toEqual(['codespan']);
    expect(links[0].href).toContain('line=18');
    expect(links[0].href).not.toContain('line=23');
    expect(links[0].href).not.toContain('endLine');
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

  it('rewrites an allowlisted markdown link href to the open-in-editor scheme', () => {
    const path = '/workspace/src/external_jwt.py';
    const ext = buildPathLinkExtension([{ path }], '/workspace');
    const links = findLinks(lex(`[external_jwt.py](${path}:636)`, ext));

    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe(`[external_jwt.py](${path}:636)`);
    expect(links[0].childTypes).toEqual(['text']);
    expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
    expect(links[0].href).toContain(`path=${encodeURIComponent(path)}`);
    expect(links[0].href).toContain('line=636');
    expect(links[0].href).toContain('workspace=%2Fworkspace');
  });

  it('rewrites an adjacent allowlisted markdown link', () => {
    const path = '/workspace/src/external_jwt.py';
    const ext = buildPathLinkExtension([{ path }], '/workspace');
    const links = findLinks(lex(`prefix[external_jwt.py](${path}:636)`, ext));

    expect(links).toHaveLength(1);
    expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
    expect(links[0].href).toContain('line=636');
  });

  it('extracts :line:col from an allowlisted markdown link href', () => {
    const path = '/workspace/src/external_jwt.py';
    const ext = buildPathLinkExtension([{ path }], '/workspace');
    const links = findLinks(lex(`[external_jwt.py](${path}:636:9)`, ext));

    expect(links).toHaveLength(1);
    expect(links[0].href).toContain('line=636');
    expect(links[0].href).toContain('col=9');
  });

  it('extracts the start line from an allowlisted markdown link href range', () => {
    const path = '/workspace/src/external_jwt.py';
    const ext = buildPathLinkExtension([{ path }], '/workspace');
    const links = findLinks(lex(`[external_jwt.py](${path}:636-640)`, ext));

    expect(links).toHaveLength(1);
    expect(links[0].href).toContain('line=636');
    expect(links[0].href).not.toContain('line=640');
    expect(links[0].href).not.toContain('col=');
  });

  it('does not create nested path links when a markdown link label is also a path', () => {
    const ext = buildPathLinkExtension([{ path: 'src/foo.ts' }], '/workspace');
    const links = findLinks(lex('[src/foo.ts](src/foo.ts:42)', ext));

    expect(links).toHaveLength(1);
    expect(links[0].raw).toBe('[src/foo.ts](src/foo.ts:42)');
    expect(links[0].childTypes).toEqual(['text']);
    expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
    expect(links[0].href).toContain('line=42');
  });

  it('rewrites a path-shaped markdown link even when the href is not allowlisted', () => {
    // The allowlist gates PROSE linkification only. A markdown link's
    // href is an explicit destination the user must click, so it
    // becomes an editor affordance and the backend gates the open at
    // click time (editor.ResolvePath).
    const ext = buildPathLinkExtension([{ path: '/workspace/src/external_jwt.py' }], '/workspace');
    const links = findLinks(lex('[other.py](/workspace/src/other.py:636)', ext));

    expect(links).toHaveLength(1);
    expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
    expect(links[0].href).toContain('path=%2Fworkspace%2Fsrc%2Fother.py');
    expect(links[0].href).toContain('line=636');
  });

  describe('path-shaped markdown-link hrefs (no allowlist entry)', () => {
    const ext = () => buildPathLinkExtension([], '/workspace');

    it('rewrites an absolute href', () => {
      const links = findLinks(lex('[prompt](/home/user/.claude/prompt.md)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
      expect(links[0].href).toContain('path=%2Fhome%2Fuser%2F.claude%2Fprompt.md');
    });

    it('normalizes a local file URI into the guarded editor link', () => {
      const links = findLinks(lex('[entity](file:///workspace/models/event.py:42:7)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
      expect(links[0].href).toContain('path=%2Fworkspace%2Fmodels%2Fevent.py');
      expect(links[0].href).toContain('line=42');
      expect(links[0].href).toContain('col=7');
      expect(links[0].title).toBe('Open /workspace/models/event.py:42:7 in editor');
    });

    it('reads GitHub-style line fragments from local file URIs', () => {
      const links = findLinks(lex('[entity](file:///workspace/models/event.py#L42-L48)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=%2Fworkspace%2Fmodels%2Fevent.py');
      expect(links[0].href).toContain('line=42');
      expect(links[0].href).not.toContain('col=');
    });

    it('accepts localhost file URIs but refuses remote file authorities', () => {
      const local = findLinks(lex('[entity](file://localhost/workspace/models/event.py)', ext()));
      expect(local[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(true);
      expect(local[0].href).toContain('path=%2Fworkspace%2Fmodels%2Fevent.py');

      const remote = findLinks(lex('[entity](file://fileserver/share/event.py)', ext()));
      expect(remote[0].href.startsWith(PATH_LINK_HREF_PREFIX)).toBe(false);
    });

    it('normalizes a Windows drive file URI without keeping the URL root slash', () => {
      const links = findLinks(lex('[entity](file:///C:/repo/models/event.py#L9)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=C%3A%2Frepo%2Fmodels%2Fevent.py');
      expect(links[0].href).toContain('line=9');
    });

    it('rewrites a ~/ href (expansion happens backend-side)', () => {
      const links = findLinks(lex('[prompt](~/.claude/prompt.md)', ext()));
      expect(links).toHaveLength(1);
      // URLSearchParams percent-encodes `~` (unlike encodeURIComponent).
      expect(links[0].href).toContain('path=%7E%2F.claude%2Fprompt.md');
    });

    it('rewrites a workspace-relative href when a workspace is present', () => {
      const links = findLinks(lex('[doc](docs/specs/theme-system.md)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=docs%2Fspecs%2Ftheme-system.md');
      expect(links[0].href).toContain('workspace=%2Fworkspace');
    });

    it('rewrites NO shape when no workspace is available — absolute and ~/ included', () => {
      // Security boundary, not just UX: workspace-less surfaces are the
      // third-party ones (PR bodies, review comments), and a
      // workspace-less href would land on editor.ResolvePath's
      // stat-free project-open pass-through. The extension here is
      // alive (non-empty allowlist) but its href half must stay off.
      const noWorkspace = buildPathLinkExtension([{ path: 'src/foo.ts' }], '');
      for (const href of ['/etc', '/home/user/notes.md', '~/.ssh', '~', 'docs/foo.md']) {
        const links = findLinks(lex(`[label](${href})`, noWorkspace));
        expect(links).toHaveLength(1);
        expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX), href).toBe(false);
      }
    });

    it('strips a trailing #fragment or ?query from a rewritten href', () => {
      const withFragment = findLinks(lex('[install](docs/guide.md#install)', ext()));
      expect(withFragment).toHaveLength(1);
      expect(withFragment[0].href).toContain('path=docs%2Fguide.md');
      expect(withFragment[0].href).not.toContain('install');

      const withQuery = findLinks(lex('[doc](/workspace/a.md?x=1)', ext()));
      expect(withQuery).toHaveLength(1);
      expect(withQuery[0].href).toContain('path=%2Fworkspace%2Fa.md');
      expect(withQuery[0].href).not.toContain('x%3D1');
    });

    it('percent-decodes the path (the standard markdown spelling of a space)', () => {
      const links = findLinks(lex('[doc](/workspace/my%20file.md)', ext()));
      expect(links).toHaveLength(1);
      // URLSearchParams re-encodes the decoded space as `+`.
      expect(links[0].href).toContain('path=%2Fworkspace%2Fmy+file.md');
    });

    it('keeps a malformed percent-escape as literal text instead of dropping the link', () => {
      const links = findLinks(lex('[doc](/workspace/100%.md)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=%2Fworkspace%2F100%25.md');
    });

    it('treats a single-segment name with :line as a path, not a URI scheme', () => {
      const links = findLinks(lex('[build](Makefile:12)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=Makefile');
      expect(links[0].href).toContain('line=12');
    });

    it('keeps a multi-colon tail on the path rather than mis-splitting it', () => {
      // `:1:2:3` is not a valid `:line[:col]` suffix. Taking the LAST
      // valid-looking pair would split as path `/workspace/a.md:1`,
      // line 2, col 3 — the whole string must stay the path instead.
      const links = findLinks(lex('[x](/workspace/a.md:1:2:3)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=%2Fworkspace%2Fa.md%3A1%3A2%3A3');
      expect(links[0].href).not.toContain('line=');
    });

    it('extracts the start line from a rewritten href range suffix', () => {
      const links = findLinks(lex('[x](/workspace/a.md:10-20)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=%2Fworkspace%2Fa.md');
      expect(links[0].href).toContain('line=10');
      expect(links[0].href).not.toContain('col=');
    });

    it('splits a :line:col suffix off a rewritten href', () => {
      const links = findLinks(lex('[spot](/workspace/src/a.go:42:7)', ext()));
      expect(links).toHaveLength(1);
      expect(links[0].href).toContain('path=%2Fworkspace%2Fsrc%2Fa.go');
      expect(links[0].href).toContain('line=42');
      expect(links[0].href).toContain('col=7');
    });

    it('never rewrites unsupported schemes, fragment, query, network-path, or UNC hrefs', () => {
      const cases = [
        'https://example.com/x',
        // Port shapes: the trailing `:8080` parses like a line suffix,
        // but the remainder still carries the scheme and is refused.
        'http://example.com:8080',
        'mailto:a@b.c',
        'agent-overflow:open?path=/etc/passwd',
        '#section',
        '?q=1',
        '//host/share/x',
        '\\\\host\\share\\x',
      ];
      for (const href of cases) {
        const links = findLinks(lex(`[label](${href})`, ext()));
        expect(links).toHaveLength(1);
        // Not exact-equality: marked unescapes backslashes in the UNC
        // case. The contract is "never becomes an editor link".
        expect(links[0].href.startsWith(PATH_LINK_HREF_PREFIX), href).toBe(false);
      }
    });
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

  it('rewrites a local file image into a guarded backend-image URL', () => {
    const ext = buildPathLinkExtension([], '/workspace');
    const images = findImages(lex('![diagram](file:///workspace/docs/flow.png)', ext));
    expect(images).toHaveLength(1);
    expect(images[0].href.startsWith(LOCAL_IMAGE_HREF_PREFIX)).toBe(true);
    expect(images[0].href).toContain('path=%2Fworkspace%2Fdocs%2Fflow.png');
    expect(images[0].href).toContain('workspace=%2Fworkspace');
  });

  it('does not rewrite remote-authority or non-file image URLs', () => {
    const ext = buildPathLinkExtension([], '/workspace');
    for (const href of ['file://fileserver/share/x.png', 'https://example.com/x.png']) {
      const images = findImages(lex(`![diagram](${href})`, ext));
      expect(images).toHaveLength(1);
      expect(images[0].href.startsWith(LOCAL_IMAGE_HREF_PREFIX), href).toBe(false);
    }
  });
});

describe('parseLocalImageHref', () => {
  it('round-trips the guarded local-image href', () => {
    expect(
      parseLocalImageHref(
        `${LOCAL_IMAGE_HREF_PREFIX}path=%2Fworkspace%2Fdiagram.png&workspace=%2Fworkspace&source=file%3A%2F%2F%2Fworkspace%2Fdiagram.png`,
      ),
    ).toEqual({
      path: '/workspace/diagram.png',
      workspacePath: '/workspace',
      sourceHref: 'file:///workspace/diagram.png',
    });
  });

  it('rejects a missing nonce or path', () => {
    expect(parseLocalImageHref('agent-overflow:image?path=/etc/passwd')).toBeNull();
    expect(parseLocalImageHref(LOCAL_IMAGE_HREF_PREFIX)).toBeNull();
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

describe('path-link start() performance contract', () => {
  // Why this suite exists: marked calls `start` on EVERY inline tokenizer
  // loop iteration, handing it a fresh slice of the remaining source. The
  // original implementation answered by running `src.indexOf(path)` once
  // per allowlisted path, so every path that did not occur cost a full
  // scan of the slice before the answer was known — O(tokens × paths ×
  // |src|) per parse, re-paid on every reveal tick of a streaming tail.
  //
  // The contract is therefore about SHAPE, not milliseconds: parse cost
  // must not scale with how many paths the server validated. The bound is
  // deliberately RELATIVE only — the same document, in the same process,
  // with an allowlist of 1 vs 64 — so it holds on any machine and under
  // any CI load. An absolute millisecond cap was tried and removed: it
  // fails on a loaded runner for reasons that have nothing to do with the
  // property under test, and a wall-clock regression that slows both sides
  // together is not what this suite is for. Reference numbers on the
  // profiling machine, same document both times: pre-fix 108ms at one path
  // and 1098ms at 64 (10.2×, and it keeps climbing with the allowlist);
  // post-fix 90ms and 91ms (1.0×). Nearly all of that 90ms is marked's own
  // inline pass over a single 180KB paragraph — the extension has stopped
  // being a term in the cost.
  const perfPaths = Array.from({ length: 64 }, (_, i) => `src/lib/module${i}/handler.ts`);

  // One long paragraph: marked's inline pass gets the whole thing as a
  // single `src`, and every emphasis / code span / bracket forces another
  // loop iteration (another `start` call over the still-huge remainder).
  // Recurrences are deliberately sparse and clustered so the scan has both
  // "answer is near" and "answer is far" regions to cross.
  const perfDoc = Array.from({ length: 900 }, (_, i) => {
    const filler =
      `pass ${i} keeps a *steady* cadence while the \`resolver\` holds ` +
      `**its** viewport and the drain settles, ` +
      `and the follow-up sentence adds enough prose to make the slice long. `;
    return i % 9 === 0 ? `${filler}see src/lib/module${i % 64}/handler.ts here. ` : filler;
  }).join('');

  const median = (samples: number[]): number => {
    const sorted = [...samples].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  };

  const timeParse = (paths: readonly string[], runs: number): number => {
    const ext = buildPathLinkExtension(
      paths.map((path) => ({ path })),
      '/workspace',
    );
    const samples: number[] = [];
    for (let i = 0; i < runs; i += 1) {
      const t0 = performance.now();
      lex(perfDoc, ext);
      samples.push(performance.now() - t0);
    }
    return median(samples);
  };

  it('parse cost does not scale with the size of the path allowlist', () => {
    // Warm the JIT on both shapes before either is measured.
    timeParse(perfPaths.slice(0, 1), 1);
    timeParse(perfPaths, 1);

    const one = timeParse(perfPaths.slice(0, 1), 3);
    const many = timeParse(perfPaths, 3);

    expect(
      many,
      `1 path=${one.toFixed(2)}ms 64 paths=${many.toFixed(2)}ms (${(many / one).toFixed(1)}x)`,
    ).toBeLessThan(one * 4);
  });

  it('still linkifies every recurrence in the pathological document', () => {
    const ext = buildPathLinkExtension(
      perfPaths.map((path) => ({ path })),
      '/workspace',
    );
    const links = findLinks(lex(perfDoc, ext));
    expect(links).toHaveLength(100);
    expect(new Set(links.map((l) => l.raw)).size).toBe(64);
  });
});
