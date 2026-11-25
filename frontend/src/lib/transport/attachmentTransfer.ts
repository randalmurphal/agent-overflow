import { networkFetch } from './networkFetch';
import { fetchPairedComputer } from './deviceSession';
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
// Four boots, one code path. The minted URL is always RELATIVE, because
// the backend that mints it does not know which origin will present it —
// the embedded webview's transport, the `--connect` stub (which relays it
// upstream), a paired remote browser, or the phone shell. The first three
// resolve it against the page; the shell carries it onto the home
// endpoint through the one seam that knows where that is
// (./homeEndpoint.ts), and the ticket in the query is the admission
// either way.

import {
  MintAttachmentDownloadTicket,
  MintAttachmentUploadTicket,
  MintDictationChunkTicket,
  MintTrackerAttachmentDownloadTicket,
  MintTrackerAttachmentUploadTicket,
} from '../stores/bindings';
import type { WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { backendCredentials, backendTransferUrl } from './homeEndpoint';
import { requireEntityBackend, withBackendTarget } from './backends';
import { resolveThreadBackend } from './entityIndex';
import type { BackendKey } from './backendKey';
import type { Attachment } from '../types/attachment';

/**
 * Uploads one file's bytes and returns the attachment row the backend
 * created.
 *
 * The metadata is fixed by the MINT, not by this request: the ticket
 * carries the thread, filename, content type and exact byte count, and
 * the PUT contributes only the body. That is why an oversize file is
 * refused here for the price of one round trip instead of after its bytes
 * have crossed: the mint decides the KIND (image or file) from the name
 * and type, and the cap it checks is that kind's.
 *
 * Single-shot. A failed upload is retried by minting again rather than
 * resumed — a ticket is spent by the first request that presents it, and
 * resumable transfer is deferred (a body is bounded by its kind's cap,
 * 50 MiB at most, and the composer compresses images first).
 */
export async function uploadAttachmentBytes(threadId: string, file: File): Promise<Attachment> {
  const backend = requireEntityBackend(resolveThreadBackend(threadId));
  const url = await withBackendTarget(backend, () => MintAttachmentUploadTicket(threadId, file.name, file.type || '', file.size));
  const response = await fetchPairedComputer(backend, networkFetch, backendTransferUrl(url, backend), {
    method: 'PUT',
    // The file itself, streamed. Never read into a string: the whole
    // point of the move is that a 10 MiB image is not a JS string, a
    // base64 inflation of one, and a WebSocket frame holding both.
    body: file,
    // Load-bearing under `--connect`: that stub checks its OWN page
    // cookie before relaying, so a request with no credentials would be
    // refused there while working everywhere else. Same-origin is also
    // the default — stated because the default changing would be a
    // silent break in one boot only. A shell page has no cookie for the
    // backend's origin at all and omits instead; the ticket is the whole
    // admission there, exactly as the route's header argues.
    credentials: backendCredentials(backend),
  });
  if (!response.ok) {
    throw new Error((await transferFailure(response, 'Upload failed')).message);
  }
  return (await response.json()) as Attachment;
}

/**
 * Uploads piece `seq` of an open dictation recording to the computer that
 * opened it. The ticket fixes the recording, the piece's place and its exact
 * length; the PUT contributes the bytes. Pieces go in order, one at a time:
 * the recording refuses one out of order.
 */
export async function uploadDictationPiece(backend: BackendKey, recordingId: string, seq: number, piece: Blob): Promise<void> {
  const url = await withBackendTarget(backend, () => MintDictationChunkTicket(recordingId, seq, piece.size));
  const response = await fetchPairedComputer(backend, networkFetch, backendTransferUrl(url, backend), {
    method: 'PUT',
    body: piece,
    credentials: backendCredentials(backend),
  });
  if (!response.ok) {
    throw new Error((await transferFailure(response, 'Recording upload failed')).message);
  }
}

/**
 * Fetches one attachment's full-size bytes as a Blob.
 *
 * Callers build an object URL from it and are responsible for revoking
 * that URL — a blob URL pins decoded image data for as long as it lives,
 * which is the whole reason the lightbox refetches instead of caching.
 */
export async function fetchAttachmentBytes(threadId: string, attachmentId: string, signal?: AbortSignal): Promise<Blob> {
  const backend = requireEntityBackend(resolveThreadBackend(threadId));
  const url = await withBackendTarget(backend, () => MintAttachmentDownloadTicket(threadId, attachmentId));
  return fetchTicketedBytes(backend, url, 'Could not load image', signal);
}

/**
 * Attaches one file to a tracker issue through the computer that holds
 * its connection, and answers the write's outcome. The mint refuses a
 * tracker that stores no files, and a file over its ceiling, before any
 * byte crosses. The route answers the write as JSON once the tracker
 * judged it; a refusal before that throws `TransferRefusedError` with the
 * route's code and sentence. A fetch that fails, with the bytes partly
 * sent or the answer lost, throws as fetch does: whether the file was
 * attached is then unknown.
 */
export async function uploadTrackerAttachmentBytes(backend: BackendKey, connection: string, issue: string, file: File): Promise<WriteView> {
  const url = await withBackendTarget(backend, () => MintTrackerAttachmentUploadTicket(connection, issue, file.name, file.size));
  const response = await fetchPairedComputer(backend, networkFetch, backendTransferUrl(url, backend), {
    method: 'PUT',
    body: file,
    credentials: backendCredentials(backend),
  });
  if (!response.ok) throw await refusal(response, null);
  return (await response.json()) as WriteView;
}

/**
 * One attachment of a subscribed ticket as a Blob, through the computer
 * that answered the subscribe. The route sends the tracker's bytes as an
 * untyped save; the caller knows the attachment's type.
 */
export async function fetchTrackerAttachmentBytes(backend: BackendKey, subscriptionId: string, attachmentId: string, signal?: AbortSignal): Promise<Blob> {
  const url = await withBackendTarget(backend, () => MintTrackerAttachmentDownloadTicket(subscriptionId, attachmentId));
  return fetchTicketedBytes(backend, url, 'Could not load the attachment', signal);
}

/** A ticketed route refused the transfer with `status`; the message is
 * what the user sees. */
export class TransferRefusedError extends Error {
  readonly status: number;
  /** The refusal's code, from a route that answers `{code, message}`
   * (the tracker routes); null from one that answers a line. */
  readonly code: string | null;

  constructor(message: string, status: number, code: string | null = null) {
    super(message);
    this.name = 'TransferRefusedError';
    this.status = status;
    this.code = code;
  }
}

/**
 * The route answered 404 for a ticket: spent by an earlier request, expired,
 * or its bytes evicted from the backend's short-lived cache. A caller that
 * minted the ticket itself may mint once more; the message is what the user
 * sees when that fails too.
 */
export class TransferUnavailableError extends TransferRefusedError {
  constructor(message: string) {
    super(message, 404);
    this.name = 'TransferUnavailableError';
  }
}

/**
 * Spends one ticketed download URL that `backend` minted and returns the
 * body. The ticket is single use, so the URL is fetched here once and never
 * handed to an element that might request it again. A refusal throws
 * `TransferRefusedError` with the route's status and what it answered, a
 * 404 as `TransferUnavailableError`. `prefix` leads each failure's message. Null leaves
 * the route's reason standing alone, a 404's included: only a route whose
 * every answer is meant for a person (read aloud) asks for that.
 */
export async function fetchTicketedBytes(backend: BackendKey, url: string, prefix: string | null, signal?: AbortSignal): Promise<Blob> {
  const response = await fetchPairedComputer(backend, networkFetch, backendTransferUrl(url, backend), { credentials: backendCredentials(backend), signal });
  if (!response.ok) throw await refusal(response, prefix);
  return await response.blob();
}

/** A refused transfer as the error it throws: a 404 as
 * `TransferUnavailableError`, any other status as `TransferRefusedError`. */
async function refusal(response: Response, prefix: string | null): Promise<TransferRefusedError> {
  const { message, code } = await transferFailure(response, prefix);
  return response.status === 404 ? new TransferUnavailableError(message) : new TransferRefusedError(message, response.status, code);
}

/**
 * Turns a refused transfer into one sentence worth showing a person, with
 * the refusal's code when the route answered one.
 *
 * The routes answer a status and a short line, never a stack or a path;
 * the tracker routes answer `{code, message}`, whose message is the app's
 * own sentence and is quoted whole. A spent, missing or mismatched ticket
 * is the plain 404 every ticketed route answers, which says nothing worth
 * quoting. A caller with no prefix wants the route's own reason, so its
 * 404 with a reason (a read-aloud sentence that expired) is quoted. The
 * body is read anyway so the connection can be reused rather than
 * abandoned mid-response.
 */
async function transferFailure(response: Response, prefix: string | null): Promise<{ message: string; code: string | null }> {
  let detail = '';
  try {
    detail = (await response.text()).trim();
  } catch {
    // A body we could not read tells us nothing the status did not.
  }
  const say = (reason: string): string => (prefix ? `${prefix}: ${reason}` : reason);
  const answered = answeredRefusal(detail);
  if (answered) return { message: say(answered.message), code: answered.code };
  if (response.status === 413) {
    return { message: say('the file is larger than this backend accepts.'), code: null };
  }
  const quotable = detail !== '' && detail.length <= 200;
  if (response.status === 404 && (prefix !== null || !quotable || detail === PLAIN_NOT_FOUND)) {
    return { message: say('this transfer is no longer available. Try again.'), code: null };
  }
  if (quotable) {
    return { message: say(detail), code: null };
  }
  return { message: prefix ? `${prefix} (${response.status}).` : `the computer answered ${response.status}.`, code: null };
}

/** A `{code, message}` refusal body (transport/trackerroutes.go), or null
 * for any other body. */
function answeredRefusal(detail: string): { code: string; message: string } | null {
  if (!detail.startsWith('{')) return null;
  let body: unknown;
  try {
    body = JSON.parse(detail);
  } catch {
    return null;
  }
  if (!body || typeof body !== 'object') return null;
  const { code, message } = body as { code?: unknown; message?: unknown };
  if (typeof message !== 'string' || message === '') return null;
  return { code: typeof code === 'string' ? code : '', message };
}

/** The body of Go's `http.NotFound`. */
const PLAIN_NOT_FOUND = '404 page not found';
