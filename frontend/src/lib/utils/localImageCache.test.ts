import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { mockLocalImage } from '../../test/mocks/attachmentTransfer';
import { getPinnedBackend } from '../transport/backends';
import { acquireLocalImage, localImageFailureReason } from './localImageCache';
import { __resetMediaBlobCacheForTest } from './mediaBlobCache';

describe('the local image cache', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => `blob:${blob instanceof Blob ? blob.size : 0}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it("asks the thread's computer once for the file itself, then spends the ticket there, for two mounts", async () => {
    const pins: Array<string | null> = [];
    const rpc = mockLocalImage(() => {
      pins.push(getPinnedBackend());
      // Untyped on the wire, so the Blob's type can only come from the RPC.
      return { blob: new Blob(['png']), mimeType: 'image/png', width: 640, height: 480 };
    });
    const fetched = vi.spyOn(globalThis, 'fetch');
    const a = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace');
    const b = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace');
    const image = await a.value;
    expect(await b.value).toBe(image);
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc).toHaveBeenCalledWith('/workspace/shot.png', '/workspace', 0);
    expect(pins).toEqual(['gpu']);
    expect(fetched).toHaveBeenCalledTimes(1);
    expect(String(fetched.mock.calls[0]![0])).toMatch(/^\/backend\/gpu\/attachments\/image\/[\w-]+\?ticket=/);
    expect(image).toMatchObject({ url: 'blob:3', mimeType: 'image/png', width: 640, height: 480 });
    expect(await image.blob.text()).toBe('png');
    expect(image.blob.type).toBe('image/png');
    a.release();
    b.release();
  });

  it("carries the backend's description of what it served", async () => {
    mockLocalImage(() => ({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, originalBytes: 5217, derived: true,
    }));
    const image = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value;
    expect(image).toMatchObject({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, originalBytes: 5217, derived: true,
    });
  });

  it('keys by computer and workspace: the same path elsewhere is another file', async () => {
    const rpc = mockLocalImage();
    await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value;
    await acquireLocalImage('laptop', '/workspace/shot.png', '/workspace').value;
    await acquireLocalImage('gpu', '/workspace/shot.png', '/other').value;
    expect(rpc).toHaveBeenCalledTimes(3);
  });

  it('serves SVG as a data URL, never a same-origin blob URL', async () => {
    mockLocalImage(() => ({ blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }) }));
    const image = await acquireLocalImage('gpu', '/workspace/d.svg', '/workspace').value;
    expect(image.url).toBe('data:image/svg+xml;base64,PHN2Zy8+');
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('does not memoize a failure', async () => {
    const rpc = mockLocalImage(() => {
      throw new Error('load local image: file not found: /workspace/shot.png: no such file');
    });
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value).rejects.toThrow('not found');
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value).rejects.toThrow('not found');
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('reports a refused transfer, so a mount after it resolves again', async () => {
    mockLocalImage();
    const rpc = setBindingMock('GetLocalImage', async () => ({
      url: '/attachments/image/gone?ticket=never-minted',
      mimeType: 'image/png', width: 0, height: 0, originalWidth: 0, originalHeight: 0, originalBytes: 3, derived: false,
    }));
    const failed = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value;
    await expect(failed).rejects.toThrow('Could not load image: this transfer is no longer available. Try again.');
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace').value).rejects.toThrow('no longer available');
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
