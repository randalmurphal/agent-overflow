import type { BrowserCompanionEvent, BrowserDownload } from './bindings';
import { browserCompanionAct, type BrowserActionFailure } from './browserActions';
import { browserCompanionState, browserPaneOpen, canUseNativeBrowser } from './browserCompanion.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import { formatBytes } from '../utils/formatBytes';

// The thread's browser downloads (internal/browser/downloads_thread.go). The
// list rides the thread's state push, newest first; this module reads it,
// runs the pane's download actions, and announces a download that settled
// on its own (a Kind "download" event) once, as a toast.

/** How long a finished download's toast stays up. */
const FINISHED_TOAST_MS = 8000;

/** The thread's downloads, newest first; empty for none. */
export function browserDownloads(threadId: string): BrowserDownload[] {
  return browserCompanionState(threadId)?.downloads ?? [];
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
 * Runs one download action. A refusal lands in the pane's banner, or in a
 * toast when the thread has no pane open here (an action from a toast).
 */
async function act(threadId: string, kind: string, downloadId?: string): Promise<BrowserActionFailure | null> {
  const failure = await browserCompanionAct(threadId, { kind, downloadId });
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  return failure;
}

/** Stops a running download. */
export function cancelBrowserDownload(threadId: string, downloadId: string): Promise<BrowserActionFailure | null> {
  return act(threadId, 'download-cancel', downloadId);
}

/** Opens the OS file manager with a finished download selected. */
export function revealBrowserDownload(threadId: string, downloadId: string): Promise<BrowserActionFailure | null> {
  return act(threadId, 'download-reveal', downloadId);
}

/** Shows a finished download in a new browser tab (download.openable). */
export function openBrowserDownload(threadId: string, downloadId: string): Promise<BrowserActionFailure | null> {
  return act(threadId, 'download-open', downloadId);
}

/** Removes the finished, failed and canceled downloads from the list. */
export function clearBrowserDownloads(threadId: string): Promise<BrowserActionFailure | null> {
  return act(threadId, 'downloads-clear');
}

/** Whether a companion event is one announceBrowserDownload takes. */
export function isBrowserDownloadAnnouncement(event: BrowserCompanionEvent | null | undefined): boolean {
  return event?.kind === 'download';
}

/**
 * A Kind "download" event: one of the thread's downloads settled other than
 * by the user's cancel. Only a page that can host the thread's browser
 * announces it; the events are not replayed, so each page announces one
 * once.
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
    addToast('success', `Downloaded ${download.name}`, FINISHED_TOAST_MS, {
      label: 'Show in folder',
      run: () => void revealBrowserDownload(threadId, download.id),
    });
    return;
  }
  const message = `${download.name} did not download: ${download.error || 'unknown error'}`;
  addErrorToast(message, new Error(message));
}
