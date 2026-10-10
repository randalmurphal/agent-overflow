// The shared markdown image: which tier it asks for and when, the box it
// reserves, the swap to a sharper variant, the remount fast path, the
// lightbox it opens and every claim it releases.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import MarkdownImage from './MarkdownImage.svelte';
import { closeImageLightbox, imageLightbox } from '../../../stores/imageLightbox.svelte';
import {
  __reportImageBoxForTest,
  __resetImageTiersForTest,
  lastImageTier,
  rememberImageTier,
} from '../../../utils/imageTiers';
import type { MarkdownImageSource, MarkdownImageVariant } from '../../../utils/markdownImageSource';
import type { MediaHandle } from '../../../utils/mediaBlobCache';
import { reportFrontendDiagnostic } from '../../../utils/frontendErrorCapture';

vi.mock('../../../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

const KEY = '["local","gpu","/w","/w/shot.png"]';
const ORIGINAL = { originalWidth: 3000, originalHeight: 1500, originalBytes: 900_000 };

interface Entry {
  settled: MarkdownImageVariant | undefined;
  value: Promise<MarkdownImageVariant>;
  resolve: (value: MarkdownImageVariant) => void;
  reject: (cause: unknown) => void;
  retained: number;
}

/**
 * A source whose tiers settle only when the test says so, and which counts
 * retention per tier the way the media cache does.
 */
function fakeSource(variantFor: (tier: number) => Partial<MarkdownImageVariant> = derivative) {
  const entries = new Map<number, Entry>();
  const entry = (tier: number): Entry => {
    let found = entries.get(tier);
    if (!found) {
      let resolve!: (value: MarkdownImageVariant) => void;
      let reject!: (cause: unknown) => void;
      const value = new Promise<MarkdownImageVariant>((res, rej) => {
        resolve = res;
        reject = rej;
      });
      void value.catch(() => {});
      const created: Entry = { settled: undefined, value, resolve, reject, retained: 0 };
      found = created;
      entries.set(tier, created);
    }
    return found;
  };
  const source: MarkdownImageSource = {
    key: KEY,
    filename: 'shot.png',
    menuTag: { 'data-image-menu': 'local', 'data-image-menu-backend': 'gpu' },
    acquire: vi.fn((tier: number): MediaHandle<MarkdownImageVariant> => {
      const held = entry(tier);
      held.retained += 1;
      let released = false;
      return {
        get settled() {
          return held.settled;
        },
        value: held.value,
        release: () => {
          if (released) return;
          released = true;
          held.retained -= 1;
        },
      };
    }),
    original: vi.fn(async () => new Blob(['original'], { type: 'image/png' })),
  };
  return {
    source,
    tiers: () => vi.mocked(source.acquire).mock.calls.map((call) => call[0]),
    retained: (tier: number) => entries.get(tier)?.retained ?? 0,
    async settle(tier: number): Promise<void> {
      const held = entry(tier);
      const value = variant(tier, variantFor(tier));
      held.settled = value;
      held.resolve(value);
      await tick();
      await tick();
    },
    async fail(tier: number, message: string): Promise<void> {
      entry(tier).reject(new Error(message));
      await tick();
      await tick();
    },
  };
}

function variant(tier: number, overrides: Partial<MarkdownImageVariant>): MarkdownImageVariant {
  return {
    url: `blob:tier-${tier}`,
    mimeType: 'image/png',
    blob: new Blob([`tier-${tier}`]),
    width: tier,
    height: tier / 2,
    ...ORIGINAL,
    derived: true,
    ...overrides,
  };
}

function derivative(tier: number): Partial<MarkdownImageVariant> {
  // The ladder never goes past the original: a tier at least its width is
  // the original itself.
  return tier === 0 || tier >= ORIGINAL.originalWidth
    ? { width: ORIGINAL.originalWidth, height: ORIGINAL.originalHeight, derived: false }
    : {};
}

function mount(source: MarkdownImageSource, props: Record<string, unknown> = {}) {
  return render(MarkdownImage, {
    props: { source, alt: 'diagram', markdownImageSrc: '/w/shot.png', ...props },
  });
}

class FakeResizeObserver {
  static instances: FakeResizeObserver[] = [];
  readonly observed = new Set<Element>();
  readonly unobserved: Element[] = [];
  constructor(_callback: ResizeObserverCallback) {
    FakeResizeObserver.instances.push(this);
  }
  observe(element: Element): void {
    this.observed.add(element);
  }
  unobserve(element: Element): void {
    this.observed.delete(element);
    this.unobserved.push(element);
  }
  disconnect(): void {
    this.observed.clear();
  }
}

beforeEach(() => {
  FakeResizeObserver.instances = [];
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
  vi.stubGlobal('devicePixelRatio', 1);
});

afterEach(() => {
  closeImageLightbox();
  __resetImageTiersForTest();
  vi.unstubAllGlobals();
});

describe('<MarkdownImage>', () => {
  it('asks for nothing until its container is measured, then for the tier that width needs', async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    expect(container.querySelector('[data-streamdown-image-loading]')).not.toBeNull();
    expect(fake.tiers()).toEqual([]);

    __reportImageBoxForTest(700);
    expect(fake.tiers()).toEqual([720]);
    // Remembered at the request, so a mount elsewhere shares it.
    expect(lastImageTier(KEY)).toBe(720);
    await fake.settle(720);
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-720');
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();
  });

  it('counts device pixels, not CSS pixels', () => {
    vi.stubGlobal('devicePixelRatio', 2);
    const fake = fakeSource();
    mount(fake.source);
    __reportImageBoxForTest(700);
    expect(fake.tiers()).toEqual([1440]);
  });

  it("reserves the original's box while a derivative is painted", async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    const img = container.querySelector('img')!;
    expect(img.getAttribute('width')).toBe('3000');
    expect(img.getAttribute('height')).toBe('1500');
    expect(img.getAttribute('alt')).toBe('diagram');
    expect(img.getAttribute('data-markdown-image-src')).toBe('/w/shot.png');
    expect(img.getAttribute('data-image-menu')).toBe('local');
  });

  it('falls back to the served size when the original size is unknown', async () => {
    const fake = fakeSource(() => ({ originalWidth: 0, originalHeight: 0, derived: false }));
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    expect(container.querySelector('img')?.getAttribute('width')).toBe('720');
    expect(container.querySelector('img')?.getAttribute('height')).toBe('360');
  });

  it('swaps a sharper variant into the same element and box when the container grows', async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    const img = container.querySelector('img')!;

    __reportImageBoxForTest(1200);
    expect(fake.tiers()).toEqual([720, 1440]);
    // The old variant stays painted until the new one lands.
    expect(img.getAttribute('src')).toBe('blob:tier-720');
    expect(lastImageTier(KEY)).toBe(720);
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();

    await fake.settle(1440);
    expect(container.querySelector('img')).toBe(img);
    expect(img.getAttribute('src')).toBe('blob:tier-1440');
    expect(img.getAttribute('width')).toBe('3000');
    expect(img.getAttribute('height')).toBe('1500');
    expect(fake.retained(720)).toBe(0);
    expect(fake.retained(1440)).toBe(1);
    expect(lastImageTier(KEY)).toBe(1440);
  });

  it('keeps what is painted when the container narrows', async () => {
    const fake = fakeSource();
    mount(fake.source);
    __reportImageBoxForTest(1200);
    await fake.settle(1440);
    __reportImageBoxForTest(300);
    expect(fake.tiers()).toEqual([1440]);
  });

  it('never upgrades the original, even one wider than the tier it was asked at', async () => {
    // The backend serves an animated GIF or an SVG whole at every tier.
    for (const original of [
      { width: 3000, height: 1500, originalWidth: 3000, originalHeight: 1500, derived: false },
      { width: 0, height: 0, originalWidth: 0, originalHeight: 0, derived: false },
    ]) {
      const fake = fakeSource(() => original);
      const view = mount(fake.source);
      __reportImageBoxForTest(700);
      await fake.settle(720);
      __reportImageBoxForTest(2000);
      expect(fake.tiers()).toEqual([720]);
      view.unmount();
      __resetImageTiersForTest();
    }
  });

  it("asks no more than the original's width when the container is wider", async () => {
    const fake = fakeSource();
    mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    // min(4000, 3000) device pixels is the 3840 tier, not the original.
    __reportImageBoxForTest(4000);
    expect(fake.tiers()).toEqual([720, 3840]);
  });

  it('keeps one request in flight and re-checks the width when it lands', async () => {
    const fake = fakeSource();
    mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    __reportImageBoxForTest(1200);
    __reportImageBoxForTest(2000);
    expect(fake.tiers()).toEqual([720, 1440]);
    await fake.settle(1440);
    expect(fake.tiers()).toEqual([720, 1440, 2160]);
  });

  it('paints a remount from the remembered tier in the frame it mounts, before any measurement', async () => {
    const fake = fakeSource();
    const first = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    first.unmount();

    const second = mount(fake.source);
    expect(second.container.querySelector('[data-streamdown-image-loading]')).toBeNull();
    expect(second.container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-720');
    expect(second.container.querySelector('img')?.getAttribute('width')).toBe('3000');
    expect(fake.tiers()).toEqual([720, 720]);
  });

  it('asks for a remembered tier still in flight instead of measuring a second request', async () => {
    const fake = fakeSource();
    rememberImageTier(KEY, 720);
    const { container } = mount(fake.source);
    expect(fake.tiers()).toEqual([720]);
    expect(container.querySelector('[data-streamdown-image-loading]')).not.toBeNull();
    __reportImageBoxForTest(1200);
    expect(fake.tiers()).toEqual([720]);
    await fake.settle(720);
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-720');
    // Landed: now the measured width can ask for more.
    expect(fake.tiers()).toEqual([720, 1440]);
  });

  it("paints a host's held tier at once, prefers it to the remembered one, and owns that claim", async () => {
    const fake = fakeSource();
    rememberImageTier(KEY, 1440);
    void fake.settle(480);
    const handle = fake.source.acquire(480);
    const { container } = mount(fake.source, { initial: { tier: 480, handle } });
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-480');
    // The host's own acquire is the only one; the picture made no second claim.
    expect(fake.tiers()).toEqual([480]);
    expect(fake.retained(480)).toBe(1);

    // An upgrade releases the host's claim, so nothing stays behind the
    // sharper variant; the host's own release later is a no-op.
    __reportImageBoxForTest(700);
    await fake.settle(720);
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-720');
    expect(fake.retained(480)).toBe(0);
    handle.release();
    expect(fake.retained(480)).toBe(0);
    expect(fake.retained(720)).toBe(1);
  });

  it('opens the lightbox on the painted variant and fetches the original only behind a derivative', async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    const picture = container.querySelector<HTMLElement>('[data-streamdown-image]')!;
    expect(picture.classList.contains('cursor-zoom-in')).toBe(true);

    await fireEvent.click(container.querySelector('img')!);
    const preview = imageLightbox();
    expect(preview?.index).toBe(0);
    expect(preview?.images).toHaveLength(1);
    const item = preview!.images[0]!;
    expect(item).toMatchObject({
      id: KEY,
      filename: 'shot.png',
      mimeType: 'image/png',
      url: 'blob:tier-720',
      width: 3000,
      height: 1500,
      originalBytes: 900_000,
      menuTag: fake.source.menuTag,
    });
    const signal = new AbortController().signal;
    expect(await (await item.original!(signal, 0)).text()).toBe('original');
    expect(fake.source.original).toHaveBeenCalledWith(signal, 0);
  });

  it('opens the original as it is, with nothing to fetch behind it', async () => {
    const fake = fakeSource(() => ({ width: 640, height: 480, originalWidth: 640, originalHeight: 480, derived: false }));
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    await fireEvent.click(container.querySelector('img')!);
    const item = imageLightbox()!.images[0]!;
    expect(item.original).toBeUndefined();
    expect(item.width).toBe(640);
  });

  it('is a named, focusable control that Enter and Space open', async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    const picture = container.querySelector<HTMLElement>('[data-streamdown-image]')!;
    expect(picture.getAttribute('role')).toBe('button');
    expect(picture.getAttribute('tabindex')).toBe('0');
    expect(picture.getAttribute('aria-label')).toBe('Preview diagram');

    await fireEvent.keyDown(picture, { key: 'Enter' });
    expect(imageLightbox()?.images[0]?.url).toBe('blob:tier-720');
    closeImageLightbox();
    await fireEvent.keyDown(picture, { key: 'a' });
    expect(imageLightbox()).toBeNull();
    await fireEvent.keyDown(picture, { key: ' ' });
    expect(imageLightbox()).not.toBeNull();
  });

  it('keeps the bytes the lightbox opened on until it closes, whatever the row does', async () => {
    const fake = fakeSource();
    const view = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    await fireEvent.click(view.container.querySelector('img')!);
    expect(fake.retained(720)).toBe(2);
    view.unmount();
    expect(fake.retained(720)).toBe(1);
    closeImageLightbox();
    expect(fake.retained(720)).toBe(0);
  });

  it('lets a linked image follow its link', async () => {
    const paragraph = document.createElement('p');
    const link = document.createElement('a');
    link.href = 'https://example.test/full.png';
    paragraph.appendChild(link);
    document.body.appendChild(paragraph);
    const fake = fakeSource();
    try {
      const { container } = render(MarkdownImage, {
        target: link,
        props: { source: fake.source, alt: 'diagram' },
      });
      // The inline link has no width of its own: the paragraph bounds it.
      __reportImageBoxForTest(700, paragraph);
      expect(fake.tiers()).toEqual([720]);
      await fake.settle(720);
      await tick();
      // The anchor is the control: the picture has no role, no tab stop and
      // no zoom cursor of its own.
      const picture = container.querySelector<HTMLElement>('[data-streamdown-image]')!;
      expect(picture.getAttribute('role')).toBeNull();
      expect(picture.hasAttribute('tabindex')).toBe(false);
      expect(picture.classList.contains('cursor-zoom-in')).toBe(false);
      await fireEvent.click(container.querySelector('img')!);
      expect(imageLightbox()).toBeNull();
    } finally {
      paragraph.remove();
    }
  });

  it('releases every claim and stops observing when it unmounts', async () => {
    const fake = fakeSource();
    const view = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    __reportImageBoxForTest(1200);
    expect(fake.retained(720)).toBe(1);
    expect(fake.retained(1440)).toBe(1);

    view.unmount();
    expect(fake.retained(720)).toBe(0);
    expect(fake.retained(1440)).toBe(0);
    const [observer] = FakeResizeObserver.instances;
    expect(observer?.unobserved).toContain(view.container);
    __reportImageBoxForTest(3000);
    expect(fake.tiers()).toEqual([720, 1440]);
    // A late answer for the abandoned request changes nothing.
    await fake.settle(1440);
    expect(fake.retained(1440)).toBe(0);
  });

  it('names the reason in the chip and keeps the whole message as its tooltip', async () => {
    const fake = fakeSource();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.fail(720, 'load local image: file not found: /w/shot.png: open /w/shot.png: no such file or directory');
    const chip = container.querySelector('[data-streamdown-image-error]');
    expect(chip?.textContent).toContain('[Image unavailable: diagram (file not found)]');
    expect(chip?.getAttribute('title')).toContain('no such file or directory');
    expect(container.querySelector('[data-streamdown-image-loading]')).toBeNull();
    expect(fake.retained(720)).toBe(0);
  });

  it('keeps the painted variant when a sharper one fails, records it, and asks again only for a higher tier', async () => {
    const fake = fakeSource();
    const { container } = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    __reportImageBoxForTest(1200);
    await fake.fail(1440, 'derive failed');
    expect(container.querySelector('img')?.getAttribute('src')).toBe('blob:tier-720');
    expect(container.querySelector('[data-streamdown-image-error]')).toBeNull();
    expect(reportFrontendDiagnostic).toHaveBeenCalledWith('Markdown image: a sharper variant failed to load', 'derive failed');
    expect(fake.retained(1440)).toBe(0);
    // The refused tier is not asked for again; the next one up is.
    __reportImageBoxForTest(1300);
    expect(fake.tiers()).toEqual([720, 1440]);
    __reportImageBoxForTest(2000);
    expect(fake.tiers()).toEqual([720, 1440, 2160]);
  });

  it('reports a decode failure in its own chip, or hands it to the host that asked', async () => {
    const fake = fakeSource();
    const own = mount(fake.source);
    __reportImageBoxForTest(700);
    await fake.settle(720);
    own.container.querySelector('img')!.dispatchEvent(new Event('error'));
    await waitFor(() => expect(own.container.querySelector('[data-streamdown-image-error]')).not.toBeNull());
    expect(own.container.textContent).toContain('(cannot decode)');
    own.unmount();

    const handed = vi.fn();
    const hosted = mount(fake.source, { ondecodeerror: handed });
    hosted.container.querySelector('img')!.dispatchEvent(new Event('error'));
    expect(handed).toHaveBeenCalledTimes(1);
  });
});
