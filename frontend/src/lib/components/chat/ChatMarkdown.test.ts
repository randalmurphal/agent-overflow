import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';

// Integration coverage for the path-link primitive flowing through
// Streamdown's URL gate. The unit suite in `pathLinkExtension.test.ts`
// proves the marked extension emits `link` tokens with our scheme; the
// click delegate / serializer tests prove downstream consumers handle
// those hrefs. What's missing without this file is the proof that
// Streamdown's `transformUrl` actually keeps the custom scheme — the
// library's `allowedLinkPrefixes={['*']}` wildcard only lets
// http/https through, so a regression that drops
// `PATH_LINK_HREF_SCHEME` from the prefix array would silently rewrite
// the href to `about:blank#blocked` without any unit test catching it.

describe('<ChatMarkdown> path-link rendering', () => {
  it('renders an agent-overflow:open anchor when the path is on the allowlist', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'See src/foo.ts:42 here',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      const href = anchor?.getAttribute('href') ?? '';
      expect(href).toContain('path=src%2Ffoo.ts');
      expect(href).toContain('line=42');
      expect(href).toContain('workspace=%2Frepo');
    });
  });

  it('emits no anchor when the allowlist excludes the path', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'See src/foo.ts here',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    // Give Streamdown async typesetting a tick; we want to be sure no
    // anchor materializes after the fact.
    await new Promise((resolve) => setTimeout(resolve, 16));

    const anchor = container.querySelector('a[href^="agent-overflow:open"]');
    expect(anchor).toBeNull();
  });

  it('drops the `@` mention prefix into the rendered anchor text', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'review @src/foo.ts now',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      expect(anchor?.textContent).toBe('@src/foo.ts');
    });
  });

  // Backtick-wrapped allowlisted paths render as `<a><code>` — the
  // dominant convention for paths in technical writing. The extension
  // emits a `link` token whose child is `codespan`; Streamdown's Block
  // renders the link's children recursively, dispatching the codespan
  // child to Element.svelte's `<code>` branch. End-to-end DOM
  // assertion catches the case the unit test cannot: if Streamdown's
  // Link component ever stopped rendering child tokens (or rendered
  // only `text`), the anchor would lose its `<code>` and the styled
  // pill would disappear silently.
  it('linkifies an allowlisted path inside an inline code span as `<a><code>`', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'see `src/foo.ts` for details',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      const code = anchor?.querySelector('code[data-streamdown-codespan]');
      expect(code).not.toBeNull();
      expect(code?.textContent).toBe('src/foo.ts');
      const href = anchor?.getAttribute('href') ?? '';
      expect(href).toContain('path=src%2Ffoo.ts');
    });
  });

  it('does not linkify a non-allowlisted path inside an inline code span', async () => {
    // The wrapped branch must defer to marked's built-in codespan
    // tokenizer when the path is not in the allowlist; server-side
    // validation stays authoritative.
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'see `src/other/bar.ts` for details',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const code = container.querySelector('code[data-streamdown-codespan]');
      expect(code?.textContent).toBe('src/other/bar.ts');
    });

    const anchor = container.querySelector('a[href^="agent-overflow:open"]');
    expect(anchor).toBeNull();
  });

  it('routes an allowlisted markdown file link through the editor href with line number', async () => {
    const path = '/repo/src/external_jwt.py';
    const { container } = render(ChatMarkdown, {
      props: {
        source: `[external_jwt.py](${path}:636)`,
        workspacePath: '/repo',
        pathRefs: [{ path }],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      expect(anchor?.textContent).toBe('external_jwt.py');
      const href = anchor?.getAttribute('href') ?? '';
      expect(href).toContain(`path=${encodeURIComponent(path)}`);
      expect(href).toContain('line=636');
      expect(href).toContain('workspace=%2Frepo');
    });
  });

  it('renders one editor anchor when a markdown link label is also an allowlisted path', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[src/foo.ts](src/foo.ts:42)',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const anchors = container.querySelectorAll('a[href^="agent-overflow:open"]');
      expect(anchors).toHaveLength(1);
      const href = anchors[0].getAttribute('href') ?? '';
      expect(href).toContain('path=src%2Ffoo.ts');
      expect(href).toContain('line=42');
    });
  });

  it('rejects a raw markdown link with the path-link scheme but no nonce', async () => {
    // The agent's prose flows verbatim through ChatMarkdown. A
    // hostile agent could try to short-circuit the validator by
    // emitting a raw markdown link with our custom scheme:
    // `[click](agent-overflow:open?path=/etc/passwd)`. Streamdown's
    // `transformUrl` only accepts a custom-scheme URL whose canonical
    // form `startsWith(allowedPrefix.href)`; because the prefix bakes
    // in a per-page-load nonce that's not observable to the agent,
    // this raw link is filtered out before any anchor is rendered.
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[click here](agent-overflow:open?path=/etc/passwd)',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    // Give Streamdown a tick to settle in case it renders the link
    // first and rewrites the href on a later pass.
    await new Promise((resolve) => setTimeout(resolve, 16));

    const anchor = container.querySelector('a[href*="/etc/passwd"]');
    expect(anchor).toBeNull();
    const anyOpenAnchor = container.querySelector('a[href^="agent-overflow:open"]');
    expect(anyOpenAnchor).toBeNull();
  });

  it('renders email-shaped prose as text without Streamdown blocked markers', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '(composer@0.7s unchanged) <test@example.com> https://example.com',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      expect(container.textContent).toContain('(composer@0.7s unchanged)');
      expect(container.textContent).toContain('<test@example.com>');
    });

    expect(container.textContent).not.toContain('[blocked]');
    expect(container.querySelector('[data-streamdown-link-blocked]')).toBeNull();
    expect(container.querySelector('a[href^="mailto:"]')).toBeNull();

    const webLink = container.querySelector('a[href="https://example.com/"]');
    expect(webLink?.textContent).toBe('https://example.com');
  });

  it('renders a repo-relative link as plain text without a [blocked] tag', async () => {
    // PR/issue bodies routinely carry repo-relative links like
    // `[docs/guide.md](docs/guide.md)` — not navigable from the app,
    // but not *blocked URLs* either. The Link.svelte hunk in
    // `patches/svelte-streamdown@3.1.2.patch` drops the " [blocked]"
    // suffix for schemeless relative references (the href survives as
    // the hover title); disallowed absolute URLs keep the tag.
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'All phases of [docs/guide.md](docs/guide.md) done.',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      const span = container.querySelector('[data-streamdown-link-blocked]');
      expect(span).not.toBeNull();
      expect(span?.textContent).toContain('docs/guide.md');
      expect(span?.getAttribute('title')).toBe('docs/guide.md');
    });

    expect(container.textContent).not.toContain('[blocked]');
    expect(container.querySelector('a')).toBeNull();
  });

  it('keeps the [blocked] tag on a disallowed absolute URL', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[click me](vbscript:evil())',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      const span = container.querySelector('[data-streamdown-link-blocked]');
      expect(span).not.toBeNull();
      expect(span?.textContent).toContain('[blocked]');
    });

    expect(container.querySelector('a')).toBeNull();
  });

  it('unwraps a ```markdown fence so inner code blocks render correctly', async () => {
    const source = [
      '```markdown',
      '',
      '## Heading',
      '',
      'Some prose.',
      '',
      '```go',
      'func main() {}',
      '```',
      '',
      'More text.',
      '',
      '```',
    ].join('\n');

    const { container } = render(ChatMarkdown, {
      props: { source, pathRefs: [] },
    });

    await waitFor(() => {
      const heading = container.querySelector('h2');
      expect(heading).not.toBeNull();
      expect(heading?.textContent).toContain('Heading');
    });

    const codeBlocks = container.querySelectorAll('[data-code-source]');
    expect(codeBlocks.length).toBe(1);
    expect(codeBlocks[0].getAttribute('data-code-source')).toContain('func main()');
    expect(container.textContent).toContain('More text.');
  });

  it('does not encode the workspace param when workspacePath is empty', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'see src/foo.ts here',
        workspacePath: '',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      const href = anchor?.getAttribute('href') ?? '';
      expect(href).toContain('path=src%2Ffoo.ts');
      expect(href).not.toContain('workspace=');
    });
  });
});
