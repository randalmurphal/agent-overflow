// Attachment bytes, over HTTP rather than over the WebSocket.
//
// Every attachment byte used to ride base64 inside one WS RPC frame, so a
// 10 MiB image became a ~13.4 MB frame on the same socket as the live
// event stream. Now an RPC mints a single-use, subject-bound TICKET and
// the bytes cross on their own connection
// (internal/transport/attachmentroutes.go).
//
// This is the ONE module in the frontend that calls `fetch` for app data.
// Everything else goes through the generated bindings, and the reason
// this does not is that bindings carry JSON: a Blob body and a streamed
// response are exactly what they cannot express.
//
// Three boots, one code path. The minted URL is always RELATIVE, so it
// resolves against whatever origin served the page — the embedded
// webview's transport, the `--connect` stub (which relays it upstream),
// or a paired remote browser. A host baked in here would be right for
// exactly one of the three, and the SPA does not know which one it is in.

import { MintAttachmentDownloadTicket, MintAttachmentUploadTicket } from '../stores/bindings';
import type { Attachment } from '../types/attachment';

/**
 * Uploads one file's bytes and returns the attachment row the backend
 * created.
 *
 * The metadata is fixed by the MINT, not by this request: the ticket
 * carries the thread, filename, content type and exact byte count, and
 * the PUT contributes only the body. That is why an oversize or
 * non-image file is refused here for the price of one round trip instead
 * of after its bytes have crossed.
 *
 * Single-shot. A failed upload is retried by minting again rather than
 * resumed — a ticket is spent by the first request that presents it, and
 * resumable transfer is deferred (bodies are at most 10 MiB and the
 * composer compresses images first).
 */
export async function uploadAttachmentBytes(threadId: string, file: File): Promise<Attachment> {
  const url = await MintAttachmentUploadTicket(threadId, file.name, file.type || '', file.size);
  const response = await fetch(url, {
    method: 'PUT',
    // The file itself, streamed. Never read into a string: the whole
    // point of the move is that a 10 MiB image is not a JS string, a
    // base64 inflation of one, and a WebSocket frame holding both.
    body: file,
    // Load-bearing under `--connect`: that stub checks its OWN page
    // cookie before relaying, so a request with no credentials would be
    // refused there while working everywhere else. Same-origin is also
    // the default — stated because the default changing would be a
    // silent break in one boot only.
    credentials: 'same-origin',
  });
  if (!response.ok) {
    throw new Error(await transferFailure(response, 'Upload failed'));
  }
  return (await response.json()) as Attachment;
}

/**
 * Fetches one attachment's full-size bytes as a Blob.
 *
 * Callers build an object URL from it and are responsible for revoking
 * that URL — a blob URL pins decoded image data for as long as it lives,
 * which is the whole reason the lightbox refetches instead of caching.
 */
export async function fetchAttachmentBytes(threadId: string, attachmentId: string): Promise<Blob> {
  const url = await MintAttachmentDownloadTicket(threadId, attachmentId);
  const response = await fetch(url, { credentials: 'same-origin' });
  if (!response.ok) {
    throw new Error(await transferFailure(response, 'Could not load image'));
  }
  return await response.blob();
}

/**
 * Turns a refused transfer into one sentence worth showing a person.
 *
 * The routes answer a status and a short line, never a stack or a path:
 * a spent, missing or mismatched ticket is 404 in every case, so there is
 * nothing here to distinguish and nothing worth quoting verbatim. The
 * body is read anyway so the connection can be reused rather than
 * abandoned mid-response.
 */
async function transferFailure(response: Response, prefix: string): Promise<string> {
  let detail = '';
  try {
    detail = (await response.text()).trim();
  } catch {
    // A body we could not read tells us nothing the status did not.
  }
  if (response.status === 413) {
    return `${prefix}: the image is larger than this backend accepts.`;
  }
  if (response.status === 404) {
    return `${prefix}: this transfer is no longer available. Try again.`;
  }
  if (detail && detail.length <= 200) {
    return `${prefix}: ${detail}`;
  }
  return `${prefix} (${response.status}).`;
}
