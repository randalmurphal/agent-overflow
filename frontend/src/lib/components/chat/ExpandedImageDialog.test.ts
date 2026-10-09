import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ExpandedImageDialog from './ExpandedImageDialog.svelte';
import type { ExpandedImagePreview, ImagePreviewItem } from '../../utils/attachmentPreview.svelte';

type Deferred = { promise: Promise<Blob>; resolve: (blob: Blob) => void; reject: (err: unknown) => void };

function deferred(): Deferred {
  let resolve!: (blob: Blob) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<Blob>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function item(id: string, overrides: Partial<ImagePreviewItem> = {}): ImagePreviewItem {
  return {
    id,
    filename: `${id}.png`,
    mimeType: 'image/png',
    url: `blob:preview-${id}`,
    width: 4000,
    height: 3000,
    originalBytes: 12_400_000,
    menuTag: { 'data-image-menu': 'attachment', 'data-image-menu-id': id },
    ...overrides,
  };
}

function open(images: ImagePreviewItem[], index = 0) {
  const onClose = vi.fn();
  const preview: ExpandedImagePreview = { images, index };
  const rendered = render(ExpandedImageDialog, { props: { preview, onClose } });
  const dialog = rendered.container.querySelector<HTMLElement>('[role="dialog"]')!;
  const canvas = rendered.container.querySelector<HTMLElement>('[data-lightbox-canvas]')!;
  canvas.setPointerCapture = () => {};
  canvas.releasePointerCapture = () => {};
  canvas.hasPointerCapture = () => false;
  return { ...rendered, onClose, dialog, canvas, picture: () => rendered.container.querySelector<HTMLImageElement>('[data-lightbox-picture]') };
}

const created: string[] = [];
const revoked: string[] = [];

describe('ExpandedImageDialog', () => {
  beforeEach(() => {
    created.length = 0;
    revoked.length = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
      const url = `blob:original-${blob instanceof Blob ? blob.size : 0}-${created.length}`;
      created.push(url);
      return url;
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation((url: string) => { revoked.push(url); });
    // happy-dom lays nothing out; the canvas is 1000x800 for the fit math.
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => new DOMRect(0, 0, 1000, 800));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('paints the preview at the original size, says it is loading with the byte count, then swaps in the original', async () => {
    const original = deferred();
    const first = item('a', { original: vi.fn(() => original.promise) });
    const { container, picture } = open([first]);

    const img = picture()!;
    expect(img.getAttribute('src')).toBe('blob:preview-a');
    expect(img.getAttribute('width')).toBe('4000');
    expect(img.getAttribute('height')).toBe('3000');
    expect(img.hasAttribute('data-lightbox-original')).toBe(false);
    expect(container.querySelector('[data-lightbox-loading]')?.textContent).toContain('Loading full size (12.4 MB)');

    original.resolve(new Blob([new Uint8Array(12)], { type: 'image/png' }));
    await waitFor(() => expect(picture()!.hasAttribute('data-lightbox-original')).toBe(true));
    expect(picture()!.getAttribute('src')).toBe(created[0]);
    expect(picture()!.getAttribute('width')).toBe('4000');
    expect(container.querySelector('[data-lightbox-loading]')).toBeNull();
  });

  it('fetches nothing for an image whose preview is already the original', () => {
    const { container } = open([item('a')]);
    expect(container.querySelector('[data-lightbox-loading]')).toBeNull();
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('shows the reason when the original cannot be loaded and keeps the preview painted', async () => {
    const original = deferred();
    const { container, picture } = open([item('a', { original: () => original.promise })]);
    original.reject(new Error('Could not load image: this transfer is no longer available. Try again.'));
    await waitFor(() => expect(container.querySelector('[data-lightbox-failed]')).not.toBeNull());
    expect(container.querySelector('[data-lightbox-failed]')?.textContent).toContain('this transfer is no longer available');
    expect(picture()!.getAttribute('src')).toBe('blob:preview-a');
    expect(container.querySelector('[data-lightbox-loading]')).toBeNull();
  });

  it('aborts the fetch and revokes what it holds when it closes', async () => {
    const pending = deferred();
    let signal: AbortSignal | undefined;
    const a = item('a', { original: (s) => { signal = s; return pending.promise; } });
    const ready = deferred();
    const b = item('b', { original: () => ready.promise });
    const { unmount, dialog, picture } = open([b, a]);
    ready.resolve(new Blob([new Uint8Array(3)]));
    await waitFor(() => expect(picture()!.hasAttribute('data-lightbox-original')).toBe(true));

    await fireEvent.keyDown(dialog, { key: 'ArrowRight' });
    await waitFor(() => expect(signal).toBeDefined());
    expect(signal!.aborted).toBe(false);
    unmount();
    expect(signal!.aborted).toBe(true);
    expect(revoked).toEqual([created[0]]);
  });

  it('moves between images with the arrows, aborting the one left behind and not refetching one seen', async () => {
    const loads: Record<string, Deferred> = { a: deferred(), b: deferred() };
    const signals: Record<string, AbortSignal[]> = { a: [], b: [] };
    const images = ['a', 'b'].map((id) => item(id, { original: (s) => { signals[id].push(s); return loads[id].promise; } }));
    const { dialog, picture } = open(images);
    expect(picture()!.getAttribute('alt')).toBe('a.png');
    await waitFor(() => expect(signals.a).toHaveLength(1));

    await fireEvent.keyDown(dialog, { key: 'ArrowRight' });
    expect(picture()!.getAttribute('alt')).toBe('b.png');
    expect(signals.a[0].aborted).toBe(true);
    await waitFor(() => expect(signals.b).toHaveLength(1));
    loads.b.resolve(new Blob([new Uint8Array(5)]));
    await waitFor(() => expect(picture()!.hasAttribute('data-lightbox-original')).toBe(true));

    await fireEvent.keyDown(dialog, { key: 'ArrowLeft' });
    expect(picture()!.getAttribute('alt')).toBe('a.png');
    // The aborted first fetch is retried; the finished one is kept.
    await waitFor(() => expect(signals.a).toHaveLength(2));
    await fireEvent.keyDown(dialog, { key: 'ArrowRight' });
    expect(signals.b).toHaveLength(1);
    expect(picture()!.getAttribute('src')).toBe(created[0]);
  });

  it('gives the arrows to panning once the person has zoomed, and Escape closes', async () => {
    const { dialog, onClose, picture } = open([item('a'), item('b')]);
    await fireEvent.keyDown(dialog, { key: '+' });
    await fireEvent.keyDown(dialog, { key: 'ArrowRight' });
    expect(picture()!.getAttribute('alt')).toBe('a.png');
    await fireEvent.keyDown(dialog, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('closes on a still press on the backdrop, not on the picture and not after a drag', async () => {
    const { canvas, onClose, picture } = open([item('a')]);
    const down = (target: Element, x: number, y: number) => fireEvent.pointerDown(target, { pointerId: 1, pointerType: 'mouse', button: 0, clientX: x, clientY: y });
    const up = (target: Element, x: number, y: number) => fireEvent.pointerUp(target, { pointerId: 1, pointerType: 'mouse', button: 0, clientX: x, clientY: y });
    const click = (target: Element, x: number, y: number) => fireEvent.click(target, { clientX: x, clientY: y });

    await down(picture()!, 500, 400);
    await up(canvas, 500, 400);
    await click(canvas, 500, 400);
    expect(onClose).not.toHaveBeenCalled();

    await down(canvas, 50, 50);
    await fireEvent.pointerMove(canvas, { pointerId: 1, clientX: 90, clientY: 50 });
    await up(canvas, 90, 50);
    await click(canvas, 90, 50);
    expect(onClose).not.toHaveBeenCalled();

    await down(canvas, 50, 50);
    await up(canvas, 51, 50);
    await click(canvas, 51, 50);
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('carries the image menu tag on the picture', () => {
    const { picture } = open([item('a')]);
    expect(picture()!.getAttribute('data-image-menu')).toBe('attachment');
    expect(picture()!.getAttribute('data-image-menu-id')).toBe('a');
  });
});

describe('ExpandedImageDialog with an unknown original size', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => 'blob:original');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => new DOMRect(0, 0, 1000, 800));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('sizes the box from the thumbnail, then from the original once it decodes', async () => {
    const original = deferred();
    const { picture } = open([item('a', { width: 0, height: 0, original: () => original.promise })]);
    const img = picture()!;
    expect(img.hasAttribute('width')).toBe(false);

    Object.defineProperty(img, 'naturalWidth', { value: 256, configurable: true });
    Object.defineProperty(img, 'naturalHeight', { value: 170, configurable: true });
    await fireEvent.load(img);
    expect(img.getAttribute('width')).toBe('256');
    expect(img.getAttribute('height')).toBe('170');

    original.resolve(new Blob([new Uint8Array(9)]));
    await waitFor(() => expect(picture()!.hasAttribute('data-lightbox-original')).toBe(true));
    const swapped = picture()!;
    Object.defineProperty(swapped, 'naturalWidth', { value: 3000, configurable: true });
    Object.defineProperty(swapped, 'naturalHeight', { value: 2000, configurable: true });
    await fireEvent.load(swapped);
    expect(swapped.getAttribute('width')).toBe('3000');
    expect(swapped.getAttribute('height')).toBe('2000');
  });
});
