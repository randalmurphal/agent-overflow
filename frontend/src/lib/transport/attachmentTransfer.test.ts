import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  TransferRefusedError,
  TransferUnavailableError,
  fetchAttachmentBytes,
  fetchTicketedBytes,
  fetchTrackerAttachmentBytes,
  uploadAttachmentBytes,
  uploadTrackerAttachmentBytes,
} from './attachmentTransfer';
import { HOME_BACKEND } from './backendKey';
import { setHomeEndpoint, storeBackendEndpoint } from './homeEndpoint';

import { noteThread, threadBackend } from './entityIndex';
import { backendById, detachBackend, takePinnedBackend, withBackendTarget } from './backends';
import { Call } from './runtime';
import { stageBackend } from '../../test/helpers/backends';

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
  stageBackend({ id: '', backendId: '', name: 'Home' });
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
    // A Request reads back 'same-origin' when nothing was set, so the
    // credentials are taken from what the module handed fetch.
    const answer = globalThis.fetch;
    let credentials: RequestCredentials | undefined;
    globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
      credentials = init?.credentials;
      return answer(input, init);
    }) as typeof globalThis.fetch;
    const file = new File([PNG], 'shot.png', { type: 'image/png' });

    const record = await uploadAttachmentBytes('thr-1', file);

    // Every field the stored row will carry is decided by the MINT. The
    // request contributes the body and nothing else.
    expect(mint).toHaveBeenCalledWith('thr-1', 'shot.png', 'image/png', file.size);
    expect(record).toEqual({ id: 'att-1', threadId: 'thr-1' });

    const sent = requests[0];
    expect(sent?.method).toBe('PUT');
    // The URL stays relative and the browser resolves it against the page:
    // the SPA is served from three different origins across the boots it
    // supports and does not know which one it is in, so anything this
    // module did to the URL would be right in at most one of the three.
    expect(sent?.url).toBe('http://page.test/attachments/upload?ticket=abc');
    expect(new Uint8Array(await sent!.arrayBuffer())).toEqual(PNG);
    // Same-origin credentials, which the --connect stub requires.
    expect(credentials).toBe('same-origin');
  });

  it.each([
    ['an over-cap refusal in words a person can act on', 413, 'attachment too large', /larger than this backend accepts/],
    ['a spent or unknown ticket as something to retry', 404, '404 page not found', /no longer available/],
    ['a short refusal line', 400, 'attachment rejected', /attachment rejected/],
    ['a long refusal body as its status alone', 400, 'x'.repeat(5000), /^Upload failed \(400\)\.$/],
  ])('reports %s', async (_name, status, body, expected) => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    answerWith(() => new Response(body, { status }));

    await expect(uploadAttachmentBytes('thr-1', new File([PNG], 'a.png', { type: 'image/png' })))
      .rejects.toThrow(expected);
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

describe('a tracker attachment', () => {
  const view = { parts: [{ step: 'attach', state: 'applied' }] };

  it('mints for the file on its connection and answers the write the PUT answered', async () => {
    const mint = setBindingMock('MintTrackerAttachmentUploadTicket', () => '/attachments/tracker/upload?ticket=up');
    answerWith(() => new Response(JSON.stringify(view), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const file = new File([PNG], 'report.pdf', { type: 'application/pdf' });

    await expect(uploadTrackerAttachmentBytes(HOME_BACKEND, 'conn-1', '10101', file)).resolves.toEqual(view);

    expect(mint).toHaveBeenCalledWith('conn-1', '10101', 'report.pdf', file.size);
    expect(requests[0]?.method).toBe('PUT');
    expect(requests[0]?.url).toBe('http://page.test/attachments/tracker/upload?ticket=up');
    expect(new Uint8Array(await requests[0]!.arrayBuffer())).toEqual(PNG);
  });

  it('throws the route\'s sentence and code for a refusal, whatever its length', async () => {
    setBindingMock('MintTrackerAttachmentUploadTicket', () => '/attachments/tracker/upload?ticket=up');
    // Longer than the 200 characters a plain line is quoted up to.
    const message = 'the file is now 5 bytes, not the 4 it was when its upload was prepared; select the file again and attach it'.padEnd(240, '.');
    answerWith(() => new Response(JSON.stringify({ code: 'tracker_upload_size_changed', message }), { status: 400, headers: { 'Content-Type': 'application/json' } }));

    const refused = await uploadTrackerAttachmentBytes(HOME_BACKEND, 'conn-1', '10101', new File([PNG], 'a.pdf')).catch((err: unknown) => err);

    expect(refused).toBeInstanceOf(TransferRefusedError);
    expect(refused).toMatchObject({ status: 400, code: 'tracker_upload_size_changed', message });
  });

  it('reads one attachment through the subscription\'s download ticket', async () => {
    const mint = setBindingMock('MintTrackerAttachmentDownloadTicket', () => '/attachments/tracker/att-1?ticket=down');
    answerWith(() => new Response(PNG, { status: 200, headers: { 'Content-Type': 'application/octet-stream' } }));

    const blob = await fetchTrackerAttachmentBytes(HOME_BACKEND, 'sub-1', 'att-1');

    expect(mint).toHaveBeenCalledWith('sub-1', 'att-1');
    expect(new Uint8Array(await blob.arrayBuffer())).toEqual(PNG);
    expect(new URL(requests[0]!.url).pathname).toBe('/attachments/tracker/att-1');
  });

  it('says what the tracker answered when a download is refused before its first byte', async () => {
    setBindingMock('MintTrackerAttachmentDownloadTicket', () => '/attachments/tracker/att-1?ticket=down');
    answerWith(() => new Response(JSON.stringify({ code: 'tracker_unavailable', message: 'Jira answered 403 for this attachment' }), { status: 400 }));

    await expect(fetchTrackerAttachmentBytes(HOME_BACKEND, 'sub-1', 'att-1'))
      .rejects.toMatchObject({ code: 'tracker_unavailable', message: 'Could not load the attachment: Jira answered 403 for this attachment' });
  });
});

describe('fetchTicketedBytes', () => {
  it('hands the caller\'s signal to the request, so an abort stops the transfer', async () => {
    globalThis.fetch = ((_input: RequestInfo | URL, init?: RequestInit) => {
      const signal = init?.signal;
      if (!signal) return Promise.reject(new Error('the request carried no signal'));
      return new Promise<Response>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true });
      });
    }) as typeof globalThis.fetch;
    const controller = new AbortController();

    const pending = fetchTicketedBytes(HOME_BACKEND, '/attachments/image/abc?ticket=t', 'Could not load image', controller.signal);
    controller.abort();

    await expect(pending).rejects.toBe(controller.signal.reason);
  });

  it('names a spent ticket as TransferUnavailableError and any other refusal as a plain Error', async () => {
    answerWith(() => new Response('404 page not found', { status: 404 }));
    const spent = await fetchTicketedBytes(HOME_BACKEND, '/attachments/image/abc?ticket=t', 'Could not load image').catch((err: unknown) => err);
    expect(spent).toBeInstanceOf(TransferUnavailableError);
    expect((spent as Error).message).toBe('Could not load image: this transfer is no longer available. Try again.');

    answerWith(() => new Response('attachment is larger than 100 MiB', { status: 413 }));
    const refused = await fetchTicketedBytes(HOME_BACKEND, '/attachments/image/abc?ticket=t', 'Could not load image').catch((err: unknown) => err);
    expect(refused).toBeInstanceOf(Error);
    expect(refused).not.toBeInstanceOf(TransferUnavailableError);
    expect((refused as Error).message).toBe('Could not load image: the file is larger than this backend accepts.');
  });

  it('carries the route\'s status on every refusal, a 404 as TransferUnavailableError', async () => {
    const url = '/attachments/speech/s1?ticket=t';
    for (const [status, body, message] of [
      [403, 'read aloud in OpenAI voices is off on this computer', 'read aloud in OpenAI voices is off on this computer'],
      [404, 'this section was already read aloud or has expired', 'this section was already read aloud or has expired'],
      [409, 'the key file is empty', 'the key file is empty'],
      [502, 'OpenAI did not read the text as written', 'OpenAI did not read the text as written'],
    ] as const) {
      answerWith(() => new Response(body, { status }));
      const refused = await fetchTicketedBytes(HOME_BACKEND, url, null).catch((err: unknown) => err);
      expect(refused).toBeInstanceOf(TransferRefusedError);
      expect(refused).toMatchObject({ status, message });
      expect(refused instanceof TransferUnavailableError).toBe(status === 404);
    }
    answerWith(() => new Response('', { status: 403 }));
    const prefixed = await fetchTicketedBytes(HOME_BACKEND, '/attachments/image/abc?ticket=t', 'Could not load image').catch((err: unknown) => err);
    expect(prefixed).toMatchObject({ status: 403, message: 'Could not load image (403).' });
  });

  it('words a failure with the caller\'s prefix, or with the route\'s reason alone', async () => {
    const url = '/attachments/speech/s1?ticket=t';
    const failure = async (body: string, status: number, prefix: string | null) => {
      answerWith(() => new Response(body, { status }));
      return fetchTicketedBytes(HOME_BACKEND, url, prefix).then(() => new Error('the fetch succeeded'), (err: unknown) => err as Error);
    };
    // The unprefixed reasons, a 404's included, are the rows of the test
    // above.
    expect((await failure('OpenAI did not read the text as written', 502, 'Could not load image')).message)
      .toBe('Could not load image: OpenAI did not read the text as written');
    expect((await failure('', 500, null)).message).toBe('the computer answered 500.');
    expect((await failure('', 500, 'Could not load image')).message).toBe('Could not load image (500).');

    // With a prefix no 404 body is quoted, and without one the plain ticket
    // 404 is not.
    expect((await failure('not found', 404, 'Could not load image')).message)
      .toBe('Could not load image: this transfer is no longer available. Try again.');
    const spent = await failure('404 page not found', 404, null);
    expect(spent).toBeInstanceOf(TransferUnavailableError);
    expect(spent.message).toBe('this transfer is no longer available. Try again.');
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

  it('puts the bytes on the endpoint and presents no cookie', async () => {
    setBindingMock('MintAttachmentUploadTicket', () => '/attachments/upload?ticket=abc');
    const seen: RequestInit[] = [];
    const real = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push({ ...init, url: String(input) } as RequestInit & { url: string });
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

  it('uploads immediately after remote thread creation without a draft edit or sidebar refresh', async () => {
    stageBackend({ id: 'phone-desk' });
    stageBackend({ id: 'other', backendId: 'other-uuid' });
    detachBackend('');
    const calls = vi.mocked(backendById('phone-desk')!.client.callByID);
    calls.mockImplementation(async (method) => method === 2579322833
      ? { id: 'created-on-phone', projectId: 'project' }
      : '/attachments/upload?ticket=new');
    setBindingMock('MintAttachmentUploadTicket', (...args: unknown[]) => Call.ByID(1857144453, ...args));
    answerWith(() => new Response(JSON.stringify({ id: 'photo', threadId: 'created-on-phone' })));

    await withBackendTarget('phone-desk', () => Call.ByID(2579322833, { projectId: 'project' }));
    const photo = new File([PNG], 'shot.png', { type: 'image/png' });
    await expect(uploadAttachmentBytes('created-on-phone', photo)).resolves.toMatchObject({ id: 'photo' });
    expect(calls).toHaveBeenLastCalledWith(1857144453, ['created-on-phone', photo.name, photo.type, photo.size]);
    expect(requests[0]!.url).toBe('http://page.test/backend/phone-desk/attachments/upload?ticket=new');
    expect(backendById('other')!.client.callByID).not.toHaveBeenCalled();
  });

  it.each([false, true])('keeps both uploads and downloads on that computer (phone: %s)', async (phone) => {
    noteThread('remote-thread', 'gpu', 0);
    if (phone) {
      setHomeEndpoint('https://mac.test');
      storeBackendEndpoint('gpu', 'https://gpu.test');
    }
    setBindingMock('MintAttachmentUploadTicket', async () => {
      expect(takePinnedBackend()).toBe('gpu');
      // Changing ownership while the mint is pending must not redirect its bytes.
      noteThread('remote-thread', '', 1);
      return '/attachments/upload?ticket=upload';
    });
    answerWith(() => new Response('{}'));
    await uploadAttachmentBytes('remote-thread', new File([PNG], 'a.png'));
    expect(threadBackend('remote-thread')).toBe('');
    expect(requests[0]!.url).toBe(phone
      ? 'https://gpu.test/attachments/upload?ticket=upload'
      : 'http://page.test/backend/gpu/attachments/upload?ticket=upload');
    expect(requests[0]!.credentials).toBe(phone ? 'omit' : 'same-origin');

    noteThread('remote-thread', 'gpu', 2);
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

describe.each(['upload', 'download'] as const)('%s ownership routing', (kind) => {
  const transfer = () => kind === 'upload'
    ? uploadAttachmentBytes('unindexed', new File([PNG], 'a.png'))
    : fetchAttachmentBytes('unindexed', 'image');
  const method = kind === 'upload' ? 'MintAttachmentUploadTicket' : 'MintAttachmentDownloadTicket';

  it('uses the only connected computer when HOME is absent', async () => {
    stageBackend({ id: 'phone-desk' });
    detachBackend('');
    setBindingMock(method, () => {
      expect(takePinnedBackend()).toBe('phone-desk');
      return '/attachments/unindexed/image?ticket=test';
    });
    answerWith(() => kind === 'upload' ? new Response('{}') : new Response(PNG));
    await transfer();
    expect(requests[0]!.url).toBe('http://page.test/backend/phone-desk/attachments/unindexed/image?ticket=test');
  });

  it('rejects ambiguous ownership before minting or sending bytes', async () => {
    stageBackend({ id: 'other' });
    const mint = setBindingMock(method, () => '/attachments/transfer');
    answerWith(() => new Response('{}'));
    await expect(transfer()).rejects.toThrow('owns this item is unknown');
    expect(mint).not.toHaveBeenCalled();
    expect(requests).toHaveLength(0);
  });

  it('rejects conflicting ownership before minting or sending bytes', async () => {
    noteThread('unindexed', '', 1);
    noteThread('unindexed', 'other', 1);
    const mint = setBindingMock(method, () => '/attachments/transfer');
    answerWith(() => new Response('{}'));
    await expect(transfer()).rejects.toThrow('Two computers claim this conversation');
    expect(mint).not.toHaveBeenCalled();
    expect(requests).toHaveLength(0);
  });
});
