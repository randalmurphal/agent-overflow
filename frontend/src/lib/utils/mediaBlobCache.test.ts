import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetMediaBlobCacheForTest,
  __setMediaBlobCacheLimitsForTest,
  acquireMediaBlob,
  rememberDecodedSize,
  type MediaBytes,
} from './mediaBlobCache';

function bytes(text: string, url = `blob:${text}`): MediaBytes {
  return { url, mimeType: 'image/png', blob: new Blob([text]) };
}

/** A load whose settlement the test controls. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('the media blob cache', () => {
  beforeEach(() => {
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    vi.restoreAllMocks();
  });

  it('runs one load for two acquires and answers a later acquire synchronously', async () => {
    const load = vi.fn(async () => bytes('a'));
    const first = acquireMediaBlob('k', load);
    const second = acquireMediaBlob('k', load);
    expect(first.settled).toBeUndefined();
    expect(await first.value).toBe(await second.value);
    expect(load).toHaveBeenCalledTimes(1);

    // The remount case: the bytes are there before the effect returns.
    const third = acquireMediaBlob('k', load);
    expect(third.settled).toBe(await first.value);
    expect(load).toHaveBeenCalledTimes(1);
    first.release();
    second.release();
    third.release();
  });

  it('does not memoize a failure', async () => {
    const load = vi.fn(async () => {
      throw new Error('nope');
    });
    await expect(acquireMediaBlob('k', load).value).rejects.toThrow('nope');
    await expect(acquireMediaBlob('k', load).value).rejects.toThrow('nope');
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('evicts least recently used first, by bytes, and never a retained entry', async () => {
    __setMediaBlobCacheLimitsForTest({ maxBytes: 10 });
    const held = acquireMediaBlob('held', async () => bytes('12345'));
    await held.value;
    const dropped = acquireMediaBlob('dropped', async () => bytes('abcde'));
    await dropped.value;
    dropped.release();

    // 15 bytes against a budget of 10: the oldest unretained entry goes.
    await acquireMediaBlob('third', async () => bytes('vwxyz')).value;

    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:abcde');
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:12345');
    // A dropped key loads again; a surviving one does not.
    const reload = vi.fn(async () => bytes('abcde'));
    await acquireMediaBlob('dropped', reload).value;
    expect(reload).toHaveBeenCalledTimes(1);
    expect(acquireMediaBlob('held', reload).settled).toBeDefined();
    expect(reload).toHaveBeenCalledTimes(1);
    held.release();
  });

  it('evicts on entry count whatever the entries weigh', async () => {
    __setMediaBlobCacheLimitsForTest({ maxEntries: 2 });
    for (const name of ['a', 'b', 'c']) {
      const handle = acquireMediaBlob(name, async () => bytes(name));
      await handle.value;
      handle.release();
    }
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:a');
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:b');
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:c');
  });

  it('counts a data URL beside its bytes and never revokes one', async () => {
    // 5 bytes of blob plus a 34-character data URL: 39 against a budget of
    // 40, so a second such entry evicts the first.
    __setMediaBlobCacheLimitsForTest({ maxBytes: 40 });
    const dataUrl = 'data:image/svg+xml;base64,PHN2Zy8+';
    expect(dataUrl).toHaveLength(34);
    const first = acquireMediaBlob('svg-a', async () => bytes('12345', dataUrl));
    await first.value;
    first.release();
    const second = acquireMediaBlob('svg-b', async () => bytes('abcde', dataUrl));
    await second.value;
    second.release();

    const reload = vi.fn(async () => bytes('12345', dataUrl));
    await acquireMediaBlob('svg-a', reload).value;
    expect(reload).toHaveBeenCalledTimes(1);
    expect(URL.revokeObjectURL).not.toHaveBeenCalled();
  });

  it('revokes a value that lands after its entry was dropped, rather than caching it', async () => {
    const pending = deferred<MediaBytes>();
    const handle = acquireMediaBlob('late', () => pending.promise);
    __resetMediaBlobCacheForTest();
    pending.resolve(bytes('late'));
    await handle.value;
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:late');
    expect(acquireMediaBlob('late', async () => bytes('again')).settled).toBeUndefined();
  });

  it('releases a holder once, however many times release is called', async () => {
    __setMediaBlobCacheLimitsForTest({ maxBytes: 1 });
    const a = acquireMediaBlob('a', async () => bytes('aa'));
    await a.value;
    const b = acquireMediaBlob('a', async () => bytes('aa'));
    a.release();
    a.release();
    // b still retains the entry, so the next entry's eviction pass skips it.
    const c = acquireMediaBlob('c', async () => bytes('cc'));
    await c.value;
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:aa');
    b.release();
    c.release();
  });

  it('remembers a decoded size only while the size is unknown', () => {
    const size = { width: 0, height: 0 };
    const img = { naturalWidth: 120, naturalHeight: 80 } as HTMLImageElement;
    rememberDecodedSize(size, img);
    expect(size).toEqual({ width: 120, height: 80 });
    rememberDecodedSize(size, { naturalWidth: 1, naturalHeight: 1 } as HTMLImageElement);
    expect(size).toEqual({ width: 120, height: 80 });
    const unknown = { width: 0, height: 0 };
    rememberDecodedSize(unknown, { naturalWidth: 0, naturalHeight: 0 } as HTMLImageElement);
    expect(unknown).toEqual({ width: 0, height: 0 });
  });
});
