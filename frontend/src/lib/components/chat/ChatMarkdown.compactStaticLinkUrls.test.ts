import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { PATH_LINK_HREF_PREFIX } from '../../utils/pathLinkExtension';
import { buildPreviewLinkExtension } from '../../utils/previewLinkExtension';
import type {
  PreviewAvailability,
  PreviewLinkTarget,
} from '../../stores/devServers.svelte';

// One href corpus, both render paths, per-case naming.
//
// A link href is classified once — `transformUrl` (a bare `new URL()` with
// no base, so every relative form throws) plus the schemeless predicate that
// decides whether a rejected href keeps the " [blocked]" tag — and that one
// decision is then realized TWICE: by `render/elements/Link.svelte` on the
// component path, and by `render/staticHtml.ts` on the compact fixed-tag path
// that owns every completed block. markdown/AGENTS.md § Landmines names that
// pair as a silent output fork: the two are separate renderers of one token
// stream, and the mermaid fence already shipped as a case one path routed
// differently from the other.
//
// `CompactStaticMarkdownHarness` mounts the same content through both
// (`compactStaticHtml` false → Block components → Link.svelte; true →
// CompactBlocks → staticHtml.ts), so each case asserts two things at once:
// the render decision this href class is supposed to produce, and that both
// paths produce it. An edit to one path that forgets the other fails the case
// that names the diverging href class.
//
// The one class this file cannot compare is the `streamdown:incomplete-link`
// sentinel, the half-typed link the completer mints. Both paths carry a branch
// rendering it as an anchor with no href, but the compact path is gated on
// `parseIncompleteMarkdown === false` and the sentinel exists only when that
// flag is true, so the static branch is structurally unreachable. It is
// asserted on the component path in `ChatMarkdown.test.ts`.

interface LinkRender {
  /** `a[data-streamdown-link]` vs `span[data-streamdown-link-blocked]`. */
  element: 'anchor' | 'blocked span';
  href: string | null;
  target: string | null;
  rel: string | null;
  title: string | null;
  /** Carries the " [blocked]" tag when the href was rejected as absolute. */
  text: string;
}

const LABEL = 'label';

function linkRenderOf(root: Element): LinkRender {
  const anchor = root.querySelector('a[data-streamdown-link]');
  const blocked = root.querySelector('span[data-streamdown-link-blocked]');
  if (anchor && blocked) throw new Error('one link rendered as both forms');
  const element = anchor ?? blocked;
  if (!element) throw new Error('no link element rendered');
  return {
    element: anchor ? 'anchor' : 'blocked span',
    href: element.getAttribute('href'),
    target: element.getAttribute('target'),
    rel: element.getAttribute('rel'),
    title: element.getAttribute('title'),
    text: element.textContent ?? '',
  };
}

function anchor(href: string, title: string | null = null): LinkRender {
  return {
    element: 'anchor',
    href,
    target: '_blank',
    rel: 'noopener noreferrer',
    title,
    text: LABEL,
  };
}

/**
 * A rejected absolute href: not navigable, and tagged so the reader can tell
 * the renderer withheld a URL rather than the author writing plain text.
 */
function blockedTagged(href: string): LinkRender {
  return {
    element: 'blocked span',
    href: null,
    target: null,
    rel: null,
    title: `Blocked URL: ${href}`,
    text: `${LABEL} [blocked]`,
  };
}

/**
 * A schemeless reference (`docs/x.md`, `/x`, `#frag`, `../x`). Not navigable
 * from here either, but it is ordinary repo-relative prose rather than a
 * withheld URL, so it renders untagged with the href as its hover title.
 */
function blockedUntagged(href: string): LinkRender {
  return {
    element: 'blocked span',
    href: null,
    target: null,
    rel: null,
    title: href,
    text: LABEL,
  };
}

const PATH_LINK_HREF = `${PATH_LINK_HREF_PREFIX}path=%2Frepo%2Fx.md`;

// [case name, markdown destination, expected render decision]
const CORPUS: Array<[string, string, LinkRender]> = [
  ['plain https', 'https://example.test/path', anchor('https://example.test/path')],
  ['plain http', 'http://example.test/path', anchor('http://example.test/path')],
  [
    'https with a query, ampersand and fragment',
    'https://example.test/s?a=1&b=2#f',
    anchor('https://example.test/s?a=1&b=2#f'),
  ],
  [
    'uppercase scheme and host, normalized by the URL parser',
    'HTTPS://Example.TEST/Path',
    anchor('https://example.test/Path'),
  ],
  [
    'https with an explicit Markdown title',
    'https://example.test/x "Read the docs"',
    anchor('https://example.test/x', 'Read the docs'),
  ],
  [
    'custom scheme on the allowlist, matched by prefix rather than the wildcard',
    PATH_LINK_HREF,
    anchor(PATH_LINK_HREF),
  ],
  ['root-relative', '/x', blockedUntagged('/x')],
  [
    'root-relative filesystem path',
    '/home/user/notes.md',
    blockedUntagged('/home/user/notes.md'),
  ],
  ['schemeless path-shaped', 'docs/readme.md', blockedUntagged('docs/readme.md')],
  ['schemeless parent-relative', '../up.md', blockedUntagged('../up.md')],
  ['fragment only', '#frag', blockedUntagged('#frag')],
  ['empty href', '', blockedUntagged('')],
  // `//host/x` names a real host, so it is excluded from the schemeless class
  // in both paths and keeps the tag: rendered as a live anchor it would be a
  // top-level cross-origin navigation off the app origin.
  ['protocol-relative', '//host.test/x', blockedTagged('//host.test/x')],
  [
    'non-http scheme (script URL)',
    'javascript:void(0)',
    blockedTagged('javascript:void(0)'),
  ],
  ['non-http scheme (data)', 'data:text/plain,hi', blockedTagged('data:text/plain,hi')],
  ['non-http scheme (mailto)', 'mailto:x@y.test', blockedTagged('mailto:x@y.test')],
  [
    'non-http scheme (vbscript)',
    'vbscript:MsgBox',
    blockedTagged('vbscript:MsgBox'),
  ],
  // The `<…>` destination form is the only one that survives the lexer with
  // its padding intact, and the URL parser strips leading/trailing spaces and
  // interior tabs — so these three padded forms are what the classifier is
  // actually handed. The last one is the interesting one: a leading space puts
  // it outside BOTH the scheme regex and the `//` check, so a padded
  // protocol-relative href classifies as schemeless and renders untagged.
  [
    'space-padded https',
    '<  https://example.test/x  >',
    anchor('https://example.test/x'),
  ],
  ['tab-prefixed https', '<\thttps://example.test/x>', anchor('https://example.test/x')],
  [
    'space-prefixed protocol-relative',
    '< //host.test/x>',
    blockedUntagged(' //host.test/x'),
  ],
];

describe('link URL classification across both render paths', () => {
  it.each(CORPUS)('%s', async (_name, destination, expected) => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: `[${LABEL}](${destination})`,
      allowedLinkPrefixes: ['*', PATH_LINK_HREF_PREFIX],
    });

    const componentPath = container.querySelector('[data-full-token-tree] > div');
    const staticPath = container.querySelector('[data-compact-static] > div');
    if (!componentPath || !staticPath) {
      throw new Error('differential Streamdown roots did not mount');
    }

    await waitFor(() => {
      expect(componentPath.textContent).toContain(LABEL);
      expect(staticPath.textContent).toContain(LABEL);
    });

    expect(linkRenderOf(componentPath)).toEqual(expected);
    expect(linkRenderOf(staticPath)).toEqual(expected);
  });
});

// The preview rewrite is the same fork in the same place: it replaces one
// link token's rendering with an anchor plus data attributes plus, in one
// state, a sibling control. Both renderers spell it from
// `markdown/render/previewLink.ts`, and these cases are what holds them to it.

function previewTarget(
  kind: PreviewAvailability['kind'],
  canAllow: boolean,
): PreviewLinkTarget {
  return {
    threadId: 'thread-1',
    backend: 'backend-a',
    machine: 'laptop',
    canAllow,
    key: kind + String(canAllow),
    resolve: () => ({ kind }) as PreviewAvailability,
  };
}

interface PreviewRender {
  href: string | null;
  title: string | null;
  state: string | null;
  port: string | null;
  path: string | null;
  thread: string | null;
  machine: string | null;
  via: string | null;
  classed: boolean;
  allow: string | null;
  allowBackend: string | null;
}

function previewRenderOf(root: Element): PreviewRender {
  const link = root.querySelector('a[data-preview-port]');
  if (!link) throw new Error('no preview anchor rendered');
  const allow = root.querySelector('button[data-preview-allow]');
  return {
    href: link.getAttribute('href'),
    title: link.getAttribute('title'),
    state: link.getAttribute('data-preview-state'),
    port: link.getAttribute('data-preview-port'),
    path: link.getAttribute('data-preview-path'),
    thread: link.getAttribute('data-preview-thread'),
    machine: link.getAttribute('data-preview-machine'),
    via: link.getAttribute('data-preview-via'),
    classed: link.classList.contains('preview-link'),
    allow: allow?.getAttribute('data-preview-allow') ?? null,
    allowBackend: allow?.getAttribute('data-preview-backend') ?? null,
  };
}

const PREVIEW_SOURCE_URL = 'http://localhost:5173/app?a=1#top';

function previewRender(
  state: string,
  title: string,
  allow: string | null,
): PreviewRender {
  return {
    href: PREVIEW_SOURCE_URL,
    title,
    state,
    port: '5173',
    path: '/app?a=1#top',
    thread: 'thread-1',
    machine: 'laptop',
    via: 'via laptop',
    classed: true,
    allow,
    allowBackend: allow === null ? null : 'backend-a',
  };
}

const PREVIEW_CORPUS: Array<
  [string, PreviewAvailability['kind'], boolean, PreviewRender]
> = [
  [
    'a shared port opens, and the link says where',
    'open',
    false,
    previewRender('open', 'Opens on laptop.', null),
  ],
  [
    'an unshared port offers to be shared when this session may',
    'not-shared',
    true,
    previewRender('not-shared', 'Port 5173 is not shared from laptop.', '5173'),
  ],
  [
    'an unshared port offers nothing when it may not',
    'not-shared',
    false,
    previewRender('not-shared', 'Port 5173 is not shared from laptop.', null),
  ],
  [
    'a machine with no address never offers to share, because it would change nothing',
    'no-address',
    true,
    previewRender('no-address', 'laptop has no address this page can reach it on.', null),
  ],
];

describe('preview link rendering across both render paths', () => {
  it.each(PREVIEW_CORPUS)('%s', async (_name, kind, canAllow, expected) => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: '[' + LABEL + '](' + PREVIEW_SOURCE_URL + ')',
      allowedLinkPrefixes: ['*', PATH_LINK_HREF_PREFIX],
      extensions: [buildPreviewLinkExtension(previewTarget(kind, canAllow))],
    });

    const componentPath = container.querySelector('[data-full-token-tree] > div');
    const staticPath = container.querySelector('[data-compact-static] > div');
    if (!componentPath || !staticPath) {
      throw new Error('differential Streamdown roots did not mount');
    }

    await waitFor(() => {
      expect(componentPath.textContent).toContain(LABEL);
      expect(staticPath.textContent).toContain(LABEL);
    });

    expect(previewRenderOf(componentPath)).toEqual(expected);
    expect(previewRenderOf(staticPath)).toEqual(expected);
  });

  it('leaves an ordinary link on the plain path with the rewrite armed', async () => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: '[' + LABEL + '](https://example.test/x)',
      allowedLinkPrefixes: ['*', PATH_LINK_HREF_PREFIX],
      extensions: [buildPreviewLinkExtension(previewTarget('open', true))],
    });

    const staticPath = container.querySelector('[data-compact-static] > div');
    if (!staticPath) throw new Error('static root did not mount');
    await waitFor(() => expect(staticPath.textContent).toContain(LABEL));
    expect(staticPath.querySelector('[data-preview-port]')).toBeNull();
    expect(linkRenderOf(staticPath)).toEqual(anchor('https://example.test/x'));
  });
});
