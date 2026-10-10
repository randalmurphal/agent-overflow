import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { TransferUnavailableError } from '../transport/attachmentTransfer';
import {
  acquireForgeAttachment,
  acquirePaintedForgeAttachment,
  fetchForgeAttachmentBytes,
  forgeAttachmentCacheKey,
} from './forgeAttachmentCache';
import { __resetImageTiersForTest, rememberImageTier } from './imageTiers';
import { __resetMediaBlobCacheForTest } from './mediaBlobCache';
import type { PRRef } from './prReference';

const responses: Response[] = [];
const fetchCalls: string[] = [];

vi.mock('../transport/deviceSession', () => ({
  fetchPairedComputer: async (_backend: string, _fetcher: unknown, input: string) => {
    fetchCalls.push(input);
    const next = responses.shift();
    if (!next) throw new Error(`no staged response for ${input}`);
    return next;
  },
}));

vi.mock('../transport/backends', () => ({
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));

const PR: PRRef = { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 };
const HREF = '/uploads/0123456789abcdef0123456789abcdef/shot.png';

function stageBody(body: string, init: ResponseInit = {}): void {
  responses.push(new Response(body, { status: 200, ...init }));
}

function attachment(overrides: Record<string, unknown> = {}) {
  return {
    url: '/attachments/forge/tok1?ticket=a',
    mimeType: 'image/png',
    kind: 'image',
    sizeBytes: 12,
    filename: 'shot.png',
    width: 0,
    height: 0,
    originalWidth: 0,
    originalHeight: 0,
    derived: false,
    ...overrides,
  };
}

describe('the forge attachment cache', () => {
  beforeEach(() => {
    responses.length = 0;
    fetchCalls.length = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:forge-${fetchCalls.length}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    __resetImageTiersForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('spends one RPC and one ticket for two mounts of the same body', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('bytes');

    const a = acquireForgeAttachment('gpu', PR, HREF, 0);
    const b = acquireForgeAttachment('gpu', PR, HREF, 0);
    expect(await a.value).toEqual(await b.value);
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(fetchCalls).toHaveLength(1);
    a.release();
    b.release();
  });

  it('keys by computer, so two machines on one PR do not share bytes', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('one');
    stageBody('two');

    await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    await acquireForgeAttachment('laptop', PR, HREF, 0).value;
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('mints again once when the ticket has already been spent', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () =>
      attachment({ url: `/attachments/forge/tok${rpc.mock.calls.length}?ticket=a` }),
    );
    responses.push(new Response('gone', { status: 404 }));
    stageBody('bytes');

    const resolved = await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(resolved.mimeType).toBe('image/png');
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(fetchCalls).toHaveLength(2);
  });

  it('reports a second miss rather than looping, in words that fit any attachment', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    responses.push(new Response('gone', { status: 404 }));
    responses.push(new Response('gone', { status: 404 }));

    const failed = await acquireForgeAttachment('gpu', PR, HREF, 0).value.catch((err: unknown) => err);
    expect(failed).toBeInstanceOf(TransferUnavailableError);
    expect((failed as Error).message).toBe('This attachment transfer is no longer available. Try again.');
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(fetchCalls).toHaveLength(2);
  });

  it('surfaces any other refusal line without minting again', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    responses.push(new Response('relay refused the transfer', { status: 502 }));

    await expect(acquireForgeAttachment('gpu', PR, HREF, 0).value).rejects.toThrow('relay refused the transfer');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('drops a failed promise so the next mount tries again', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => {
      throw new Error("glab is not authenticated");
    });

    await expect(acquireForgeAttachment('gpu', PR, HREF, 0).value).rejects.toThrow('authenticated');
    await expect(acquireForgeAttachment('gpu', PR, HREF, 0).value).rejects.toThrow('authenticated');
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  // The page's CSP (connect-src 'self') refuses a fetch of a blob: or data:
  // URL, so a copy cannot read the bytes back from the URL it painted.
  it('hands back the bytes it read, so a copy needs no second request', async () => {
    setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('png-bytes', { headers: { 'content-type': 'image/png' } });
    const raster = await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(await raster.blob.text()).toBe('png-bytes');

    setBindingMock('FetchForgeAttachment', async () =>
      attachment({ mimeType: 'image/svg+xml', filename: 'diagram.svg' }),
    );
    stageBody('<svg/>');
    const svg = await acquireForgeAttachment('gpu', PR, `${HREF}?svg`, 0).value;
    expect(await svg.blob.text()).toBe('<svg/>');
    expect(svg.blob.type).toBe('image/svg+xml');
  });

  it('serves SVG as a data URL, never a same-origin blob URL', async () => {
    setBindingMock('FetchForgeAttachment', async () =>
      attachment({ mimeType: 'image/svg+xml', filename: 'diagram.svg' }),
    );
    stageBody('<svg/>');

    const resolved = await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(resolved.url.startsWith('data:image/svg+xml;base64,')).toBe(true);
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('carries the declared pixel size through, so a host reserves the box before the decode', async () => {
    setBindingMock('FetchForgeAttachment', async () => attachment({ width: 640, height: 480 }));
    stageBody('bytes');
    const resolved = await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(resolved.width).toBe(640);
    expect(resolved.height).toBe(480);
  });

  it('asks at the tier and keeps each tier its own entry', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async (_pr: unknown, _href: string, maxWidth: number) =>
      attachment({ width: maxWidth, height: 10, originalWidth: 3000, originalHeight: 40, derived: true }),
    );
    stageBody('small');
    stageBody('large');

    const small = await acquireForgeAttachment('gpu', PR, HREF, 720).value;
    const large = await acquireForgeAttachment('gpu', PR, HREF, 1440).value;
    await acquireForgeAttachment('gpu', PR, HREF, 720).value;
    expect(rpc.mock.calls.map((call) => call[2])).toEqual([720, 1440]);
    expect([small.width, large.width]).toEqual([720, 1440]);
    expect(await small.blob.text()).toBe('small');
    expect(forgeAttachmentCacheKey('gpu', PR, HREF)).not.toContain('720');
  });

  it('asks for the original and carries what the backend said it served', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, sizeBytes: 5217, derived: true,
    }));
    stageBody('bytes');
    const resolved = await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(rpc).toHaveBeenCalledWith(expect.anything(), HREF, 0);
    expect(resolved).toMatchObject({
      width: 320, height: 240, originalWidth: 641, originalHeight: 480, sizeBytes: 5217, originalBytes: 5217, derived: true,
    });
  });
});

describe('the forge attachment original', () => {
  beforeEach(() => {
    responses.length = 0;
    fetchCalls.length = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:forge-${fetchCalls.length}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    __resetImageTiersForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('asks for the attachment itself, typed by what the backend classified', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment({ mimeType: 'image/webp' }));
    stageBody('full-size');
    const blob = await fetchForgeAttachmentBytes('gpu', PR, HREF, 0);
    expect(rpc).toHaveBeenCalledWith(expect.anything(), HREF, 0);
    expect(blob.type).toBe('image/webp');
    expect(await blob.text()).toBe('full-size');
  });

  it('is never cached: a second call fetches again and the tier-0 entry stays a miss', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('one');
    stageBody('two');
    stageBody('three');
    await fetchForgeAttachmentBytes('gpu', PR, HREF, 0);
    await fetchForgeAttachmentBytes('gpu', PR, HREF, 0);
    await acquireForgeAttachment('gpu', PR, HREF, 0).value;
    expect(rpc).toHaveBeenCalledTimes(3);
    expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
  });

  it('mints once more on a spent ticket', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    responses.push(new Response('gone', { status: 404 }));
    stageBody('bytes');
    expect(await (await fetchForgeAttachmentBytes('gpu', PR, HREF, 0)).text()).toBe('bytes');
    expect(rpc).toHaveBeenCalledTimes(2);
  });
});

describe('the painted resolution a save or copy reuses', () => {
  beforeEach(() => {
    responses.length = 0;
    fetchCalls.length = 0;
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:forge-${fetchCalls.length}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    __resetImageTiersForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('shares the entry at the tier the page last asked for, so a file is fetched once', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () =>
      attachment({ kind: 'file', mimeType: 'application/pdf', filename: 'report.pdf' }),
    );
    stageBody('pdf');
    const painted = acquireForgeAttachment('gpu', PR, HREF, 1080);
    rememberImageTier(forgeAttachmentCacheKey('gpu', PR, HREF), 1080);
    const saved = acquirePaintedForgeAttachment('gpu', PR, HREF);
    expect(await saved.value).toBe(await painted.value);
    expect(rpc).toHaveBeenCalledTimes(1);
    painted.release();
    saved.release();
  });

  it('asks for the original when nothing on the page has asked', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('bytes');
    await acquirePaintedForgeAttachment('gpu', PR, HREF).value;
    expect(rpc).toHaveBeenCalledWith(expect.anything(), HREF, 0);
  });
});
