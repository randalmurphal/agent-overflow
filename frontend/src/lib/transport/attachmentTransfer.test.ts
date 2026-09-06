import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { fetchAttachmentBytes, uploadAttachmentBytes } from './attachmentTransfer';
import { __resetHomeEndpointForTest, setHomeEndpoint, storeBackendEndpoint } from './homeEndpoint';

import { noteThread } from './entityIndex';
import { takePinnedBackend } from './backends';

// These tests drive the module against a fetch of their own rather than
// through test/mocks/attachmentTransfer.ts, because what is under test IS
// the request: the URL it goes to, the body it carries, and what it makes
// of a refusal. The shared mock answers those requests; it cannot check
// them.

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47]);

let realFetch: typeof globalThis.fetch;
let requests: Request[];

function answerWith(build: (request: Request) => Response | Promise<Response>): void {
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(
      typeof input === 'string' ? new URL(input, 'http://page.test/').href : input as never,
      init,
    );
    requests.push(request.clone());
    return await build(request);
  }) as typeof globalThis.fetch;
}

beforeEach(() => {
  resetBindingMocks();
  requests = [];
  realFetch = globalThis.fetch;
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

describe('uploadAttachmentBytes', () => {
  it('mints for exactly these bytes and PUTs them to the URL it was given', async () => {
    const mint = setBindingMock(
      'MintAttachmentUploadTicket',
      () => '/attachments/upload?ticket=abc',
    );
    answerWith(() => new Response(JSON.stringify({ id: 'att-1', threadId: 'thr-1' }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
    const file = new File([PNG], 'shot.png', { type: 'image/png' });

    const record = await uploadAttachmentBytes('thr-1', file);

    // Every field the stored row will carry is decided by the MINT. The
    // request contributes the body and nothing else.
    expect(mint).toHaveBeenCalledWith('thr-1', 'shot.png', 'image/png', file.size);
    expect(record).toEqual({ id: 'att-1', threadId: 'thr-1' });

    const sent = requests[0];
    expect(sent?.method).toBe('PUT');
    expect(new URL(sent!.url).pathname + new URL(sent!.url).search)
      .toBe('/attachments/upload?ticket=abc');
    expect(new Uint8Array(await sent!.arrayBuffer())).toEqual(PNG);
  });

  it('asks for a relative URL, never an absolute one', async () => {
    // The SPA is served from three different origins across the boots it
    // supports, and it does not know which one it is in. A minted URL is
    // resolved by the browser against the page; anything this module did
    // to it would be right in at most one of the three.
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    answerWith(() => new Response('{}', { status: 200 }));

    await uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' }));

    expect(new URL(requests[0]!.url).origin).toBe('http://page.test');
  });

  it('sends same-origin credentials, which the --connect stub requires', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    let credentials: RequestCredentials | undefined;
    globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
      credentials = init?.credentials;
      return new Response('{}', { status: 200 });
    }) as typeof globalThis.fetch;

    await uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' }));

    expect(credentials).toBe('same-origin');
  });

  it('reports an over-cap refusal in words a person can act on', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    answerWith(() => new Response('attachment too large', { status: 413 }));

    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(/larger than this backend accepts/);
  });

  it('reports a spent or unknown ticket as something to retry', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    answerWith(() => new Response('404 page not found', { status: 404 }));

    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(/no longer available/);
  });

  it('surfaces a short refusal line and never a long body', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    answerWith(() => new Response('attachment rejected', { status: 400 }));
    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(/attachment rejected/);

    requests = [];
    answerWith(() => new Response('x'.repeat(5000), { status: 400 }));
    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(/^Upload failed \(400\)\.$/);
  });

  it('lets a failed mint through untouched', async () => {
    // A refused mint is an RPC refusal — a missing scope, a thread that is
    // not there — and the transport's own error surface already says so.
    // Wrapping it here would replace a specific message with a vague one.
    setBindingMock('MintAttachmentUploadTicket', () => {
      throw new Error('attachment: payload 99 bytes exceeds limit 10');
    });
    const fetchSpy = vi.fn();
    globalThis.fetch = fetchSpy as unknown as typeof globalThis.fetch;

    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(/exceeds limit/);
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe('fetchAttachmentBytes', () => {
  it('mints for one attachment and returns the body as a Blob', async () => {
    const mint = setBindingMock(
      'MintAttachmentDownloadTicket',
      () => '/attachments/thr-1/att-1?ticket=xyz',
    );
    answerWith(() => new Response(PNG, {
      status: 200,
      headers: { 'Content-Type': 'image/png' },
    }));

    const blob = await fetchAttachmentBytes('thr-1', 'att-1');

    expect(mint).toHaveBeenCalledWith('thr-1', 'att-1');
    expect(blob.type).toBe('image/png');
    expect(new Uint8Array(await blob.arrayBuffer())).toEqual(PNG);
    const sent = requests[0];
    expect(sent?.method).toBe('GET');
    expect(new URL(sent!.url).pathname).toBe('/attachments/thr-1/att-1');
  });

  it('reports a refusal rather than handing back an empty Blob', async () => {
    setBindingMock('MintAttachmentDownloadTicket', () => '/attachments/thr-1/att-1?ticket=xyz');
    answerWith(() => new Response('404 page not found', { status: 404 }));

    await expect(fetchAttachmentBytes('thr-1', 'att-1'))
      .rejects.toThrow(/Could not load image/);
  });
});

// The minted URL stays relative — the backend that mints it does not know
// which origin will present it — so a shell carries it onto the home
// endpoint through the one seam that knows where that is. The ticket in
// the query is the whole admission either way, which is exactly what the
// route's own header argues, and is why dropping the cookie costs
// nothing.
describe('under a shell origin', () => {
  const ENDPOINT = 'https://desk.tail-scale.ts.net:7777';

  beforeEach(() => {
    setHomeEndpoint(ENDPOINT);
  });

  afterEach(() => {
    __resetHomeEndpointForTest();
  });

  it('puts the bytes on the endpoint and presents no cookie', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    const seen: RequestInit[] = [];
    const real = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push({ ...init, ...{ url: String(input) } } as RequestInit & { url: string });
      return new Response(JSON.stringify({ id: 'att-1', threadId: 'thr-1' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }) as typeof globalThis.fetch;
    try {
      await uploadAttachmentBytes('thr-1', new File([PNG], 'shot.png', { type: 'image/png' }));
    } finally {
      globalThis.fetch = real;
    }

    expect((seen[0] as RequestInit & { url: string }).url)
      .toBe(`${ENDPOINT}/attachments/upload?ticket=abc`);
    expect(seen[0].credentials).toBe('omit');
  });

  it('reads the bytes back from the endpoint too', async () => {
    setBindingMock('MintAttachmentDownloadTicket', () => '/attachments/thr-1/att-1?ticket=xyz');
    const seen: string[] = [];
    const real = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      seen.push(String(input));
      return new Response(PNG, { status: 200, headers: { 'Content-Type': 'image/png' } });
    }) as typeof globalThis.fetch;
    try {
      await fetchAttachmentBytes('thr-1', 'att-1');
    } finally {
      globalThis.fetch = real;
    }

    expect(seen[0]).toBe(`${ENDPOINT}/attachments/thr-1/att-1?ticket=xyz`);
  });
});


describe('a thread on another computer', () => {
  afterEach(() => __resetHomeEndpointForTest());

  it.each([false, true])('keeps both uploads and downloads on that computer (phone: %s)', async (phone) => {
    noteThread('remote-thread', 'gpu');
    if (phone) {
      setHomeEndpoint('https://mac.test');
      storeBackendEndpoint('gpu', 'https://gpu.test');
    }
    setBindingMock('MintAttachmentUploadTicket', async () => {
      expect(takePinnedBackend()).toBe('gpu');
      // Changing ownership while the mint is pending must not redirect its bytes.
      noteThread('remote-thread', '');
      return '/attachments/upload?ticket=upload';
    });
    answerWith(() => new Response('{}'));
    await uploadAttachmentBytes('remote-thread', new File([PNG], 'a.png'));
    expect(requests[0]!.url).toBe(phone
      ? 'https://gpu.test/attachments/upload?ticket=upload'
      : 'http://page.test/backend/gpu/attachments/upload?ticket=upload');
    expect(requests[0]!.credentials).toBe(phone ? 'omit' : 'same-origin');

    noteThread('remote-thread', 'gpu');
    setBindingMock('MintAttachmentDownloadTicket', () => {
      expect(takePinnedBackend()).toBe('gpu');
      return '/attachments/remote-thread/image?ticket=download';
    });
    answerWith(() => new Response(PNG));
    const image = await fetchAttachmentBytes('remote-thread', 'image');
    expect(new Uint8Array(await image.arrayBuffer())).toEqual(PNG);
    expect(requests[1]!.url).toBe(phone
      ? 'https://gpu.test/attachments/remote-thread/image?ticket=download'
      : 'http://page.test/backend/gpu/attachments/remote-thread/image?ticket=download');
  });
});
