import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { __resetForgeEmojiForTest, loadForgeEmoji } from '../../utils/forgeEmoji.svelte';
import * as extensionModule from '../../utils/forgeEmojiExtension';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __reportImageBoxForTest, __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';
import { parseForgeAttachmentHref, type ForgeAttachmentSource } from '../../utils/forgeAttachments';

vi.mock('../../transport/deviceSession', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../transport/deviceSession')>()),
  fetchPairedComputer: async () => new Response('bytes', { status: 200 }),
}));
vi.mock('../../transport/backends', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../transport/backends')>()),
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));

// Text and links inside forge HTML get what the forge gives them in a
// paragraph (utils/forgeHtmlText.ts): references and mentions, emoji, a
// relative href resolved where the forge sends it, and same-document jumps.
// GitLab links a bare URL there too; GitHub does not.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const GH = 'https://github.com/acme/widget';
const GL = 'https://gitlab.com/group/widget';
const SHA = '0123456789abcdef0123456789abcdef01234567';
const HEX = '0123456789abcdef0123456789abcdef';

const TABLE = [
  '<table><tr>',
  `<td>:x: See #12 and @octo, acme/other#3 at ${SHA}</td>`,
  '<td><code>#13 :x:</code></td>',
  '<td><a href="https://example.invalid/">#14 :tada:</a></td>',
  '<td><kbd>#15 :tada:</kbd></td>',
  '<td>https://example.invalid/x a@b.example [l](https://example.invalid/) end</td>',
  '</tr></table>',
  '',
].join('\n');

interface Shown {
  text: string;
  href: string | null;
}

const anchors = (root: Element): Shown[] =>
  [...root.querySelectorAll('a')].map((a) => ({ text: a.textContent!, href: a.getAttribute('href') }));

async function mount(source: string, forgeSource: ForgeAttachmentSource | null, streaming: boolean, until = 'end'): Promise<HTMLElement> {
  const { container } = render(ForgeAttachmentChatHarness, { props: { source, forgeSource, streaming } });
  await waitFor(() => expect(container.textContent).toContain(until));
  return container;
}

beforeEach(async () => {
  __resetForgeEmojiForTest();
  await loadForgeEmoji();
});
afterEach(() => __resetForgeEmojiForTest());

describe('text in forge HTML', () => {
  it.each([false, true])('links references and converts emoji on a GitHub surface (streaming %s)', async (streaming) => {
    const container = await mount(TABLE, GITHUB, streaming);
    const cells = [...container.querySelectorAll('td')];
    expect(cells.map((cell) => cell.textContent)).toEqual([
      `❌ See #12 and @octo, acme/other#3 at 0123456`,
      '#13 :x:',
      '#14 🎉',
      '#15 🎉',
      'https://example.invalid/x a@b.example [l](https://example.invalid/) end',
    ]);
    expect(anchors(cells[0])).toEqual([
      { text: '#12', href: `${GH}/issues/12` },
      { text: '@octo', href: 'https://github.com/octo' },
      { text: 'acme/other#3', href: 'https://github.com/acme/other/issues/3' },
      { text: '0123456', href: `${GH}/commit/${SHA}` },
    ]);
    expect(cells[0].querySelector('a code')?.textContent).toBe('0123456');
    expect(anchors(cells[2])).toEqual([{ text: '#14 🎉', href: 'https://example.invalid/' }]);
    expect(cells[3].querySelector('a')).toBeNull();
    // GitHub links no bare URL or email in HTML, and Markdown is text there.
    expect(cells[4].querySelector('a')).toBeNull();
  });

  it.each([false, true])('links references, emoji and URLs by GitLab rules (streaming %s)', async (streaming) => {
    const source = [
      '<div>',
      'See !3, #4+ and @group/sub. https://gitlab.com/group/widget/-/issues/9 and smb://x/y',
      'a@b.example and [[Home]] :tada::tada: :x: end',
      '</div>',
      '',
    ].join('\n');
    const container = await mount(source, GITLAB, streaming);
    const div = container.querySelector('.markdown-body div div')!;
    expect(div.textContent).toContain(':tada::tada: ❌ end');
    expect(anchors(div)).toEqual([
      { text: '!3', href: `${GL}/-/merge_requests/3` },
      { text: '#4', href: `${GL}/-/issues/4` },
      { text: '@group/sub', href: 'https://gitlab.com/group/sub' },
      { text: '#9', href: `${GL}/-/issues/9` },
      // Admitted by the renderer's link rule, as in a paragraph.
      { text: 'smb://x/y', href: 'smb://x/y' },
      { text: 'Home', href: `${GL}/-/wikis/Home` },
    ]);
    expect(div.textContent).toContain('a@b.example');
  });

  it('reads a backslash in HTML text as itself', async () => {
    const container = await mount('<p>\\:tada: b <b>\\:x:</b> end</p>\n', GITHUB, false);
    expect(container.querySelector('.markdown-body p')!.textContent).toBe('\\🎉 b \\❌ end');
  });

  it('keeps a GitHub cell literal off a forge surface and in agent chat', async () => {
    const container = await mount(TABLE, null, false);
    expect(container.querySelector('td')!.textContent).toBe(`:x: See #12 and @octo, acme/other#3 at ${SHA}`);
    expect(container.querySelectorAll('td a')).toHaveLength(1);
    const chat = render(ChatMarkdown, { props: { source: '<p>See #12 :x: <a href="docs/a.md">doc</a></p>\n' } }).container;
    await waitFor(() => expect(chat.querySelector('.markdown-body')).not.toBeNull());
    expect(chat.querySelector('a')).toBeNull();
    expect(chat.textContent).not.toContain('❌');
  });

  it('re-renders an HTML block once the emoji table arrives', async () => {
    let finish!: () => void;
    __resetForgeEmojiForTest(() => new Promise((resolve) => {
      finish = () => resolve(extensionModule);
    }));
    const container = await mount('<p>then :rocket: #12 here end</p>\n', GITHUB, false);
    const p = () => container.querySelector('.markdown-body p')!;
    expect(p().textContent).toBe('then :rocket: #12 here end');
    expect(p().querySelector('a')?.getAttribute('href')).toBe(`${GH}/issues/12`);
    finish();
    await loadForgeEmoji();
    flushSync();
    await waitFor(() => expect(p().textContent).toBe('then 🚀 #12 here end'));
  });

  it.each([GITHUB, GITLAB])('links text in an html region beside its Markdown island (%#)', async (forge) => {
    const source = '<div align="center">#12 :x:\n\n**md** #13\n\n</div>\n\nend\n';
    const container = await mount(source, forge, false);
    const div = container.querySelector('div[align="center"]')!;
    expect(div.textContent).toContain('#12 ❌');
    expect([...div.querySelectorAll('a')].map((a) => a.textContent)).toEqual(['#12', '#13']);
    expect(div.querySelector('strong')?.textContent).toBe('md');
  });
});

describe('links in forge HTML', () => {
  const LINKS = `<p><a href="docs/a.md">doc</a> <a href="/x">root</a> <a href="?q=1">query</a> <a href="/uploads/${HEX}/f.pdf">upload</a> <a href="javascript:alert(1)">script</a> end</p>\n`;

  it.each([false, true])('resolves a relative href where each forge sends it (streaming %s)', async (streaming) => {
    const github = await mount(LINKS, GITHUB, streaming);
    expect(anchors(github.querySelector('.markdown-body')!)).toEqual([
      { text: 'doc', href: 'https://github.com/acme/widget/pull/docs/a.md' },
      { text: 'root', href: 'https://github.com/x' },
      { text: 'query', href: 'https://github.com/acme/widget/pull/4?q=1' },
      { text: 'upload', href: `https://github.com/uploads/${HEX}/f.pdf` },
      { text: 'script', href: null },
    ]);
    const gitlab = await mount(LINKS, GITLAB, streaming);
    expect(anchors(gitlab.querySelectorAll('.markdown-body')[0]!)).toEqual([
      { text: 'doc', href: `${GL}/-/blob/HEAD/docs/a.md` },
      { text: 'root', href: `${GL}/-/blob/HEAD/x` },
      { text: 'query', href: `${GL}/-/merge_requests/3?q=1` },
      // Claimed as the attachment a paragraph's link to it is.
      { text: 'upload', href: expect.stringMatching(/^agent-overflow:forge\?/) },
      { text: 'script', href: null },
    ]);
    const upload = [...gitlab.querySelectorAll('a')].find((a) => a.textContent === 'upload')!;
    expect(parseForgeAttachmentHref(upload.getAttribute('href'))?.href).toBe(`/uploads/${HEX}/f.pdf`);
    expect(upload.getAttribute('title')).toBe('Download f.pdf');
  });

  it('drops a relative href off a forge surface', async () => {
    const container = await mount(LINKS, null, false);
    expect(anchors(container).map((each) => each.href)).toEqual([null, null, null, null, null]);
  });
});

describe('same-document jumps in forge HTML', () => {
  const SOURCE = [
    '<div><a name="named"></a>Named</div>',
    '',
    '<div id="custom">Custom</div>',
    '',
    '<p><a href="#later">to heading</a> <a href="#named">to name</a></p>',
    '',
    '## Later',
    '',
    '[md to html](#named) end',
    '',
  ].join('\n');

  it.each([false, true])('mark HTML anchors and fragment links like Markdown ones (streaming %s)', async (streaming) => {
    const github = await mount(SOURCE, GITHUB, streaming);
    const targets = (root: Element) => [...root.querySelectorAll('[data-md-anchor]')].map((each) => each.getAttribute('data-md-anchor'));
    expect(targets(github)).toEqual(['named', 'custom', 'later']);
    const fragments = [...github.querySelectorAll('a[data-md-fragment]')].map((a) => a.getAttribute('href'));
    expect(fragments).toEqual(['#later', '#named', '#named']);
    // A pane shows many bodies: no authored name becomes a real id or name.
    expect(github.querySelector('[id], a[name]')).toBeNull();

    const gitlab = await mount(SOURCE, GITLAB, streaming);
    // GitLab keeps `<a name>` only.
    expect(targets(gitlab)).toEqual(['named', 'later']);
    expect(gitlab.querySelectorAll('a[data-md-fragment]')).toHaveLength(3);
  });

  it('leaves a fragment href unlinked off a forge surface', async () => {
    const container = await mount(SOURCE, null, false);
    expect(container.querySelector('[data-md-fragment], [data-md-anchor]')).toBeNull();
  });
});

describe('claimed media beside linked text', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => 'blob:forge-1');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });
  afterEach(() => {
    __resetMediaBlobCacheForTest();
    __resetImageTiersForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it.each([false, true])('still hydrates in a cell whose text links (streaming %s)', async (streaming) => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => ({
      url: '/attachments/forge/tok1?ticket=t', mimeType: 'image/png', kind: 'image', sizeBytes: 5, filename: 'a.png',
    }));
    const source = `<table><tr><td><img src="/uploads/${HEX}/a.png"> #12 :x: end</td></tr></table>`;
    const container = await mount(source, GITLAB, streaming);
    await waitFor(() => {
      __reportImageBoxForTest(800);
      expect(container.querySelector('td img[src^="blob:"]')).not.toBeNull();
    });
    expect(anchors(container.querySelector('td')!)).toEqual([{ text: '#12', href: `${GL}/-/issues/12` }]);
    expect(container.querySelector('td')!.textContent).toContain('#12 ❌ end');
    expect(rpc).toHaveBeenCalledTimes(1);
  });
});
