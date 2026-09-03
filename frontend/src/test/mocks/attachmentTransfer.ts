// Fake for the two-hop attachment transfer (wave 6b).
//
// Attachment bytes no longer cross on the RPC wire, so a single
// `setBindingMock('UploadAttachment', ...)` no longer describes an
// upload. What a test has to stand in for now is BOTH halves: the mint
// that authorizes exactly one transfer, and the fetch that spends its
// ticket. These helpers install both from one call, so a test still says
// "an upload produces this record" in one line.
//
// The fake keeps the property the real mechanism rests on rather than
// merely returning the right value: the metadata comes from the TICKET,
// not from the request. A test that asserts on the mint's arguments is
// asserting on what the stored row would say, which is the same thing the
// backend guarantees.

import type { Mock } from 'vitest';
import { setBindingMock } from './bindings-app';
import type { Attachment } from '../../lib/types/attachment';

type MockedFn = Mock<(...args: unknown[]) => unknown>;

export type UploadHandler = (
  threadId: string,
  filename: string,
  mimeType: string,
  size: number,
) => Attachment | Promise<Attachment>;

export type DownloadHandler = (
  threadId: string,
  attachmentId: string,
) => Blob | Promise<Blob>;

/** A minted-but-unspent ticket, exactly as the backend books one. */
const tickets = new Map<string, () => Promise<Response>>();
let ticketCounter = 0;
let realFetch: typeof globalThis.fetch | undefined;

/**
 * A one-pixel PNG. Enough for a Blob to be a plausible image; no test
 * decodes it.
 */
export const TEST_PNG_BYTES = new Uint8Array([
  0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
  0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
]);

/**
 * Stubs `MintAttachmentUploadTicket` and answers the PUT that spends it.
 *
 * Returns the MINT mock, whose first three arguments are the thread,
 * filename and content type in the same positions the deleted
 * `UploadAttachment` binding used — so an assertion about what was
 * uploaded still reads the same way.
 *
 * `handler` runs when the bytes are sent, not when the ticket is minted,
 * which is what lets a test hold an upload open mid-flight. Throwing from
 * it produces a refused transfer rather than a rejected mint.
 */
export function mockAttachmentUpload(handler: UploadHandler): MockedFn {
  installTransferFetch();
  return setBindingMock(
    'MintAttachmentUploadTicket',
    (threadId: string, filename: string, mimeType: string, size: number) => {
      const ticket = issueTicket(async () => {
        const record = await handler(threadId, filename, mimeType, size);
        return new Response(JSON.stringify(record), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      });
      return `/attachments/upload?ticket=${ticket}`;
    },
  );
}

/**
 * Stubs `MintAttachmentDownloadTicket` and answers the GET that spends
 * it. Defaults to a small PNG, which is all most callers need — they
 * assert that an image rendered, not what was in it.
 */
export function mockAttachmentDownload(handler?: DownloadHandler): MockedFn {
  installTransferFetch();
  const answer: DownloadHandler = handler
    ?? (() => new Blob([TEST_PNG_BYTES], { type: 'image/png' }));
  return setBindingMock(
    'MintAttachmentDownloadTicket',
    (threadId: string, attachmentId: string) => {
      const ticket = issueTicket(async () => {
        const blob = await answer(threadId, attachmentId);
        return new Response(blob, {
          status: 200,
          headers: { 'Content-Type': blob.type || 'application/octet-stream' },
        });
      });
      return `/attachments/${encodeURIComponent(threadId)}/${encodeURIComponent(attachmentId)}?ticket=${ticket}`;
    },
  );
}

/**
 * Drops every outstanding ticket and restores the real fetch. Called from
 * the shared test setup, so a test that mocked a transfer cannot leave a
 * global stub behind for the next one.
 */
export function resetAttachmentTransferMocks(): void {
  tickets.clear();
  if (realFetch) {
    globalThis.fetch = realFetch;
    realFetch = undefined;
  }
}

function issueTicket(answer: () => Promise<Response>): string {
  const ticket = `test-ticket-${++ticketCounter}`;
  tickets.set(ticket, answer);
  return ticket;
}

/**
 * Installs a fetch that answers only the attachment routes. Anything else
 * reaches the real one, so a test that fetches something unrelated is not
 * silently rerouted through here.
 */
function installTransferFetch(): void {
  if (realFetch) return;
  realFetch = globalThis.fetch;
  const passthrough = realFetch;
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = requestURL(input);
    if (!url.pathname.startsWith('/attachments/')) {
      return passthrough(input, init);
    }
    const ticket = url.searchParams.get('ticket') ?? '';
    const answer = tickets.get(ticket);
    if (!answer) {
      // Single use, exactly as the real routes are: a replayed ticket is
      // a 404 and is indistinguishable from one that never existed.
      return new Response('not found', { status: 404 });
    }
    tickets.delete(ticket);
    try {
      return await answer();
    } catch (err) {
      return new Response(err instanceof Error ? err.message : String(err), { status: 400 });
    }
  }) as typeof globalThis.fetch;
}

function requestURL(input: RequestInfo | URL): URL {
  const raw = typeof input === 'string'
    ? input
    : input instanceof URL
      ? input.href
      : input.url;
  // Relative by design — the SPA never names a host — so resolve against
  // whatever origin the test document has.
  return new URL(raw, globalThis.location?.href ?? 'http://localhost/');
}
