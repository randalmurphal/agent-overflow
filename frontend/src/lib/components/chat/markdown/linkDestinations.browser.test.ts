import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../../app.css';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { countFindMatches, findNeedle } from '../../../utils/findMatch';
import { findTextSegments } from '../../../utils/findHighlights';
import { markdownFindText } from './markdownFindText';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import { parseForgeAttachmentHref, type ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { resetAppearanceForTest, setAppearance } from '../../../stores/appearance.svelte';

// A link or image destination is decided once (the destination hooks),
// whatever syntax wrote it: the same href and claim on both forges and
// both render paths, in Markdown and in HTML, and find counts what shows.
// An authored image never makes the app request its own origin.

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
const HEX = '0123456789abcdef0123456789abcdef';
const UUID = '01234567-89ab-cdef-0123-456789abcdef';

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  // An attachment stays loading: its host is mounted, no bytes arrive.
  setBindingMock('FetchForgeAttachment', () => new Promise(() => {}));
  document.body.innerHTML = '';
});

/** ChatMarkdown as a review comment mounts it; `streaming` renders through the component path, settled through the compact static one. */
async function mounted(source: string, forge: ForgeAttachmentSource | null, streaming = false, embeddedHtml = forge !== null): Promise<Element> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml, streaming, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.innerHTML.trim()).toBeTruthy());
  await new Promise((resolve) => setTimeout(resolve, 50));
  return container.querySelector('.markdown-body')!;
}

/** Each way a link to `href` labelled `guide` can be written, with how many anchors it renders. A
 * definition comes first, so a streamed body has it before the use. */
const WAYS = (href: string): [string, string, number][] => [
  ['inline', `See [guide](${href}) here.`, 1],
  ['reference', `[g]: ${href}\n\nSee [guide][g] here.`, 1],
  ['collapsed reference', `[guide]: ${href}\n\nSee [guide][] here.`, 1],
  ['shortcut reference', `[guide]: ${href}\n\nSee [guide] here.`, 1],
  ['inline HTML', `See <a href="${href}">guide</a> here.`, 1],
  ['HTML block', `<div><a href="${href}">guide</a> here.</div>`, 1],
  ['HTML region around a Markdown island', `<div><a href="${href}">guide</a>\n\nIsland [guide](${href}) text.\n\n</div>`, 2],
];

/** What each forge sends a destination to: a claim's original href, or the anchor's href. */
const DESTINATIONS: [string, string, ForgeAttachmentSource, (href: string | null) => string | null, string][] = [
  ['a relative path', './guide.md', GITHUB, (href) => href, 'https://github.com/acme/widget/pull/guide.md'],
  ['a relative path', './guide.md', GITLAB, (href) => href, 'https://gitlab.com/group/widget/-/blob/HEAD/guide.md'],
  ['a scheme-relative URL', '//example.invalid/guide', GITHUB, (href) => href, 'https://example.invalid/guide'],
  ['a scheme-relative URL', '//example.invalid/guide', GITLAB, (href) => href, 'https://example.invalid/guide'],
  ['an upload', `/uploads/${HEX}/guide.pdf`, GITLAB, (href) => parseForgeAttachmentHref(href)?.href ?? null, `/uploads/${HEX}/guide.pdf`],
];

const CASES = DESTINATIONS.flatMap(([what, href, forge, read, expected]) =>
  WAYS(href).flatMap(([way, source, anchors]) =>
    [false, true].map((streaming) => [`${what} on ${forge.pr.forge}, ${way}, ${streaming ? 'component' : 'static'} path`, source, forge, read, expected, anchors, streaming] as const),
  ),
);

describe('a link destination', () => {
  it.each(CASES)('%s', async (_name, source, forge, read, expected, anchors, streaming) => {
    const root = await mounted(source, forge, streaming);
    const links = [...root.querySelectorAll('a')].filter((a) => a.textContent === 'guide');
    expect(links.map((a) => read(a.getAttribute('href')))).toEqual(Array(anchors).fill(expected));
    // Find counts the text the render shows, the blocked tags it does not.
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs;
    for (const query of ['guide', 'here', 'blocked']) {
      const needle = findNeedle(query);
      const total = (runs: readonly string[]) => runs.reduce((sum, run) => sum + countFindMatches(run, needle), 0);
      expect(total(projected), query).toBe(total(dom));
    }
  });
});

describe('a private attachment image', () => {
  const ASSET = `https://github.com/user-attachments/assets/${UUID}`;
  const UPLOAD = `/uploads/${HEX}/shot.png`;
  it.each([
    ['GitHub', 'a reference', `[s]: ${ASSET}\n\n![shot][s]`, GITHUB],
    ['GitHub', 'a collapsed reference', `[shot]: ${ASSET}\n\n![shot][]`, GITHUB],
    ['GitHub', 'a scheme-relative destination', `![shot](//github.com/user-attachments/assets/${UUID})`, GITHUB],
    ['GitHub', 'a scheme-relative HTML src', `<div><img alt="shot" src="//github.com/user-attachments/assets/${UUID}"></div>`, GITHUB],
    ['GitLab', 'a reference', `[s]: ${UPLOAD}\n\n![shot][s]`, GITLAB],
    ['GitLab', 'a shortcut reference', `[shot]: ${UPLOAD}\n\n![shot]`, GITLAB],
    ['GitLab', 'a scheme-relative destination', `![shot](//gitlab.com/group/widget${UPLOAD})`, GITLAB],
    ['GitLab', 'a scheme-relative HTML src', `<div><img alt="shot" src="//gitlab.com/group/widget${UPLOAD}"></div>`, GITLAB],
  ] as const)('on %s, written as %s, loads through the attachment host', async (_forge, _way, source, forge) => {
    for (const streaming of [false, true]) {
      const root = await mounted(source, forge, streaming);
      await waitFor(() => expect(root.querySelector('[data-forge-attachment-loading]')).not.toBeNull());
      expect(root.querySelector('img[src^="http"], img[src^="//"]')).toBeNull();
      document.body.innerHTML = '';
    }
  });
});

describe('a private attachment written as a URL the parser reads differently', () => {
  // The hooks see the URL a renderer would fetch, so no spelling reaches the
  // private host directly. Backslashes are doubled once for JS; Markdown
  // reads `\\` as one more.
  const GH = 'private-user-images.githubusercontent.com/123/shot.png';
  const GL = `gitlab.com/group/widget/uploads/${HEX}/shot.png`;
  const backslashed = (path: string) => path.replaceAll('/', '\\');
  const image: [string, string, ForgeAttachmentSource][] = [
    ['GitHub block HTML, one slash', `<div><img src="https:/${GH}" alt="shot"></div>`, GITHUB],
    ['GitHub inline HTML, one slash', `See <img src="https:/${GH}" alt="shot"> here.`, GITHUB],
    ['GitHub Markdown, backslashes', `![shot](https:\\\\${backslashed(GH)})`, GITHUB],
    ['GitHub block HTML, a tab in the scheme', `<div><img src="ht&#9;tps://${GH}" alt="shot"></div>`, GITHUB],
    ['GitHub block HTML, a newline in the host', `<div><img src="https://private-user-images.github&#10;usercontent.com/123/shot.png" alt="shot"></div>`, GITHUB],
    ['GitHub inline HTML, leading space and userinfo', `See <img src=" https://u@${GH}" alt="shot"> here.`, GITHUB],
    ['GitLab Markdown, one slash', `![shot](https:/${GL})`, GITLAB],
    ['GitLab block HTML, backslashes', `<div><img src="https:${backslashed(`//${GL}`)}" alt="shot"></div>`, GITLAB],
    ['GitLab inline HTML, one slash', `See <img src="https:/${GL}" alt="shot"> here.`, GITLAB],
  ];
  const link: [string, string, ForgeAttachmentSource][] = [
    ['GitHub Markdown, one slash', `See [shot](https:/${GH}) here.`, GITHUB],
    ['GitHub block HTML, backslashes', `<div><a href="https:\\\\${backslashed(GH)}">shot</a></div>`, GITHUB],
    ['GitLab Markdown, one slash', `See [shot](https:/${GL}) here.`, GITLAB],
    ['GitLab block HTML, a tab after the scheme', `<div><a href="https:&#9;//${GL}">shot</a></div>`, GITLAB],
  ];
  const direct = (root: Element): string[] =>
    [...root.querySelectorAll('img, a, source')].flatMap((element) =>
      ['src', 'href', 'srcset'].flatMap((name) => {
        const value = element.getAttribute(name);
        if (value === null) return [];
        const url = URL.parse(value, location.href);
        return url && /^https?:$/.test(url.protocol) && (url.host === 'private-user-images.githubusercontent.com' || url.pathname.includes(`/uploads/${HEX}/`)) ? [value] : [];
      }),
    );

  it.each(image)('as an image, %s, loads through the attachment host', async (_name, source, forge) => {
    for (const streaming of [false, true]) {
      const root = await mounted(source, forge, streaming);
      await waitFor(() => expect(root.querySelector('[data-forge-attachment-loading]')).not.toBeNull());
      expect(direct(root)).toEqual([]);
      document.body.innerHTML = '';
    }
  });

  it.each(link)('as a link, %s, is the attachment link', async (_name, source, forge) => {
    for (const streaming of [false, true]) {
      const root = await mounted(source, forge, streaming);
      const anchor = [...root.querySelectorAll('a')].find((a) => a.textContent === 'shot');
      expect(parseForgeAttachmentHref(anchor?.getAttribute('href') ?? null)).not.toBeNull();
      expect(direct(root)).toEqual([]);
      document.body.innerHTML = '';
    }
  });
});

describe('a <video> claim', () => {
  // A video plays what the HTML parser reads as its src or its first
  // `<source src>`, in a sentence, alone on its line, or in a block.
  const U = `/uploads/${HEX}/still.png`;
  const VIDEOS: [string, string, number][] = [
    ['src text inside another attribute', `<video title="text src='${U}'">fallback</video>`, 0],
    ['a <source> inside a comment', `<video><!-- <source src="${U}"> -->fallback</video>`, 0],
    ['its own src', `<video src="${U}">fallback</video>`, 1],
    ['a <source src>', `<video><source src="${U}">fallback</video>`, 1],
    ['a <source> after a comment holding </video>', `<video><!-- </video> --><source src="${U}">fallback</video>`, 1],
    ['a <source> after an unclaimed own src', `<video src="https://example.invalid/a.mp4"><source src="${U}"></video>`, 1],
    ['a quoted > before its src', `<video title='a>b' src="${U}">fallback</video>`, 1],
  ];
  const FORMS: [string, (video: string) => string][] = [
    ['in a sentence', (video) => `See ${video} here.`],
    ['alone on its line', (video) => video],
    ['in a block', (video) => `<div>${video}</div>`],
  ];
  it.each(VIDEOS)('reads %s the same in every position', async (_name, video, claims) => {
    for (const [form, write] of FORMS) {
      for (const streaming of [false, true]) {
        const root = await mounted(write(video), GITLAB, streaming);
        expect(root.querySelectorAll('[data-forge-attachment-loading]').length, `${form}, ${streaming ? 'component' : 'static'}`).toBe(claims);
        document.body.innerHTML = '';
      }
    }
  });
});

describe('a relative link the URL parser reads as naming another host', () => {
  // `/\host` and `\\host` resolve as `//host` in a browser; relative means
  // on the forge, so neither links off it.
  it.each(
    [GITHUB, GITLAB].flatMap((forge) =>
      ['/\\elsewhere.invalid/x', '\\\\elsewhere.invalid/x', '\\/elsewhere.invalid/x'].flatMap((href) =>
        [
          ['Markdown', `See [label](<${href}>) here.`],
          ['block HTML', `<div><a href="${href}">label</a> here.</div>`],
          ['inline HTML', `See <a href="${href}">label</a> here.`],
        ].map(([way, source]) => [`${forge.pr.forge}, ${way}, ${href}`, source, forge] as const),
      ),
    ),
  )('%s stays on the forge', async (_name, source, forge) => {
    for (const streaming of [false, true]) {
      const root = await mounted(source, forge, streaming);
      expect(root.textContent).toContain('label');
      for (const anchor of root.querySelectorAll('a[href]')) {
        expect(new URL(anchor.getAttribute('href')!, location.href).host).toBe(forge.pr.host);
      }
      document.body.innerHTML = '';
    }
  });
});

describe('an authored image on the page\'s own origin', () => {
  const FIXTURE = '/src/test/fixtures/images/badge-80x20.svg';
  // The test server answers on both loopback names; the other one is
  // another origin, which an image may load from.
  const otherOrigin = (): string => {
    const url = new URL(location.origin);
    url.hostname = url.hostname === 'localhost' ? '127.0.0.1' : 'localhost';
    return url.origin;
  };
  let probe = 0;
  const requested = (url: string) => performance.getEntriesByType('resource').some((entry) => entry.name === url);

  it.each([
    ['a Markdown image in agent chat', null, (own: string) => `See ![badge](${own}) here.`],
    ['a Markdown image on GitHub', GITHUB, (own: string) => `See ![badge](${own}) here.`],
    ['a Markdown image on GitLab', GITLAB, (own: string) => `See ![badge](${own}) here.`],
    ['an inline HTML image on GitHub', GITHUB, (own: string) => `See <img alt="badge" src="${own}"> here.`],
    ['an inline HTML image on GitLab', GITLAB, (own: string) => `See <img alt="badge" src="${own}"> here.`],
    ['a block HTML image on GitLab', GITLAB, (own: string) => `<div><img alt="badge" src="${own}"></div>`],
    ['a block HTML image on GitHub', GITHUB, (own: string) => `<p align="center"><img alt="badge" src="${own}"></p>`],
  ] as const)('is not requested: %s', async (_name, forge, write) => {
    for (const streaming of [false, true]) {
      const own = `${location.origin}${FIXTURE}?own-${++probe}`;
      const root = await mounted(write(own), forge, streaming);
      await waitFor(() => expect(root.textContent).toContain('[Image unavailable: badge]'));
      for (const img of root.querySelectorAll('img')) {
        expect(img.getAttribute('src')?.startsWith(location.origin) ?? false).toBe(false);
        expect(img.currentSrc.startsWith(location.origin)).toBe(false);
      }
      expect(requested(own)).toBe(false);
      document.body.innerHTML = '';
    }
  });

  it('loads from another origin, as the control', async () => {
    const other = `${otherOrigin()}${FIXTURE}?other-${++probe}`;
    const root = await mounted(`<div><img alt="badge" src="${other}"></div>`, GITLAB);
    await waitFor(() => expect(root.querySelector('img')?.getAttribute('src')).toBe(other));
    // The bytes arrive: the test server answers on its other loopback name.
    await root.querySelector('img')!.decode();
  });

  it.each([
    ['an inline <picture>', (own: string, other: string) => `See ${picture(own, other)} here.`],
    ['a <picture> in an HTML block', (own: string, other: string) => `<p align="center">${picture(own, other)}</p>`],
  ])('is not requested as %s source on GitHub: the fallback paints', async (_name, write) => {
    for (const streaming of [false, true]) {
      const own = `${location.origin}${FIXTURE}?own-${++probe}`;
      const other = `${otherOrigin()}${FIXTURE}?other-${++probe}`;
      const root = await mounted(write(own, other), GITHUB, streaming);
      await waitFor(() => expect(root.querySelector('img')?.getAttribute('src')).toBe(other));
      for (const img of root.querySelectorAll('img')) expect(img.currentSrc.startsWith(location.origin)).toBe(false);
      expect(requested(own)).toBe(false);
      document.body.innerHTML = '';
    }
  });

  // A source no surface can paint never wins over a fallback that paints.
  it.each(
    (['dark', 'light'] as const).flatMap((mode) => [
      [mode, 'an inline <picture>', (source: string, other: string) => `See ${picture(source, other)} here.`],
      [mode, 'a <picture> in an HTML block', (source: string, other: string) => `<p align="center">${picture(source, other)}</p>`],
    ] as const),
  )('in the %s scheme, %s with ftp: sources paints its fallback on GitHub', async (mode, _name, write) => {
    setBindingMock('SetAppearance', async () => {});
    try {
      void setAppearance({ mode });
      for (const streaming of [false, true]) {
        const other = `${otherOrigin()}${FIXTURE}?other-${++probe}`;
        const root = await mounted(write('ftp://example.invalid/a.png', other), GITHUB, streaming);
        await waitFor(() => expect(root.querySelector('img')?.getAttribute('src')).toBe(other));
        expect(root.textContent).not.toContain('unavailable');
        expect(root.textContent).not.toContain('does not display');
        document.body.innerHTML = '';
      }
    } finally {
      resetAppearanceForTest();
    }
  });
});

/** A picture whose every color scheme's source is `own`, with `other` as its fallback. */
function picture(own: string, other: string): string {
  return (
    `<picture><source media="(prefers-color-scheme: dark)" srcset="${own}">` +
    `<source media="(prefers-color-scheme: light)" srcset="${own}">` +
    `<img alt="badge" src="${other}"></picture>`
  );
}
