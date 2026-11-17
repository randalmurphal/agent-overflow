import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { BrowserDownload } from './bindings';
import { resetBrowserCompanionForTest } from './browserCompanion.svelte';
import {
  announceBrowserDownload,
  downloadProgressStep,
  isBrowserDownloadAnnouncement,
  downloadSourceHost,
  downloadStatus,
} from './browserDownloads.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { setHostWindowFromBootstrap } from '../transport/pageHost';

const THREAD = 'thread-1';

function download(extra: Partial<BrowserDownload> = {}): BrowserDownload {
  return { id: 'd1', name: 'report.pdf', state: 'in_progress', receivedBytes: 0, ...extra } as BrowserDownload;
}

describe('browser downloads', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    resetToastsForTest();
  });
  afterEach(() => {
    resetToastsForTest();
    resetBrowserCompanionForTest();
    setHostWindowFromBootstrap(true);
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
    expect(downloadStatus(download({ state: 'completed', receivedBytes: 912 }))).toBe('912 B');
    expect(downloadStatus(download({ state: 'failed', error: 'net::ERR_FAILED' }))).toBe('Failed: net::ERR_FAILED');
    expect(downloadStatus(download({ state: 'canceled' }))).toBe('Canceled');
    expect(downloadStatus(download({ state: 'canceled', error: 'page closed' }))).toBe('Canceled: page closed');
    expect(downloadSourceHost(download({ source: 'https://example.test:8080/files' }))).toBe('example.test:8080');
    expect(downloadSourceHost(download({ source: 'file:///repo/a.html' }))).toBe('a local file');
    expect(downloadSourceHost(download())).toBe('');
  });

  it('announces a finished download with Show in folder, which reveals it', async () => {
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => ({ kind: 'state', threadId: THREAD, pages: [] })));
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'completed', receivedBytes: 10 }) } as never);
    const toasts = getToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0]).toMatchObject({ type: 'success', message: 'Downloaded report.pdf' });
    expect(toasts[0].action?.label).toBe('Show in folder');
    toasts[0].action?.run();
    await vi.waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][0]).toBe(THREAD);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'download-reveal', downloadId: 'd1' });
  });

  it('puts a refused Show in folder from the toast in a toast when no pane is open', async () => {
    setBindingMock('BrowserCompanionDo', vi.fn(async () => { throw new Error('browser: report.pdf is gone; downloads are removed after 7 days'); }));
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'completed' }) } as never);
    getToasts()[0].action?.run();
    await vi.waitFor(() => expect(getToasts().some((toast) => toast.type === 'error')).toBe(true));
    expect(getToasts().find((toast) => toast.type === 'error')?.message).toContain('report.pdf is gone');
  });

  it('announces a failed download as an error with its reason', () => {
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'failed', error: 'the server closed the connection' }) } as never);
    expect(getToasts()).toMatchObject([{ type: 'error', message: 'report.pdf did not download: the server closed the connection' }]);
  });

  it('takes the download and downloads-unmanaged kinds and leaves the rest to the companion state', () => {
    expect(isBrowserDownloadAnnouncement({ kind: 'download' } as never)).toBe(true);
    expect(isBrowserDownloadAnnouncement({ kind: 'downloads-unmanaged' } as never)).toBe(true);
    expect(isBrowserDownloadAnnouncement({ kind: 'state' } as never)).toBe(false);
    expect(isBrowserDownloadAnnouncement({ kind: 'error' } as never)).toBe(false);
    expect(isBrowserDownloadAnnouncement(null)).toBe(false);
  });

  it('announces downloads that are off in a profile as an error with the reason', () => {
    const error = 'Downloads are off in this browser profile: the pane browser refused AO\'s download directory';
    announceBrowserDownload({ kind: 'downloads-unmanaged', threadId: THREAD, error } as never);
    expect(getToasts()).toMatchObject([{ type: 'error', message: error }]);
  });

  it('announces nothing on a page that cannot host the browser', () => {
    setHostWindowFromBootstrap(false);
    announceBrowserDownload({ kind: 'download', threadId: THREAD, download: download({ state: 'completed' }) } as never);
    announceBrowserDownload({ kind: 'downloads-unmanaged', threadId: THREAD, error: 'off' } as never);
    expect(getToasts()).toHaveLength(0);
  });
});
