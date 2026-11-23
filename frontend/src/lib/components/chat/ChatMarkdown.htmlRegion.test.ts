import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __reportImageBoxForTest, __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// An HTML wrapper around Markdown, as GitHub and GitLab render it: the
// forge renders the whole comment to one HTML string and parses it once,
// so the Markdown lands inside the wrapper. The expected structures are the
// forges' `/markdown` output for each source. Forge surfaces render settled
// bodies only; the streaming splitter does not keep a multi-block HTML
// construct (a region or a <details> run) in one block.

vi.mock('../../transport/deviceSession', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../transport/deviceSession')>()),
  fetchPairedComputer: async () => new Response('bytes', { status: 200 }),
}));
vi.mock('../../transport/backends', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../transport/backends')>()),
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));

const CASES: [string, string, string][] = [
  [
    'a CodeRabbit analysis chain',
    '<details>\n<summary>🧩 Analysis chain</summary><blockquote>\n\nSome **markdown** here.\n\n<details>\n<summary>Nested</summary>\n\ninner text\n\n</details>\n\n</blockquote></details>',
    '<details><summary>🧩 Analysis chain</summary><blockquote><p>Some <strong>markdown</strong> here.</p><details><summary>Nested</summary><p>inner text</p></details></blockquote></details>',
  ],
  [
    'Markdown in a table cell',
    '<table>\n<tr><th>A</th><th>B</th></tr>\n<tr><td>\n\n**md in cell**\n\n</td><td><code>x</code></td></tr>\n</table>',
    '<table><tbody><tr><th>A</th><th>B</th></tr><tr><td><p><strong>md in cell</strong></p></td><td><code>x</code></td></tr></tbody></table>',
  ],
  [
    'a centered div',
    '<div align="center">\n\n**markdown** inside div\n\n</div>',
    '<div align="center"><p><strong>markdown</strong> inside div</p></div>',
  ],
];

// The same sources in agent chat: no HTML, the Markdown as written.
const CHAT: Record<string, string> = {
  'a CodeRabbit analysis chain': '<p>Some <strong>markdown</strong> here.</p><p>inner text</p>',
  'Markdown in a table cell': '<p><strong>md in cell</strong></p>',
  'a centered div': '<p><strong>markdown</strong> inside div</p>',
};

// The structure a reader sees: elements with the attributes a forge
// keeps, text with whitespace collapsed, and no renderer markup.
const KEPT_ATTRS = ['align', 'open', 'colspan', 'rowspan', 'href', 'src', 'alt'];
function structure(root: Node): string {
  let out = '';
  for (const node of root.childNodes) {
    if (node.nodeType === Node.TEXT_NODE) {
      const text = (node.textContent ?? '').replace(/\s+/g, ' ');
      if (text.trim() !== '') out += text;
      continue;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) continue;
    const element = node as Element;
    const tag = element.localName;
    // Renderer wrappers with no element of their own in the forge output.
    if ((tag === 'span' && element.classList.contains('contents')) || element.matches('.md-committed, .md-volatile')) {
      out += structure(element);
      continue;
    }
    const attrs = KEPT_ATTRS.filter((name) => element.hasAttribute(name)).map((name) => ` ${name}="${element.getAttribute(name)}"`).join('');
    out += `<${tag}${attrs}>${structure(element)}</${tag}>`;
  }
  return out;
}
const forgeStructure = (html: string) => structure(new DOMParser().parseFromString(html, 'text/html').body);
const shown = (container: HTMLElement) => structure(container.querySelector('.markdown-body')!);

afterEach(() => {
  __resetMediaBlobCacheForTest();
  __resetImageTiersForTest();
  resetBindingMocks();
  vi.restoreAllMocks();
});

describe('<ChatMarkdown embeddedHtml> html regions', () => {
  it.each(CASES)('renders %s as the forges do', async (_name, source, forge) => {
    const { container } = render(ChatMarkdown, { props: { source, embeddedHtml: true } });
    await waitFor(() => expect(shown(container)).toBe(forgeStructure(forge)));
  });

  it('keeps every allowlist guarantee inside a region', async () => {
    const source = [
      '<div onclick="window.__pwned = 1" data-markdown-island="0:0">',
      '<script>window.__pwned = 2</script><img src="x" onerror="window.__pwned = 3"><a href="javascript:alert(1)">bad link</a><blink>odd</blink>',
      '',
      '**island**',
      '',
      '</div>',
    ].join('\n');
    const { container } = render(ChatMarkdown, { props: { source, embeddedHtml: true } });
    await waitFor(() => expect(container.querySelector('.markdown-body div p strong')?.textContent).toBe('island'));
    const body = container.querySelector('.markdown-body')!;
    expect(body.querySelector('script, blink, [onclick], [onerror], [data-markdown-island]')).toBeNull();
    expect(body.querySelector('a')?.hasAttribute('href')).toBe(false);
    expect(body.querySelector('img[src]')).toBeNull();
    // A script goes with its source; an element off the allowlist leaves its text.
    expect(body.textContent).not.toContain('__pwned');
    expect(body.textContent).toContain('odd');
    expect(body.textContent).not.toContain('<blink>');
    expect((window as { __pwned?: number }).__pwned).toBeUndefined();
  });
});

describe('<ChatMarkdown> without embedded HTML', () => {
  it.each(CASES)('renders %s as before: no HTML, the Markdown as written', async (name, source) => {
    const { container } = render(ChatMarkdown, { props: { source } });
    await waitFor(() => expect(shown(container)).toBe(forgeStructure(CHAT[name])));
  });
});

describe('forge media inside an html region', () => {
  const GITHUB: ForgeAttachmentSource = {
    pr: { forge: 'github', host: 'github.com', namespace: 'octo', repo: 'widget', number: 7 },
    backend: 'gpu',
    webBase: 'https://github.com/octo/widget/pull/7',
  };
  const ASSET = 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666';

  beforeEach(() => {
    let n = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:forge-${++n}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  it('hydrates a claimed image beside an island', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => ({
      url: '/attachments/forge/tok1?ticket=t',
      mimeType: 'image/png',
      kind: 'image',
      sizeBytes: 5,
      filename: 'a.png',
    }));
    const source = `<div align="center">\n<img alt="a shot" src="${ASSET}">\n\n**under the shot**\n\n</div>`;
    const { container } = render(ForgeAttachmentChatHarness, { props: { source, forgeSource: GITHUB } });
    await waitFor(() => {
      __reportImageBoxForTest(800);
      expect(container.querySelector('div[align="center"] img[src^="blob:"]')).not.toBeNull();
    });
    expect(container.querySelector('div[align="center"] > p strong')?.textContent).toBe('under the shot');
    expect(container.querySelector(`img[src="${ASSET}"]`)).toBeNull();
    expect(rpc).toHaveBeenCalledTimes(1);
  });
});
