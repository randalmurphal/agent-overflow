import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { BrowserCompanionEvent, BrowserDownload } from './bindings';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from './browserCompanion.svelte';
import {
  announceBrowserDownload,
  downloadProgressStep,
  isBrowserDownloadAnnouncement,
  downloadSourceHost,
  downloadStatus,
} from './browserDownloads.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { setHostWindowFromBootstrap } from '../transport/pageHost';
import { TransportError, type TransportHello } from '../transport/wsClient';

const THREAD = 'thread-1';

function download(extra: Partial<BrowserDownload> = {}): BrowserDownload {
  return { id: 'd1', name: 'report.pdf', state: 'in_progress', receivedBytes: 0, ...extra } as BrowserDownload;
}

// A finished download announced at number 5 under list generation 1, the
// list that resolves it then.
function finished(extra: Partial<BrowserCompanionEvent> = {}): BrowserCompanionEvent {
  return { kind: 'download', threadId: THREAD, download: download({ state: 'completed', receivedBytes: 10 }), downloadsGen: 1, seq: 5, ...extra };
}

// The thread's state numbered seq, showing list generation gen (0 for none).
function state(seq: number, gen: number): BrowserCompanionEvent {
  return { kind: 'state', threadId: THREAD, seq, downloadsGen: gen || undefined, pages: [] };
}

const hello = (launchId: string): TransportHello => ({
  protocolVersion: 1, capabilities: [], backendId: 'home', backendName: 'home', serverTimeMs: 0,
  clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0, launchId,
});

const showInFolder = () => getToasts()[0]?.action?.label;

describe('browser downloads', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetToastsForTest();
  });
  afterEach(() => {
    resetToastsForTest();
    resetBrowserCompanionForTest();
    setHostWindowFromBootstrap(true);
    __setTransportHelloForTest(null);
  });

  it('steps the progress of the running downloads with a known size', () => {
    expect(downloadProgressStep([])).toBeNull();
    // A running download of unknown size has no step to show.
    expect(downloadProgressStep([download({ receivedBytes: 500 })])).toBeNull();
    expect(downloadProgressStep([download({ receivedBytes: 499, totalBytes: 1000 })])).toBe(3 / 8);
    expect(downloadProgressStep([download({ receivedBytes: 500, totalBytes: 1000 })])).toBe(4 / 8);
    // Two running downloads count together; a finished one does not count.
    expect(downloadProgressStep([
      download({ id: 'a', receivedBytes: 1000, totalBytes: 1000 }),
      download({ id: 'b', receivedBytes: 0, totalBytes: 1000 }),
      download({ id: 'c', state: 'completed', receivedBytes: 0, totalBytes: 9000 }),
    ])).toBe(4 / 8);
  });

  it('describes each state', () => {
    expect(downloadStatus(download({ receivedBytes: 1500, totalBytes: 3_000_000 }))).toBe('2 KB of 3.0 MB');
    expect(downloadStatus(download({ receivedBytes: 1500 }))).toBe('2 KB');
    expect(downloadStatus(download({ receivedBytes: 1500, totalBytes: 3_000_000, error: 'the browser refused to cancel the download' })))
      .toBe('2 KB of 3.0 MB · the browser refused to cancel the download');
    expect(downloadStatus(download({ state: 'completed', receivedBytes: 912 }))).toBe('912 B');
    expect(downloadStatus(download({ state: 'completed', receivedBytes: 912, error: 'the saved file is a link' }))).toBe('912 B · the saved file is a link');
    expect(downloadStatus(download({ state: 'failed', error: 'net::ERR_FAILED' }))).toBe('Failed: net::ERR_FAILED');
    expect(downloadStatus(download({ state: 'canceled' }))).toBe('Canceled');
    expect(downloadStatus(download({ state: 'canceled', error: 'page closed' }))).toBe('Canceled: page closed');
    expect(downloadSourceHost(download({ source: 'https://example.test:8080/files' }))).toBe('example.test:8080');
    expect(downloadSourceHost(download({ source: 'file:///repo/a.html' }))).toBe('a local file');
    expect(downloadSourceHost(download())).toBe('');
  });

  it('announces a finished download with Show in folder, which reveals it', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, pages: [] })));
    announceBrowserDownload(finished());
    const toasts = getToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0]).toMatchObject({ type: 'success', message: 'Downloaded report.pdf' });
    expect(toasts[0].action?.label).toBe('Show in folder');
    toasts[0].action?.run();
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][0]).toBe(THREAD);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'download-reveal', downloadId: 'd1' });
  });

  it('announces a saved file AO does not open with its reason and no action', () => {
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'completed', error: 'the saved file is a link' }) } as never);
    const toasts = getToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0]).toMatchObject({ type: 'info', message: 'Downloaded report.pdf; the saved file is a link' });
    expect(toasts[0].action).toBeUndefined();
  });

  it('puts a refused Show in folder from the toast in a toast when no pane is open', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => { throw new Error('browser: report.pdf is gone; downloads are removed after 7 days'); }));
    announceBrowserDownload(finished());
    getToasts()[0].action?.run();
    await vi.waitFor(() => expect(getToasts().some((toast) => toast.type === 'error')).toBe(true));
    expect(getToasts().find((toast) => toast.type === 'error')?.message).toContain('report.pdf is gone');
  });

  it('withdraws Show in folder when a later state shows the list cleared, keeping the toast', () => {
    announceBrowserDownload(finished());
    // A state read before the announcement (no list yet), and a later one
    // of the same list, leave the action.
    applyBrowserCompanionState(state(4, 0));
    applyBrowserCompanionState(state(6, 1));
    expect(showInFolder()).toBe('Show in folder');
    applyBrowserCompanionState(state(7, 2));
    expect(getToasts()).toMatchObject([{ type: 'success', message: 'Downloaded report.pdf' }]);
    expect(getToasts()[0].action).toBeUndefined();
  });

  it('withdraws Show in folder when the thread\'s browser closed, which leaves no list', () => {
    announceBrowserDownload(finished());
    applyBrowserCompanionState(state(6, 0));
    expect(getToasts()[0].action).toBeUndefined();
  });

  it('withdraws Show in folder when a restarted backend sends the thread\'s state', () => {
    __setTransportHelloForTest(hello('launch-1'));
    announceBrowserDownload(finished());
    __setTransportHelloForTest(hello('launch-2'));
    // The restart numbers from one again, under a generation that may repeat.
    applyBrowserCompanionState(state(1, 1));
    expect(getToasts()[0].action).toBeUndefined();
  });

  it('offers no Show in folder for a download its list no longer resolves when announced', () => {
    announceBrowserDownload(finished({ downloadsGen: 0 }));
    expect(getToasts()).toMatchObject([{ type: 'success', message: 'Downloaded report.pdf' }]);
    expect(getToasts()[0].action).toBeUndefined();
    resetToastsForTest();
    // The clear's state overtook the announcement.
    applyBrowserCompanionState(state(6, 2));
    announceBrowserDownload(finished());
    expect(getToasts()[0].action).toBeUndefined();
  });

  it('shows a notice, not an error, when Show in folder names a download no longer listed', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new TransportError('browser_download_unavailable', "That download is no longer in this browser's list");
    }));
    announceBrowserDownload(finished());
    getToasts()[0].action?.run();
    await vi.waitFor(() => expect(getToasts().some((toast) => toast.type === 'info')).toBe(true));
    expect(getToasts().some((toast) => toast.type === 'error')).toBe(false);
  });

  it('announces a failed download as an error with its reason', () => {
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'failed', error: 'the server closed the connection' }) } as never);
    expect(getToasts()).toMatchObject([{ type: 'error', message: 'report.pdf did not download: the server closed the connection' }]);
  });

  it('takes the download kind and leaves the rest to the companion state', () => {
    expect(isBrowserDownloadAnnouncement({ kind: 'download' } as never)).toBe(true);
    expect(isBrowserDownloadAnnouncement({ kind: 'state' } as never)).toBe(false);
    expect(isBrowserDownloadAnnouncement({ kind: 'error' } as never)).toBe(false);
    expect(isBrowserDownloadAnnouncement(null)).toBe(false);
  });

  it('announces nothing on a page that cannot host the browser', () => {
    setHostWindowFromBootstrap(false);
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'completed' }) } as never);
    expect(getToasts()).toHaveLength(0);
  });
});
