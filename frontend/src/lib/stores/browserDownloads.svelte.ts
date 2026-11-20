import { untrack } from 'svelte';
import type { BrowserCompanionEvent, BrowserDownload } from './bindings';
import { actInPane, browserCompanionAct, type BrowserActionFailure } from './browserActions';
import { browserAction, downloadIdOf, type DownloadKind, type ThreadAction } from './browserActionWire';
import {
  browserPaneOpen,
  browserPaneState,
  browserThreadSnapshot,
  canUseNativeBrowser,
  observeBrowserCompanion,
  type BrowserPaneAttachment,
} from './browserCompanion.svelte';
import { addErrorToast, addToast, getToasts, withdrawToastAction, type ToastAction } from './toast.svelte';
import { getTransportHelloFor } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { formatBytes } from '../utils/formatBytes';

// The thread's browser downloads (internal/browser/downloads_thread.go). The
// list rides the thread's state push, newest first; this module reads it,
// runs the pane's download actions, and announces a download that settled
// on its own (a Kind "download" event) once, as a toast.

/** How long a finished download's toast stays up. */
const FINISHED_TOAST_MS = 8000;

/**
 * A finished download's toast action, offered while the thread's list
 * resolves it: the list's generation it was announced under, the number
 * the backend gave the announcement among the thread's states, and the
 * backend launch that numbered it.
 */
interface OfferedAction {
  readonly threadId: string;
  readonly gen: number;
  readonly seq: number;
  readonly launch: string;
}

// A finished download's toast action carries its offer, so the offer goes
// with the toast's action: dismissed, expired or withdrawn. An offer whose
// thread the window stopped watching stays while its toast does: no state
// withdraws it, and its action answers that the download is gone when the
// list no longer resolves it.
const OFFER: unique symbol = Symbol('download offer');
type OfferAction = ToastAction & { readonly [OFFER]: OfferedAction };

function offerOf(action: ToastAction | undefined): OfferedAction | null {
  return action && OFFER in action ? (action as OfferAction)[OFFER] : null;
}

function homeLaunch(): string {
  return getTransportHelloFor(HOME_BACKEND)?.launchId ?? '';
}

/**
 * Whether a state of the offer's thread says its list no longer resolves
 * the download: it is numbered after the announcement and shows another
 * generation (a clear, the thread's browser closing), or another launch
 * of the backend made it.
 */
function stateDropsOffer(offer: OfferedAction, state: BrowserCompanionEvent, launch: string): boolean {
  if (launch !== offer.launch) return true;
  return (state.seq ?? 0) > offer.seq && (state.downloadsGen ?? 0) !== offer.gen;
}

observeBrowserCompanion({
  stateAdmitted: (event) => {
    const launch = homeLaunch();
    for (const toast of untrack(getToasts)) {
      const offer = offerOf(toast.action);
      if (offer && offer.threadId === event.threadId && stateDropsOffer(offer, event, launch)) withdrawToastAction(toast.id);
    }
  },
});

/**
 * Whether the announcement's action resolves as the window knows the
 * thread now: the backend named the list generation that resolves it, and
 * no state already admitted, numbered after it, drops it.
 */
function announcementResolves(offer: OfferedAction): boolean {
  if (!offer.gen) return false;
  const held = browserThreadSnapshot(offer.threadId);
  return held.status !== 'known' || !stateDropsOffer(offer, held.snapshot, offer.launch);
}

/** The backend's answer for a download its list no longer has (internal/app browserDownloadUnavailableCode). */
function downloadUnavailable(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'browser_download_unavailable';
}

/** The downloads of the thread `pane` is attached for, newest first; empty for none (browserPaneState). */
export function browserDownloads(pane: BrowserPaneAttachment): BrowserDownload[] {
  return browserPaneState(pane)?.downloads ?? [];
}

export function downloadRunning(download: BrowserDownload): boolean {
  return download.state === 'in_progress';
}

/** How many steps a progress indicator moves through. */
export const DOWNLOAD_PROGRESS_STEPS = 8;

/**
 * The received share of the running downloads with a known size, rounded
 * down to one of DOWNLOAD_PROGRESS_STEPS steps; null when no running
 * download has a known size. The indicator moves only when the step changes.
 */
export function downloadProgressStep(downloads: BrowserDownload[]): number | null {
  let received = 0;
  let total = 0;
  for (const download of downloads) {
    if (!downloadRunning(download) || !download.totalBytes) continue;
    received += Math.min(download.receivedBytes, download.totalBytes);
    total += download.totalBytes;
  }
  if (total === 0) return null;
  return Math.floor((received / total) * DOWNLOAD_PROGRESS_STEPS) / DOWNLOAD_PROGRESS_STEPS;
}

/** The download's status line under its name. */
export function downloadStatus(download: BrowserDownload): string {
  switch (download.state) {
    case 'in_progress': {
      const bytes = download.totalBytes
        ? `${formatBytes(download.receivedBytes)} of ${formatBytes(download.totalBytes)}`
        : formatBytes(download.receivedBytes);
      // A running download's error is a refused cancel's reason.
      return download.error ? `${bytes} · ${download.error}` : bytes;
    }
    case 'completed':
      // A completed download with an error saved a file AO does not open.
      return download.error ? `${formatBytes(download.receivedBytes)} · ${download.error}` : formatBytes(download.receivedBytes);
    case 'failed':
      return `Failed: ${download.error || 'unknown error'}`;
    default:
      return download.error ? `Canceled: ${download.error}` : 'Canceled';
  }
}

/** The host of the page the download began on, for its row. */
export function downloadSourceHost(download: BrowserDownload): string {
  if (!download.source) return '';
  try {
    const url = new URL(download.source);
    return url.protocol === 'file:' ? 'a local file' : url.host;
  } catch {
    return download.source;
  }
}

/**
 * Where a download action comes from: the attached pane the user acted in
 * (its downloads list), whose order an opening takes its place in, or the
 * thread alone (a toast).
 */
export type DownloadActionSource =
  | { readonly kind: 'pane'; readonly pane: BrowserPaneAttachment }
  | { readonly kind: 'thread'; readonly threadId: string };

/**
 * Runs one download action. A refusal lands in the pane's banner, or in a
 * toast when the thread has no pane open here (an action from a toast): a
 * notice when the download is no longer in the list, an error otherwise.
 */
async function act(from: DownloadActionSource, action: ThreadAction): Promise<BrowserActionFailure | null> {
  const threadId = from.kind === 'pane' ? from.pane.threadId : from.threadId;
  const failure = from.kind === 'pane' ? await actInPane(from.pane, action) : await browserCompanionAct(threadId, action);
  if (!failure || browserPaneOpen(threadId)) return failure;
  if (downloadUnavailable(failure.cause)) addToast('info', 'That download is no longer in the browser\'s downloads list.');
  else addErrorToast(failure.error, failure.cause);
  return failure;
}

/** One download's action; a blank id names no download and sends nothing. */
function actOnDownload(from: DownloadActionSource, kind: DownloadKind, raw: string): Promise<BrowserActionFailure | null> {
  const downloadId = downloadIdOf(raw);
  return downloadId ? act(from, browserAction.download(kind, downloadId)) : Promise.resolve(null);
}

/** Stops a running download. */
export function cancelBrowserDownload(from: DownloadActionSource, downloadId: string): Promise<BrowserActionFailure | null> {
  return actOnDownload(from, 'download-cancel', downloadId);
}

/** Opens the OS file manager with a finished download selected. */
export function revealBrowserDownload(from: DownloadActionSource, downloadId: string): Promise<BrowserActionFailure | null> {
  return actOnDownload(from, 'download-reveal', downloadId);
}

/** Shows a finished download in a new browser tab (download.openable). */
export function openBrowserDownload(from: DownloadActionSource, downloadId: string): Promise<BrowserActionFailure | null> {
  return actOnDownload(from, 'download-open', downloadId);
}

/** Removes the finished, failed and canceled downloads from the list. */
export function clearBrowserDownloads(from: DownloadActionSource): Promise<BrowserActionFailure | null> {
  return act(from, browserAction.clearDownloads());
}

/** Whether a companion event is one announceBrowserDownload takes. */
export function isBrowserDownloadAnnouncement(event: BrowserCompanionEvent | null | undefined): boolean {
  return event?.kind === 'download';
}

/**
 * A Kind "download" event: one of the thread's downloads settled other than
 * by the user's cancel. Only a page that can host the thread's browser
 * announces it; the events are not replayed, so each page announces one
 * once. A finished one's "Show in folder" is offered while the thread's
 * list resolves it, and withdrawn from the toast when a state shows the
 * list cleared or the thread's browser closed.
 */
export function announceBrowserDownload(event: BrowserCompanionEvent): void {
  if (!event.threadId || !canUseNativeBrowser(event.threadId)) return;
  const download = event.download;
  if (!download) return;
  const threadId = event.threadId;
  if (download.state === 'completed' && download.error) {
    // Saved, but not a file AO opens or shows.
    addToast('info', `Downloaded ${download.name}; ${download.error}`, FINISHED_TOAST_MS);
    return;
  }
  if (download.state === 'completed') {
    const offer: OfferedAction = { threadId, gen: event.downloadsGen ?? 0, seq: event.seq ?? 0, launch: homeLaunch() };
    if (!announcementResolves(offer)) {
      addToast('success', `Downloaded ${download.name}`, FINISHED_TOAST_MS);
      return;
    }
    const action: OfferAction = {
      label: 'Show in folder',
      run: () => void revealBrowserDownload({ kind: 'thread', threadId }, download.id),
      [OFFER]: offer,
    };
    addToast('success', `Downloaded ${download.name}`, FINISHED_TOAST_MS, action);
    return;
  }
  const message = `${download.name} did not download: ${download.error || 'unknown error'}`;
  addErrorToast(message, new Error(message));
}
