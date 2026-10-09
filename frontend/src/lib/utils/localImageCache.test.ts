import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { getPinnedBackend } from '../transport/backends';
import { acquireLocalImage, localImageFailureReason } from './localImageCache';
import { __resetMediaBlobCacheForTest } from './mediaBlobCache';

function reply(overrides: Record<string, unknown> = {}) {
  // "png" as bytes; the backend has already sniffed and validated them.
  return { data: 'cG5n', mimeType: 'image/png', width: 640, height: 480, ...overrides };
}

describe('the local image cache', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob: Blob) => `blob:${blob.size}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it("reads the bytes once, from the thread's computer, for two mounts of the same image", async () => {
    const pins: Array<string | null> = [];
    const rpc = setBindingMock('GetLocalImageData', async () => {
      pins.push(getPinnedBackend());
      return reply();
    });
    const a = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace');
    const b = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace');
    const image = await a.value;
    expect(await b.value).toBe(image);
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc).toHaveBeenCalledWith('/workspace/shot.png', '/workspace');
    expect(pins).toEqual(['gpu']);
    expect(image).toMatchObject({ url: 'blob:3', mimeType: 'image/png', width: 640, height: 480 });
    expect(await image.blob.text()).toBe('png');
    expect(image.blob.type).toBe('image/png');
    a.release();
    b.release();
  });

  it('keys by computer and workspace: the same path elsewhere is another file', async () => {
    const rpc = setBindingMock('GetLocalImageData', async () => reply());
    await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value;
    await acquireLocalImage('laptop', '/workspace/shot.png', '/workspace').value;
    await acquireLocalImage('gpu', '/workspace/shot.png', '/other').value;
    expect(rpc).toHaveBeenCalledTimes(3);
  });

  it('serves SVG as a data URL, never a same-origin blob URL', async () => {
    setBindingMock('GetLocalImageData', async () =>
      reply({ data: 'PHN2Zy8+', mimeType: 'image/svg+xml', width: 0, height: 0 }),
    );
    const image = await acquireLocalImage('gpu', '/workspace/d.svg', '/workspace').value;
    expect(image.url).toBe('data:image/svg+xml;base64,PHN2Zy8+');
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('does not memoize a failure', async () => {
    const rpc = setBindingMock('GetLocalImageData', async () => {
      throw new Error('load local image: file not found: /workspace/shot.png: no such file');
    });
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value).rejects.toThrow('not found');
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value).rejects.toThrow('not found');
    expect(rpc).toHaveBeenCalledTimes(2);
  });
});

describe('localImageFailureReason', () => {
  it('names the reason the backend put between its prefix and the cause', () => {
    expect(localImageFailureReason('load local image: file not found: /w/x.png: open /w/x.png: no such file')).toBe('file not found');
    expect(localImageFailureReason('load local image: not an image: attachment: payload is not an image a browser displays')).toBe('not an image');
    expect(localImageFailureReason('load local image: larger than 25 MiB: /w/x.png: file is too large: exceeds 26214400 bytes')).toBe('larger than 25 MiB');
  });

  it('takes the first clause of any other message, bounded', () => {
    expect(localImageFailureReason('backend is no longer connected')).toBe('backend is no longer connected');
    expect(localImageFailureReason('scope files:read is not granted: ask the owner')).toBe('scope files:read is not granted');
    const long = 'x'.repeat(80);
    expect(localImageFailureReason(long)).toHaveLength(48);
    expect(localImageFailureReason(long).endsWith('…')).toBe(true);
    expect(localImageFailureReason('')).toBe('');
  });
});
