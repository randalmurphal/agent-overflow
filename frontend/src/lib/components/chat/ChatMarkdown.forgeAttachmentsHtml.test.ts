import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import ImageMenuHost from './ImageMenuHost.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __reportImageBoxForTest, __resetImageTiersForTest } from '../../utils/imageTiers';
import { __resetMediaBlobCacheForTest } from '../../utils/mediaBlobCache';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Forge media a PR template writes INSIDE an HTML wrapper. There is no
// markdown token to claim there — the whole run is one html block — so the
// path under test is the sanitizer's claim hook plus `EmbeddedHtmlHost`.
// Both renderers are exercised: a settled body goes through the compact
// static serializer (which must bail for an html token), a streaming one
// through the component tree.

vi.mock('../../transport/deviceSession', () => ({
  fetchPairedComputer: async () => new Response('bytes', { status: 200 }),
}));
vi.mock('../../transport/backends', () => ({
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));

const HEX = '0123456789abcdef0123456789abcdef';
const UUID = '4f0b0b1e-1111-2222-3333-444455556666';
const GITHUB_ASSET = `https://github.com/user-attachments/assets/${UUID}`;
const GITLAB_UPLOAD = `/uploads/${HEX}/a.png`;
const PRIVATE_ASSET = 'https://private-user-images.githubusercontent.com/123/asset.png?jwt=example';

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'octo', repo: 'widget', number: 7 },
  backend: 'gpu',
  webBase: 'https://github.com/octo/widget/pull/7',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'gpu',
  webBase: 'https://gitlab.example.test/group/widget/-/merge_requests/3',
};

// One ticket per mint. The id shape matters: `backendTransferUrl` admits
// only `/attachments/forge/<token>`, so a URL-shaped id would be refused
// before any fetch.
let ticket = 0;
function stageAttachment(kind: 'image' | 'video' = 'image') {
  return setBindingMock('FetchForgeAttachment', async () => ({
    url: `/attachments/forge/tok${++ticket}?ticket=t`,
    mimeType: kind === 'video' ? 'video/mp4' : 'image/png',
    kind,
    sizeBytes: 5,
    filename: kind === 'video' ? 'clip.mp4' : 'a.png',
  }));
}

beforeEach(() => {
  let n = 0;
  vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:forge-${++n}`);
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
});

afterEach(() => {
  __resetMediaBlobCacheForTest();
  __resetImageTiersForTest();
  resetBindingMocks();
  vi.restoreAllMocks();
});

// happy-dom lays nothing out: report every observed container's width so
// each host makes its first request.
function measure(): void {
  __reportImageBoxForTest(800);
}

// `streaming` selects the render path: false renders one settled
// `.md-committed` instance the compact static serializer tries first, true
// puts the body in the `.md-volatile` component instance.
const paths: Array<[string, boolean, string]> = [
  ['the compact static path', false, '.md-committed'],
  ['the component path', true, '.md-volatile'],
];

describe.each(paths)('forge media inside an html block, on %s', (_name, streaming, root) => {
  it('renders a GitHub asset wrapped in <p align="center">', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: `<p align="center"><img src="${GITHUB_ASSET}" alt="a shot"></p>`,
        forgeSource: GITHUB,
        streaming,
      },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} img[src^="blob:"]`)).not.toBeNull();
    });
    const image = container.querySelector('img')!;
    expect(image.getAttribute('alt')).toBe('a shot');
    // The wrapper the forge wrote survives; only the media is replaced.
    expect(container.querySelector('p[align="center"] img')).not.toBeNull();
    // The page never points an element at the forge URL it cannot read.
    expect(container.querySelector(`img[src="${GITHUB_ASSET}"]`)).toBeNull();
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc.mock.calls[0]?.[1]).toBe(GITHUB_ASSET);
  });

  // GitHub's uploader writes `<img width height src>`, and an author narrows
  // a screenshot by editing the width. Three positions, three claim paths:
  // alone on its line (the block extension), in a sentence (the inline
  // extension) and inside a wrapper (the sanitizer and EmbeddedHtmlHost).
  it.each([
    ['alone on its line', `<img width="300" alt="a shot" src="${GITHUB_ASSET}">`],
    ['in a sentence', `See <img width="300" alt="a shot" src="${GITHUB_ASSET}"> here.`],
    ['inside a wrapper', `<p align="center"><img width="300" alt="a shot" src="${GITHUB_ASSET}"></p>`],
    ['as a picture fallback', `See <picture><source srcset="https://example.com/dark.png"><img width="300" alt="a shot" src="${GITHUB_ASSET}"></picture> here.`],
  ])('paints a claimed image at its authored width, %s', async (_position, source) => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, { props: { source, forgeSource: GITHUB, streaming } });

    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} img[src^="blob:"]`)).not.toBeNull();
    });
    const image = container.querySelector<HTMLImageElement>(`${root} img[src^="blob:"]`)!;
    expect(image.style.width).toBe('300px');
    expect(image.style.height).toBe('');
    expect(container.querySelector(`img[src="${GITHUB_ASSET}"]`)).toBeNull();
    expect(container.textContent).not.toContain('picture');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  // GitLab's rich text editor writes `{width=…}` after an image it resized.
  it('paints a GitLab upload at the width of its attribute list, which shows nothing', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: `See ![a shot](${GITLAB_UPLOAD}){width=300} here.`, forgeSource: GITLAB, streaming },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} img[src^="blob:"]`)).not.toBeNull();
    });
    const image = container.querySelector<HTMLImageElement>(`${root} img[src^="blob:"]`)!;
    expect(image.style.width).toBe('300px');
    expect(container.textContent).not.toContain('{');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('renders a GitLab upload inside a table cell', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: `<table><tr><td><img src="${GITLAB_UPLOAD}"></td></tr></table>`,
        forgeSource: GITLAB,
        streaming,
      },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} td img[src^="blob:"]`)).not.toBeNull();
    });
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc.mock.calls[0]?.[1]).toBe(GITLAB_UPLOAD);
  });

  it('renders media inside a <details> body, wrapper and all', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source:
          `<details><summary>Screenshots</summary>` +
          `<div align="center"><img src="${GITLAB_UPLOAD}"></div></details>`,
        forgeSource: GITLAB,
        streaming,
      },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelector('details img[src^="blob:"]')).not.toBeNull();
    });
    expect(container.querySelector('summary')?.textContent).toContain('Screenshots');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('plays a wrapped <video>, which used to render as escaped literal tags', async () => {
    const rpc = stageAttachment('video');
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: `<div align="center"><video src="/uploads/${HEX}/clip.mp4" controls></video></div>`,
        forgeSource: GITLAB,
        streaming,
      },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} video[src^="blob:"]`)).not.toBeNull();
    });
    expect(container.textContent).not.toContain('<video>');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('fetches once per distinct href, however many times it is embedded', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source:
          `<p><img src="${GITLAB_UPLOAD}"><img src="${GITLAB_UPLOAD}">` +
          `<img src="/uploads/${HEX}/b.png"></p>`,
        forgeSource: GITLAB,
        streaming,
      },
    });

    await waitFor(() => {
      measure();
      expect(container.querySelectorAll('img[src^="blob:"]')).toHaveLength(3);
    });
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('leaves a non-forge image in an html block exactly as it was', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: '<p align="center"><img src="https://example.test/x.png" alt="b"></p>',
        forgeSource: GITLAB,
        streaming,
      },
    });

    await waitFor(() => {
      expect(container.querySelector(`${root} img`)).not.toBeNull();
    });
    expect(container.querySelector('img')!.getAttribute('src')).toBe(
      'https://example.test/x.png',
    );
    expect(container.querySelector('[data-markdown-media-claim]')).toBeNull();
    expect(rpc).not.toHaveBeenCalled();
  });
});

describe('forge media inside an html block, with no forge source', () => {
  it('renders the sanitizer output it always did', async () => {
    const rpc = stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: `<p align="center"><img src="${GITLAB_UPLOAD}" alt="a shot"></p>`,
        forgeSource: null,
      },
    });

    await waitFor(() => expect(container.querySelector('p[align="center"]')).not.toBeNull());
    // A path-relative src is refused by the sanitizer, as before.
    expect(container.querySelector('img')!.hasAttribute('src')).toBe(false);
    expect(container.querySelector('[data-markdown-embedded-html]')).toBeNull();
    expect(rpc).not.toHaveBeenCalled();
  });
});

// Every path a forge image renders through carries the image menu: a
// markdown image token, and media inside an HTML wrapper, on both renderers.
// Video and audio do not.
describe.each(paths)('the image menu on forge media, on %s', (_name, streaming, root) => {
  async function rightClick(target: Element): Promise<void> {
    await fireEvent(target, new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 5, clientY: 5 }));
    await tick();
  }
  function imageMenu(): Element | null {
    return document.querySelector('[role="menu"][aria-label="Image Actions"]');
  }

  it.each([
    ['a markdown image', `![a shot](${GITHUB_ASSET})`],
    ['an image inside an HTML wrapper', `<p align="center"><img src="${GITHUB_ASSET}" alt="a shot"></p>`],
    ['a linked <picture>', `[<picture><img src="${PRIVATE_ASSET}"></picture>](https://example.com)`],
    ['a linked <img>', `[<img src="${PRIVATE_ASSET}" alt="a shot">](https://example.com)`],
    ['a linked markdown image', `[![a shot](${PRIVATE_ASSET})](https://example.com)`],
  ])('opens on %s', async (_shape, source) => {
    stageAttachment();
    render(ImageMenuHost);
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source, forgeSource: GITHUB, streaming },
    });
    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} img[src^="blob:"]`)).not.toBeNull();
    });
    // The page never points an element at the forge URL it cannot read.
    expect(container.querySelector(`img[src*="githubusercontent"], img[src*="user-attachments"]`)).toBeNull();
    await rightClick(container.querySelector(`${root} img[src^="blob:"]`)!);
    expect(imageMenu()).not.toBeNull();
  });

  it('keeps a linked image inside its link', async () => {
    stageAttachment();
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: `[<picture><img src="${PRIVATE_ASSET}"></picture>](https://example.com)`, forgeSource: GITHUB, streaming },
    });
    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} a[href^="https://example.com"] img[src^="blob:"]`)).not.toBeNull();
    });
  });

  it('does not open on a video', async () => {
    stageAttachment('video');
    render(ImageMenuHost);
    const { container } = render(ForgeAttachmentChatHarness, {
      props: {
        source: `<div align="center"><video src="/uploads/${HEX}/clip.mp4" controls></video></div>`,
        forgeSource: GITLAB,
        streaming,
      },
    });
    await waitFor(() => {
      measure();
      expect(container.querySelector(`${root} video[src^="blob:"]`)).not.toBeNull();
    });
    await rightClick(container.querySelector(`${root} video`)!);
    expect(imageMenu()).toBeNull();
  });
});
