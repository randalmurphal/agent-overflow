import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { mockLocalImage } from '../../test/mocks/attachmentTransfer';
import { TransferUnavailableError } from '../transport/attachmentTransfer';
import { getPinnedBackend } from '../transport/backends';
import {
  acquireLocalImage,
  fetchLocalImageBytes,
  localImageCacheKey,
  localImageFailureReason,
} from './localImageCache';
import { __resetMediaBlobCacheForTest } from './mediaBlobCache';

// A reply whose ticket the route has never seen, so presenting it is the
// same 404 a spent or expired ticket answers.
function spentReply() {
  return {
    url: '/attachments/image/gone?ticket=never-minted',
    mimeType: 'image/png', width: 0, height: 0, originalWidth: 0, originalHeight: 0, originalBytes: 3, derived: false,
  };
}

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

  it("asks the thread's computer once at the tier, then spends the ticket there, for two mounts", async () => {
    const pins: Array<string | null> = [];
    const rpc = mockLocalImage(() => {
      pins.push(getPinnedBackend());
      // Untyped on the wire, so the Blob's type can only come from the RPC.
      return { blob: new Blob(['png']), mimeType: 'image/png', width: 640, height: 480 };
    });
    const fetched = vi.spyOn(globalThis, 'fetch');
    const a = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720);
    const b = acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720);
    const image = await a.value;
    expect(await b.value).toBe(image);
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc).toHaveBeenCalledWith('/workspace/shot.png', '/workspace', 720);
    expect(pins).toEqual(['gpu']);
    expect(fetched).toHaveBeenCalledTimes(1);
    expect(String(fetched.mock.calls[0]![0])).toMatch(/^\/backend\/gpu\/attachments\/image\/[\w-]+\?ticket=/);
    expect(image).toMatchObject({ url: 'blob:3', mimeType: 'image/png', width: 640, height: 480 });
    expect(await image.blob.text()).toBe('png');
    expect(image.blob.type).toBe('image/png');
    a.release();
    b.release();
  });

  it('keeps each tier its own entry, and the tierless key names the image', async () => {
    const rpc = mockLocalImage((_path, _workspace, maxWidth) => ({ width: maxWidth || 2000, height: 100 }));
    const small = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value;
    const large = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 1440).value;
    const original = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 0).value;
    await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value;
    expect(rpc.mock.calls.map((call) => call[2])).toEqual([720, 1440, 0]);
    expect([small.width, large.width, original.width]).toEqual([720, 1440, 2000]);
    expect(localImageCacheKey('gpu', '/workspace/shot.png', '/workspace'))
      .toBe(JSON.stringify(['local', 'gpu', '/workspace', '/workspace/shot.png']));
  });

  it("carries the backend's description of what it served", async () => {
    mockLocalImage(() => ({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, originalBytes: 5217, derived: true,
    }));
    const image = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 320).value;
    expect(image).toMatchObject({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, originalBytes: 5217, derived: true,
    });
  });

  it('keys by computer and workspace: the same path elsewhere is another file', async () => {
    const rpc = mockLocalImage();
    await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value;
    await acquireLocalImage('laptop', '/workspace/shot.png', '/workspace', 720).value;
    await acquireLocalImage('gpu', '/workspace/shot.png', '/other', 720).value;
    expect(rpc).toHaveBeenCalledTimes(3);
  });

  it('serves SVG as a data URL, never a same-origin blob URL', async () => {
    mockLocalImage(() => ({ blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }) }));
    const image = await acquireLocalImage('gpu', '/workspace/d.svg', '/workspace', 720).value;
    expect(image.url).toBe('data:image/svg+xml;base64,PHN2Zy8+');
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('does not memoize a failure', async () => {
    const rpc = mockLocalImage(() => {
      throw new Error('load local image: file not found: /workspace/shot.png: no such file');
    });
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value).rejects.toThrow('not found');
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value).rejects.toThrow('not found');
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('mints once more when the ticket was already spent, and paints the second', async () => {
    const good = mockLocalImage(() => ({ width: 64, height: 32 }));
    const rpc = setBindingMock('GetLocalImage', async (path: string, workspace: string, maxWidth: number) =>
      rpc.mock.calls.length === 1 ? spentReply() : await good(path, workspace, maxWidth),
    );
    const image = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value;
    expect(image.width).toBe(64);
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(rpc.mock.calls.map((call) => call[2])).toEqual([720, 720]);
  });

  it('reports a second spent ticket rather than looping, and a mount after it resolves again', async () => {
    mockLocalImage();
    const rpc = setBindingMock('GetLocalImage', async () => spentReply());
    const failed = await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value.catch((err: unknown) => err);
    expect(failed).toBeInstanceOf(TransferUnavailableError);
    expect((failed as Error).message).toBe('Could not load image: this transfer is no longer available. Try again.');
    expect(rpc).toHaveBeenCalledTimes(2);
    await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value).rejects.toThrow('no longer available');
    expect(rpc).toHaveBeenCalledTimes(4);
  });

  it('passes any other refusal through without minting again', async () => {
    mockLocalImage();
    const rpc = setBindingMock('GetLocalImage', async () => ({ ...spentReply(), url: '/attachments/image/x?ticket=t' }));
    const realFetch = globalThis.fetch;
    vi.stubGlobal('fetch', async () => new Response('route refused', { status: 400 }));
    try {
      await expect(acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 720).value)
        .rejects.toThrow('Could not load image: route refused');
      expect(rpc).toHaveBeenCalledTimes(1);
    } finally {
      vi.unstubAllGlobals();
      expect(globalThis.fetch).toBe(realFetch);
    }
  });
});

describe('the local image original', () => {
  afterEach(() => {
    __resetMediaBlobCacheForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it("asks for the file itself on the thread's computer and types it by the RPC", async () => {
    const pins: Array<string | null> = [];
    const rpc = mockLocalImage(() => {
      pins.push(getPinnedBackend());
      return { blob: new Blob(['full']), mimeType: 'image/webp', width: 4000, height: 3000 };
    });
    const blob = await fetchLocalImageBytes('gpu', '/workspace/shot.webp', '/workspace', 0);
    expect(rpc).toHaveBeenCalledWith('/workspace/shot.webp', '/workspace', 0);
    expect(pins).toEqual(['gpu']);
    expect(blob.type).toBe('image/webp');
    expect(await blob.text()).toBe('full');
  });

  it('is never cached: each call fetches, and the timeline cache stays empty', async () => {
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:x');
    const rpc = mockLocalImage();
    await fetchLocalImageBytes('gpu', '/workspace/shot.png', '/workspace', 0);
    await fetchLocalImageBytes('gpu', '/workspace/shot.png', '/workspace', 0);
    expect(rpc).toHaveBeenCalledTimes(2);
    // The tier-0 entry a host would read is still a miss.
    await acquireLocalImage('gpu', '/workspace/shot.png', '/workspace', 0).value;
    expect(rpc).toHaveBeenCalledTimes(3);
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
  });

  it('mints once more on a spent ticket, then reports the second', async () => {
    mockLocalImage();
    const rpc = setBindingMock('GetLocalImage', async () => spentReply());
    await expect(fetchLocalImageBytes('gpu', '/workspace/shot.png', '/workspace', 0))
      .rejects.toBeInstanceOf(TransferUnavailableError);
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('stops at an abort instead of minting again', async () => {
    mockLocalImage();
    const controller = new AbortController();
    const rpc = setBindingMock('GetLocalImage', async () => {
      controller.abort();
      return spentReply();
    });
    await expect(fetchLocalImageBytes('gpu', '/workspace/shot.png', '/workspace', 0, controller.signal))
      .rejects.toBe(controller.signal.reason);
    expect(rpc).toHaveBeenCalledTimes(1);
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
