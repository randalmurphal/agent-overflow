import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import '../../../app.css';
import ReviewDiffBody from './ReviewDiffBody.svelte';
import { createReviewDiffMedia, type ImageHandle, type ImageRowState, type ImageSideName, type ReviewDiffMedia, type ReviewMediaHost } from '../../stores/reviewDiffMedia.svelte';
import { DiffFilesHandle } from '../../stores/reviewDiffFiles';
import type { DiffReviewComment } from '../../types/models';
import { parseReviewFiles, type ReviewFile } from '../../utils/patchStore';
import { imageRowHeight } from '../../utils/reviewImageRows';
import { waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { reviewThreadRowSnippet } from '../../../test/helpers/reviewThreadRowSnippet.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// The diff list's image rows in a real layout: a change's two sides beside
// each other in split view and stacked in unified, each at the height the
// list reserved, so bytes arriving move no row.

const VIEWPORT_PX = 500;

function textFile(path: string, lines: number): string[] {
  const out = [`diff --git a/${path} b/${path}`, 'new file mode 100644', '--- /dev/null', `+++ b/${path}`, `@@ -0,0 +1,${lines} @@`];
  for (let line = 1; line <= lines; line += 1) out.push(`+${path}:${line}`);
  return out;
}

const PATCH = [
  ...textFile('src/a.ts', 40),
  'diff --git a/img/logo.png b/img/logo.png',
  'index 1111111..2222222 100644',
  'Binary files a/img/logo.png and b/img/logo.png differ',
  ...textFile('src/b.ts', 120),
  '',
].join('\n');

function png(width: number, height: number, color: string): string {
  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext('2d')!;
  context.fillStyle = color;
  context.fillRect(0, 0, width, height);
  return canvas.toDataURL('image/png');
}

type TestHandle = ImageHandle & { state: ImageRowState };

// A media whose rows' states the test sets: the handle is a reactive
// record, so the row repaints as the test writes it.
function fakeMedia() {
  const handles = new Map<string, TestHandle>();
  const pixels = new SvelteMap<string, { width: number; height: number }>();
  const media: ReviewDiffMedia = {
    setTarget() {},
    identity: 'test-open',
    requestSizes() {},
    sizes: () => null,
    readsSizes: false,
    image(file: ReviewFile, sides: readonly ImageSideName[]) {
      const handle = rawProps({ state: { kind: 'loading', sides } as ImageRowState, retry() {}, release() {} });
      handles.set(file.path, handle);
      return handle;
    },
    pixels: (path, side) => pixels.get(`${side}:${path}`) ?? null,
    rememberPixels(path, side, width, height) {
      pixels.set(`${side}:${path}`, { width, height });
    },
  };
  return { media, handles };
}

const mounted: { app: object; host: HTMLElement; files: ReviewFile[] }[] = [];

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

afterEach(() => {
  for (const { app, host, files } of mounted.splice(0)) {
    unmount(app);
    host.remove();
    files[0]?.body.segments[0]?.store.dispose();
  }
});

async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}:${scrollEl.querySelectorAll('[data-testid="review-line-block"]').length}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable review geometry');
}

async function mountImages(viewMode: 'stacked' | 'split', real?: ReviewDiffMedia) {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:900px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  const files = parseReviewFiles(PATCH);
  const fake = fakeMedia();
  const media = real ?? fake.media;
  const handles = fake.handles;
  const mediaHost: ReviewMediaHost = { media, newSideOnly: false, renderedPaths: new Set(), toggleRendered() {}, reloadDiff() {} };
  const props = rawProps({
    subjectId: 'subject',
    spanOwner: 'thread-1',
    scope: 'workspace',
    files,
    viewMode,
    wordWrap: false,
    collapsedPaths: new Set<string>() as ReadonlySet<string>,
    onToggleCollapsed: () => {},
    drafts: [] as DiffReviewComment[],
    commentThread: reviewThreadRowSnippet(),
    mediaHost,
  });
  const app = mount(ReviewDiffBody, { target: host, props });
  mounted.push({ app, host, files });
  const scrollEl = host.querySelector('[data-testid="review-scroll"]') as HTMLElement;
  await settle(scrollEl);
  return { scrollEl, handles, host };
}

const OLD_URL = png(40, 20, '#c33');
const NEW_URL = png(64, 32, '#3c3');

function show(handle: TestHandle): void {
  handle.state = {
    kind: 'image',
    maxBytes: 25 * 1024 * 1024,
    sides: [
      { side: 'old', size: 1200, overCap: false, url: OLD_URL },
      { side: 'new', size: 3400, overCap: false, url: NEW_URL },
    ],
  };
  flushSync();
}

async function imagesLoaded(host: HTMLElement): Promise<void> {
  await waitFor(() => {
    const images = [...host.querySelectorAll<HTMLImageElement>('[data-testid="review-image"]')];
    return images.length === 2 && images.every((img) => img.complete && img.naturalWidth > 0);
  }, 'the images decoded');
}

describe('a review diff\'s image row', () => {
  it('stacks a change\'s old side above its new in unified view, at the height it reserved, captioned', async () => {
    const { scrollEl, handles, host } = await mountImages('stacked');
    const row = host.querySelector<HTMLElement>('[data-testid="review-image-diff"]')!;
    expect(row.getBoundingClientRect().height).toBe(imageRowHeight(2, false));
    show(handles.get('img/logo.png')!);
    await imagesLoaded(host);
    await settle(scrollEl);
    const [old, neu] = [...host.querySelectorAll<HTMLElement>('[data-testid="review-image-side"]')];
    expect(old.dataset.side).toBe('old');
    expect(neu.dataset.side).toBe('new');
    expect(neu.getBoundingClientRect().top).toBeGreaterThanOrEqual(old.getBoundingClientRect().bottom);
    expect(row.getBoundingClientRect().height).toBe(imageRowHeight(2, false));
    // Both images inside their row.
    for (const side of [old, neu]) expect(side.getBoundingClientRect().bottom).toBeLessThanOrEqual(row.getBoundingClientRect().bottom);
    await waitFor(() => host.querySelectorAll('[data-testid="review-image-caption"]')[1]?.textContent?.includes('64 × 32 px') ?? false, 'the measured caption');
    const captions = [...host.querySelectorAll('[data-testid="review-image-caption"]')].map((caption) => caption.textContent);
    expect(captions).toEqual(['Old · 40 × 20 px · 1.2 KB', 'New · 64 × 32 px · 3.3 KB']);
  });

  it('puts the two sides in two columns in split view', async () => {
    const { scrollEl, handles, host } = await mountImages('split');
    const row = host.querySelector<HTMLElement>('[data-testid="review-image-diff"]')!;
    expect(row.getBoundingClientRect().height).toBe(imageRowHeight(2, true));
    show(handles.get('img/logo.png')!);
    await imagesLoaded(host);
    await settle(scrollEl);
    const [old, neu] = [...host.querySelectorAll<HTMLElement>('[data-testid="review-image-side"]')].map((side) => side.getBoundingClientRect());
    expect(old.top).toBe(neu.top);
    expect(neu.left).toBeGreaterThanOrEqual(old.right);
    expect(Math.abs(old.width - neu.width)).toBeLessThanOrEqual(1);
  });

  it('moves no row when the bytes of an image above the viewport arrive', async () => {
    const { scrollEl, handles, host } = await mountImages('stacked');
    // The image row above the top edge, a line of the next file at the top.
    const row = host.querySelector<HTMLElement>('[data-testid="review-image-diff"]')!;
    scrollEl.scrollTop += row.getBoundingClientRect().bottom - scrollEl.getBoundingClientRect().top + 120;
    await settle(scrollEl);
    expect(row.getBoundingClientRect().bottom).toBeLessThan(scrollEl.getBoundingClientRect().top);
    const lines = [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="review-line-block"]')];
    const before = { scrollTop: scrollEl.scrollTop, tops: lines.map((line) => line.getBoundingClientRect().top) };
    show(handles.get('img/logo.png')!);
    await imagesLoaded(host);
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBe(before.scrollTop);
    expect(lines.map((line) => line.getBoundingClientRect().top)).toEqual(before.tops);
  });

  it('reads a row mounted before its open was attached once the open arrives', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => ({
      old: { exists: true, size: 30 * 1024 * 1024, overCap: true },
      new: { exists: true, size: 30 * 1024 * 1024, overCap: true },
      maxBytes: 25 * 1024 * 1024,
    }));
    const media = createReviewDiffMedia();
    const { host } = await mountImages('stacked', media);
    expect(host.querySelector('[data-testid="review-image-diff"]')).not.toBeNull();
    expect(read).not.toHaveBeenCalled();
    media.setTarget({ kind: 'git', backend: 'home', workspace: { projectId: 'p1', workspacePath: '' }, files: new DiffFilesHandle('files-1', null) });
    flushSync();
    await waitFor(() => host.querySelectorAll('[data-testid="review-image-over-cap"]').length === 2, 'the row read under the open');
    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0][1]).toBe('files-1');
    media.setTarget(null);
  });
});
