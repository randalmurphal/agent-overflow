import { afterEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import { CHAT_MARKDOWN_PRESENCE_CONTEXT } from './markdownSettledContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { setViewOnlySessionFromBootstrap } from '../../transport/runMode';

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
  afterEach(() => {
    setViewOnlySessionFromBootstrap(false);
  });

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
      expect(anchor?.getAttribute('title')).toBe('Open src/foo.ts:42 in editor');
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

  it('emits no local path affordance in a view-only session', async () => {
    setViewOnlySessionFromBootstrap(true);
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'See src/foo.ts here',
        workspacePath: '/repo',
        pathRefs: [{ path: 'src/foo.ts' }],
      },
    });

    await new Promise((resolve) => setTimeout(resolve, 16));
    expect(container.querySelector('a[href^="agent-overflow:open"]')).toBeNull();
    expect(container.textContent).toContain('src/foo.ts');
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

  it('renders backslash-escaped Markdown punctuation as literal text', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: String.raw`\*literal asterisks\*, \_underscores\_, and \# not a heading`,
      },
    });

    await waitFor(() => {
      expect(container.textContent).toContain(
        '*literal asterisks*, _underscores_, and # not a heading',
      );
    });
    expect(container.querySelector('em')).toBeNull();
    expect(container.querySelector('h1')).toBeNull();
  });

  it('keeps explicit Markdown titles on allowed links', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[Documentation](https://example.com "Read the documentation")',
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href="https://example.com/"]');
      expect(anchor?.getAttribute('title')).toBe('Read the documentation');
    });
  });

  it('renders a repo-relative link as plain text without a [blocked] tag', async () => {
    // PR/issue bodies routinely carry repo-relative links like
    // `[docs/guide.md](docs/guide.md)` — not navigable from the app,
    // but not *blocked URLs* either. The Link.svelte divergence in the
    // vendored svelte-streamdown (`DIVERGENCE.md` entry 10) drops the
    // " [blocked]"
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

  it('rewrites an absolute-path markdown link into an editor path link', async () => {
    // `[~/.claude/x.md](/home/user/.claude/x.md)` — an agent linking a
    // file outside the repo. The path-link extension rewrites the
    // path-shaped href to the nonce'd editor scheme; the click
    // delegate forwards it to OpenInEditor, and the backend gates the
    // open (existing regular file when outside the workspace).
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[~/.claude/x.md](/home/user/.claude/x.md)',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      expect(anchor?.getAttribute('href')).toContain('path=%2Fhome%2Fuser%2F.claude%2Fx.md');
      expect(anchor?.getAttribute('title')).toBe('Open /home/user/.claude/x.md in editor');
    });
    expect(container.querySelector('a[href="/home/user/.claude/x.md"]')).toBeNull();
  });

  it('rewrites a local file URI without rendering a blocked marker', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[Entity definition](file:///repo/models/event.py#L42)',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      const anchor = container.querySelector('a[href^="agent-overflow:open"]');
      expect(anchor).not.toBeNull();
      expect(anchor?.getAttribute('href')).toContain('path=%2Frepo%2Fmodels%2Fevent.py');
      expect(anchor?.getAttribute('href')).toContain('line=42');
      expect(anchor?.getAttribute('title')).toBe('Open /repo/models/event.py:42 in editor');
    });
    expect(container.textContent).not.toContain('[blocked]');
    expect(container.querySelector('[data-streamdown-link-blocked]')).toBeNull();
  });

  it('loads a local file image through the guarded backend image path', async () => {
    setBindingMock('GetLocalImageData', async () => ({
      data: 'iVBORw0KGgo=',
      mimeType: 'image/png',
    }));
    const { container } = render(ChatMarkdown, {
      props: {
        source: '![diagram](file:///repo/docs/diagram.png)',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      expect(container.querySelector('img[alt="diagram"]')).not.toBeNull();
    });
    expect(container.querySelector('[data-streamdown-image-blocked]')).toBeNull();
  });

  it('never rewrites an absolute href into an editor link on a workspace-less surface', async () => {
    // PR bodies and review comments render with pathRefs=[] and NO
    // workspacePath, and third parties author them: GitHub
    // root-relative links (`/owner/repo/...`) are routine there. An
    // editor affordance for those would hand a stat-free
    // ResolvePath pass-through to anyone who can comment on a PR —
    // href rewriting requires a workspace for EVERY shape.
    const { container } = render(ChatMarkdown, {
      props: {
        source: '[the fix](/owner/repo/blob/main/x.md) and [home](~/.ssh/id_ed25519)',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      expect(container.textContent).toContain('the fix');
    });

    expect(container.querySelector('a[href^="agent-overflow:open"]')).toBeNull();
    expect(container.querySelector('a[href="/owner/repo/blob/main/x.md"]')).toBeNull();
  });

  it('never renders a raw same-origin img for a /-leading image src', async () => {
    // Image.svelte carried the same isPathRelativeUrl bypass Link.svelte
    // lost (DIVERGENCE.md entry 17 follow-up): `![x](/api/whatever)`
    // rendered a raw <img> issuing a model-authored same-origin GET.
    const { container } = render(ChatMarkdown, {
      props: {
        source: '![beacon](/api/whatever) and ![off](//evil.example/x.png)',
        workspacePath: '/repo',
        pathRefs: [],
      },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-image-blocked]')).not.toBeNull();
    });

    expect(container.querySelector('img')).toBeNull();
  });

  it('never renders a raw same-origin anchor for a /-leading href (no extension installed)', async () => {
    // pathRefs: undefined → no marked extension at all. Upstream
    // streamdown would render `[x](/home/user/x.md)` as a RAW anchor
    // (its isPathRelativeUrl branch bypasses transformUrl) — a same-tab
    // top-level navigation onto the SPA origin: a 404 at best, an
    // origin-isolation escape at worst. The vendored Link.svelte drops
    // that branch (DIVERGENCE.md entry 17); the href renders as a
    // non-navigable schemeless reference instead.
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'see [my notes](/home/user/x.md) here',
      },
    });

    await waitFor(() => {
      const span = container.querySelector('[data-streamdown-link-blocked]');
      expect(span).not.toBeNull();
      expect(span?.textContent).toContain('my notes');
      expect(span?.getAttribute('title')).toBe('/home/user/x.md');
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
    expect(codeBlocks[0].querySelector('code')?.textContent).toContain('func main()');
    expect(container.textContent).toContain('More text.');
  });

  // Token-level scope for the marker-line-alignment patch hunk lives in
  // `markdown/listMarkerCode.test.ts`; this pair closes the loop from
  // tokens to DOM — the reported artifact is a code card per bullet, and
  // the deliberate indented block one blank line down must still get one.
  it('renders a space-aligned bullet value as list text, not a code block', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: '- Starter\n-     $499 per month', pathRefs: [] },
    });

    await waitFor(() => {
      expect(container.querySelectorAll('li').length).toBe(2);
    });

    expect(container.querySelector('[data-code-source]')).toBeNull();
    expect(container.querySelectorAll('li')[1]?.textContent).toContain('$499 per month');
  });

  it('still renders an indented code block that starts below the marker line', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: '- item one\n\n        deep indent', pathRefs: [] },
    });

    await waitFor(() => {
      const codeBlock = container.querySelector('[data-code-source]');
      expect(codeBlock).not.toBeNull();
      expect(codeBlock?.querySelector('code')?.textContent).toContain('deep indent');
    });
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

describe('<ChatMarkdown> warm-gate presence registration', () => {
  // MessageTimeline's quietContextSignal reports settled-by-absence when
  // ZERO ChatMarkdowns are mounted (nothing to typeset → no reason to
  // hold the warm gate to the 2.5s failsafe). That only works if every
  // ChatMarkdown registers on mount and unregisters on unmount — a leak
  // in either direction poisons the count for the rest of the pane's
  // life.
  it('registers on mount and unregisters on unmount via CHAT_MARKDOWN_PRESENCE_CONTEXT', async () => {
    let mounted = 0;
    const register = (): (() => void) => {
      mounted += 1;
      return () => {
        mounted -= 1;
      };
    };

    const { unmount } = render(ChatMarkdown, {
      props: { source: 'plain **markdown**' },
      context: new Map([[CHAT_MARKDOWN_PRESENCE_CONTEXT, register]]),
    });

    await waitFor(() => expect(mounted).toBe(1));
    unmount();
    expect(mounted).toBe(0);
  });

  it('skips registration outside a timeline (no context)', () => {
    // Settings preview / design canvas ChatMarkdowns get no presence
    // context and must render without touching the warm gate.
    const { container } = render(ChatMarkdown, {
      props: { source: 'standalone' },
    });
    expect(container.textContent).toContain('standalone');
  });
});

describe('<ChatMarkdown> library chrome removed in the W1 sweep', () => {
  // The vendored renderer used to ship four interactive surfaces this app
  // never wanted: a footnote popover, a table download menu, a citation
  // carousel, and mermaid's inline panzoom toolbar. All four are gone.
  // These assertions pin what SURVIVED, so a future edit that revives the
  // chrome (or drops the surviving markup with it) fails here.

  it('renders a footnote reference chip and no popover', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'A claim[^note].\n\n[^note]: The supporting body.' },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-footnote-ref]')).not.toBeNull();
    });

    const chip = container.querySelector('[data-streamdown-footnote-ref]')!;
    expect(chip.textContent).toBe('note');
    // No dialog, and nothing that claims to control one.
    expect(container.querySelector('dialog')).toBeNull();
    expect(container.querySelector('[data-streamdown-footnote-popover]')).toBeNull();
    expect(chip.getAttribute('aria-haspopup')).toBeNull();
    expect(chip.getAttribute('aria-controls')).toBeNull();
  });

  it('renders a table with no download control', async () => {
    const { container } = render(ChatMarkdown, {
      props: {
        source: '| Left | Right |\n| :--- | ----: |\n| alpha | beta |',
      },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-streamdown-table]')).not.toBeNull();
    });

    expect(container.querySelectorAll('td')).toHaveLength(2);
    expect(container.textContent).toContain('alpha');
    // The download menu was the only control the table ever rendered.
    expect(container.querySelector('[data-streamdown-table] button')).toBeNull();
    expect(container.querySelector('button[aria-label="Download"]')).toBeNull();
  });

  it('renders citation-shaped prose through the host snippet', async () => {
    const { container } = render(ChatMarkdown, {
      props: { source: 'The result holds [1] under load.' },
    });

    await waitFor(() => {
      expect(container.textContent).toContain('under load');
    });

    // The `citations` tokenizer stays (it changes how `[foo]` parses), but
    // ChatMarkdown's `inlineCitation` snippet renders the literal text and
    // the carousel/list component is gone.
    expect(container.textContent).toContain('The result holds [1] under load.');
    expect(container.querySelector('[aria-label^="Citation"]')).toBeNull();
    expect(container.querySelector('dialog')).toBeNull();
  });
});
