import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import {
  __resetForgeAttachmentCacheForTest,
  acquireForgeAttachment,
} from './forgeAttachmentCache';
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
    __resetForgeAttachmentCacheForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('spends one RPC and one ticket for two mounts of the same body', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => attachment());
    stageBody('bytes');

    const a = acquireForgeAttachment('gpu', PR, HREF);
    const b = acquireForgeAttachment('gpu', PR, HREF);
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

    await acquireForgeAttachment('gpu', PR, HREF).value;
    await acquireForgeAttachment('laptop', PR, HREF).value;
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('mints again once when the ticket has already been spent', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () =>
      attachment({ url: `/attachments/forge/tok${rpc.mock.calls.length}?ticket=a` }),
    );
    responses.push(new Response('gone', { status: 404 }));
    stageBody('bytes');

    const resolved = await acquireForgeAttachment('gpu', PR, HREF).value;
    expect(resolved.mimeType).toBe('image/png');
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(fetchCalls).toHaveLength(2);
  });

  it('reports a second miss rather than looping', async () => {
    setBindingMock('FetchForgeAttachment', async () => attachment());
    responses.push(new Response('gone', { status: 404 }));
    responses.push(new Response('gone', { status: 404 }));

    await expect(acquireForgeAttachment('gpu', PR, HREF).value).rejects.toThrow('no longer available');
  });

  it('surfaces the route refusal a person can act on', async () => {
    setBindingMock('FetchForgeAttachment', async () => attachment());
    responses.push(new Response('attachment is larger than 100 MiB', { status: 413 }));

    await expect(acquireForgeAttachment('gpu', PR, HREF).value).rejects.toThrow('larger than 100 MiB');
  });

  it('drops a failed promise so the next mount tries again', async () => {
    const rpc = setBindingMock('FetchForgeAttachment', async () => {
      throw new Error("glab is not authenticated");
    });

    await expect(acquireForgeAttachment('gpu', PR, HREF).value).rejects.toThrow('authenticated');
    await expect(acquireForgeAttachment('gpu', PR, HREF).value).rejects.toThrow('authenticated');
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('serves SVG as a data URL, never a same-origin blob URL', async () => {
    setBindingMock('FetchForgeAttachment', async () =>
      attachment({ mimeType: 'image/svg+xml', filename: 'diagram.svg' }),
    );
    stageBody('<svg/>');

    const resolved = await acquireForgeAttachment('gpu', PR, HREF).value;
    expect(resolved.url.startsWith('data:image/svg+xml;base64,')).toBe(true);
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it('revokes an evicted entry, and never one a mount still displays', async () => {
    setBindingMock('FetchForgeAttachment', async () =>
      // One entry per call, each claiming half the byte budget so the third
      // forces eviction.
      attachment({ url: `/attachments/forge/t${fetchCalls.length}?ticket=a`, sizeBytes: 40 * 1024 * 1024 }),
    );
    stageBody('a');
    stageBody('b');
    stageBody('c');

    const held = acquireForgeAttachment('gpu', PR, `${HREF}?1`);
    const heldUrl = (await held.value).url;
    const dropped = acquireForgeAttachment('gpu', PR, `${HREF}?2`);
    const droppedUrl = (await dropped.value).url;
    dropped.release();

    await acquireForgeAttachment('gpu', PR, `${HREF}?3`).value;

    expect(URL.revokeObjectURL).toHaveBeenCalledWith(droppedUrl);
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith(heldUrl);
    held.release();
  });
});
