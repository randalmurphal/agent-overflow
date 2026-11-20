// When the browser pane reports its native view shown, in real Chromium with
// the real pane strip: a report waits for the pane's mount, and the first
// one goes out when the mount appears, even when the pane's state arrived
// before it and nothing moved since; and the view reads hidden while the
// compact thread list is the screen shown, though the strip keeps its
// bounds under it; and a toast over the pane hides the view while it covers
// it, judged again as the stack appears, grows and goes, without taking the
// pane's keyboard commands. The engines show and hide the native
// view from exactly these reports (internal/browser `paneCanShow`).

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import { revealPane } from '../../stores/panes.svelte';
import { applyBrowserCompanionState } from '../../stores/browserCompanion.svelte';
import { getCompactScreen, setCompactLayoutForTest, showCompactList, showCompactThread } from '../../stores/layoutMode.svelte';
import { airspaceSurfaces } from '../../utils/paneAirspace.svelte';
import { addToast, removeToast, resetToastsForTest } from '../../stores/toast.svelte';
import Toast from '../shared/Toast.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { mountBrowserPaneStrip, type BrowserPaneStrip } from '../../../test/helpers/browserPaneStrip';

const THREAD = 'thread-browser';

const shown = {
  kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, seq: 1,
  pages: [{ id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false }],
};

let strip: BrowserPaneStrip;
let host: HTMLDivElement;
let reports: BrowserPaneStrip['reports'];
let answerAttach: () => void;

beforeEach(async () => {
  // The attach answers when the test lets it.
  const attached = new Promise<void>((resolve) => { answerAttach = resolve; });
  strip = await mountBrowserPaneStrip({
    threadId: THREAD,
    attach: async () => {
      await attached;
      return { id: 'mount-1', state: shown };
    },
  });
  ({ host, reports } = strip);
});

afterEach(async () => {
  answerAttach();
  setCompactLayoutForTest(false);
  await strip.teardown();
});

it('reports the rect when the mount arrives after the pane already had its state', async () => {
  await waitFor(() => host.querySelector('[data-testid="browser-pane-host-rect"]') !== null, 'the browser pane mounted');
  // A push lands during the attach: the pane draws the tab, with no mount
  // to report for, so the frames in between report nothing.
  applyBrowserCompanionState(shown);
  await waitFor(() => host.querySelector('input[aria-label="Address"]') !== null, 'the chrome drawn');
  for (let i = 0; i < 4; i += 1) await raf();
  expect(reports).toEqual([]);

  // The attach answers with the same state: nothing on the page moves.
  answerAttach();
  await waitFor(() => reports.length > 0, 'a rect reported for the mount');
  expect(reports.at(-1)).toMatchObject({ mount: 'mount-1', visible: true });
});

it('reports the view hidden while the compact thread list is shown over the strip', async () => {
  answerAttach();
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown');
  // Compact, with the strip resting on the browser pane.
  setCompactLayoutForTest(true);
  revealPane('browser-main');
  await waitFor(() => getCompactScreen() === 'thread' && reports.at(-1)?.visible === true, 'the browser view shown on the thread screen');

  showCompactList();
  await waitFor(() => reports.at(-1)?.visible === false, 'the browser view hidden on the list screen');
  // The strip is hidden by visibility, so it keeps its box.
  const hostRect = host.querySelector<HTMLElement>('[data-testid="browser-pane-host-rect"]')!;
  expect(getComputedStyle(hostRect).visibility).toBe('hidden');
  expect(hostRect.getBoundingClientRect().width).toBeGreaterThan(0);
  for (let i = 0; i < 4; i += 1) await raf();
  expect(reports.at(-1)!.visible).toBe(false);

  showCompactThread();
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown again on the thread screen');
});

it('hides the view while a toast covers it, as the stack appears, grows and goes', async () => {
  // The strip against the window's right edge, so the browser pane, the
  // strip's second pane, sits under the toast corner.
  host.style.cssText = 'position:fixed;top:0;right:0;width:1000px;height:700px;display:flex';
  const toasts = mount(Toast, { target: document.body });
  try {
    answerAttach();
    await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown');

    const first = addToast('success', 'Downloaded report.pdf', 0);
    await waitFor(() => reports.at(-1)?.visible === false, 'the browser view hidden under the toast');
    // A toast is not an overlay: the pane's commands stay live.
    expect(airspaceSurfaces()).toEqual([]);

    // The pane ends just above the stack: shown again.
    const stack = document.querySelector<HTMLElement>('[aria-live="polite"]')!;
    host.style.height = `${Math.floor(stack.getBoundingClientRect().top) - 8}px`;
    await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown above the stack');

    // A second toast grows the same stack up over the pane.
    const second = addToast('info', 'Another', 0);
    await waitFor(() => reports.at(-1)?.visible === false, 'the browser view hidden under the grown stack');

    removeToast(first);
    removeToast(second);
    await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown once the toasts went');
  } finally {
    await unmount(toasts);
    resetToastsForTest();
  }
});
