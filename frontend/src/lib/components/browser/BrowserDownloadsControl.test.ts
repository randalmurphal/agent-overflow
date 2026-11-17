import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserDownloadsControlHarness from './BrowserDownloadsControlHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { airspaceSurfaces } from '../../utils/paneAirspace.svelte';

const THREAD = 'thread-1';
let seq = 0;
let last: unknown = null;

function download(id: string, extra: Record<string, unknown> = {}) {
  return { id, name: `${id}.bin`, state: 'completed', receivedBytes: 1000, ...extra };
}

function seed(downloads: ReturnType<typeof download>[]) {
  seq += 1;
  last = {
    kind: 'state', threadId: THREAD, activePageId: 'p', visible: true, seq,
    pages: [{ id: 'p', url: 'https://example.test/', title: 'p', canGoBack: false, canGoForward: false }],
    downloads,
  };
  applyBrowserCompanionState(last as never);
}

describe('<BrowserDownloadsControl>', () => {
  let act: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    resetBrowserCompanionForTest();
    seq = 0;
    // An action answers with the state the pane already has.
    act = setBindingMock('BrowserCompanionDo', vi.fn(async () => last));
  });
  afterEach(() => resetBrowserCompanionForTest());

  it('is absent until the thread has a download', async () => {
    seed([]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    expect(view.queryByTestId('browser-downloads')).toBeNull();
    seed([download('a')]);
    await tick();
    expect(view.getByTestId('browser-downloads').getAttribute('aria-label')).toBe('Downloads');
  });

  it('shows the running downloads in steps, and a spinner when their size is unknown', async () => {
    seed([download('a', { state: 'in_progress', receivedBytes: 300, totalBytes: 1000 })]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    expect(view.getByTestId('browser-downloads').getAttribute('aria-label')).toBe('Downloads, 1 running');
    const bar = view.getByTestId('browser-downloads-progress');
    expect(bar.dataset.step).toBe(String(2 / 8));
    expect(bar.style.width).toBe('25%');
    // Progress within the same step leaves the bar where it is.
    seed([download('a', { state: 'in_progress', receivedBytes: 370, totalBytes: 1000 })]);
    await tick();
    expect(view.getByTestId('browser-downloads-progress').style.width).toBe('25%');
    seed([download('a', { state: 'in_progress', receivedBytes: 400 })]);
    await tick();
    expect(view.queryByTestId('browser-downloads-progress')).toBeNull();
    expect(view.getByTestId('browser-downloads').querySelector('svg')).not.toBeNull();
  });

  it('lists each download with the actions its state allows', async () => {
    seed([
      download('run', { state: 'in_progress', receivedBytes: 500, totalBytes: 1000, source: 'https://files.test/x' }),
      download('img', { name: 'chart.png', openable: true }),
      download('zip', { name: 'pack.zip' }),
      download('bad', { state: 'failed', error: 'net::ERR_FAILED' }),
    ]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    await fireEvent.click(view.getByTestId('browser-downloads'));
    const list = await view.findByTestId('browser-downloads-list');
    const rows = within(list).getAllByTestId('browser-download');
    expect(rows.map((row) => row.dataset.state)).toEqual(['in_progress', 'completed', 'completed', 'failed']);
    expect(within(rows[0]).getByTestId('browser-download-status').textContent).toBe('500 B of 1 KB · files.test');
    expect(within(rows[0]).queryByTestId('browser-download-cancel')).not.toBeNull();
    expect(within(rows[0]).queryByTestId('browser-download-reveal')).toBeNull();
    expect(within(rows[1]).queryByTestId('browser-download-open')).not.toBeNull();
    expect(within(rows[1]).queryByTestId('browser-download-reveal')).not.toBeNull();
    expect(within(rows[2]).queryByTestId('browser-download-open')).toBeNull();
    expect(within(rows[2]).queryByTestId('browser-download-reveal')).not.toBeNull();
    expect(within(rows[3]).getByTestId('browser-download-status').textContent).toBe('Failed: net::ERR_FAILED');
    expect(within(rows[3]).queryByRole('button')).toBeNull();

    await fireEvent.click(within(rows[0]).getByTestId('browser-download-cancel'));
    await fireEvent.click(within(rows[2]).getByTestId('browser-download-reveal'));
    await fireEvent.click(within(list).getByTestId('browser-downloads-clear'));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(3));
    expect(act.mock.calls.map((call) => [call[1].kind, call[1].downloadId ?? ''])).toEqual([
      ['download-cancel', 'run'],
      ['download-reveal', 'zip'],
      ['downloads-clear', ''],
    ]);
  });

  it('opens a download in a tab and closes the list', async () => {
    seed([download('img', { name: 'chart.png', openable: true })]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    await fireEvent.click(view.getByTestId('browser-downloads'));
    await fireEvent.click(await view.findByTestId('browser-download-open'));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'download-open', downloadId: 'img' });
    await tick();
    expect(view.queryByTestId('browser-downloads-list')).toBeNull();
  });

  it('registers the open list as airspace, so the native page yields to it', async () => {
    seed([download('a')]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    expect(airspaceSurfaces()).toHaveLength(0);
    await fireEvent.click(view.getByTestId('browser-downloads'));
    const list = await view.findByTestId('browser-downloads-list');
    expect(airspaceSurfaces().some((surface) => surface.contains(list))).toBe(true);
    await fireEvent.click(view.getByTestId('browser-downloads'));
    await waitFor(() => expect(airspaceSurfaces()).toHaveLength(0));
  });

  it('offers no clear while every download is running', async () => {
    seed([download('a', { state: 'in_progress' })]);
    const view = render(BrowserDownloadsControlHarness, { threadId: THREAD });
    await fireEvent.click(view.getByTestId('browser-downloads'));
    await view.findByTestId('browser-downloads-list');
    expect(view.queryByTestId('browser-downloads-clear')).toBeNull();
  });
});
