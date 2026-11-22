// The address suggestions in real Chromium with the real pane strip: the
// list hangs under the address field, paints over the page area, and the
// pane reports its native view hidden while the list is open and shown once
// it closes, while the browser pane's commands stay live under it (the field
// keeps the keyboard). The engines hide the view from exactly these reports.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { userEvent } from 'vitest/browser';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import { focusPane } from '../../stores/panes.svelte';
import { clearCommandRegistry, isCommandEnabled } from '../../stores/commandRegistry.svelte';
import { registerBrowserCommands } from '../../stores/browserCommands.svelte';
import { makeCommandContext } from '../../stores/builtinCommands.svelte';
import { applyBrowserCompanionState } from '../../stores/browserCompanion.svelte';
import { resetDevServersForTest } from '../../stores/devServers.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { waitFor } from '../../../test/helpers/browserFrames';
import { mountBrowserPaneStrip, type BrowserPaneStrip } from '../../../test/helpers/browserPaneStrip';

const THREAD = 'thread-browser';

let strip: BrowserPaneStrip;
let host: HTMLDivElement;
let reports: BrowserPaneStrip['reports'];

beforeEach(async () => {
  resetDevServersForTest();
  const state = {
    kind: 'state', threadId: THREAD, activePageId: 'a', visible: true,
    pages: [
      { id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false },
      { id: 'b', url: 'https://example.test/b', title: 'Page b', canGoBack: false, canGoForward: false },
    ],
  };
  setBindingMock('BrowserCompanionRecentAddresses', async () => [{ url: 'https://recent.test/', title: 'Recent' }]);
  setBindingMock('GetDevServers', async () => ({ previewHost: '', servers: [] }));
  strip = await mountBrowserPaneStrip({
    threadId: THREAD,
    attach: async () => ({ id: 'mount-1', state }),
    // The thread's state push, which opens a companion in the app.
    beforeMount: () => applyBrowserCompanionState(state as never),
  });
  ({ host, reports } = strip);
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown');
});

afterEach(async () => {
  await strip.teardown();
  resetDevServersForTest();
});

function list(): HTMLElement | null {
  return document.querySelector<HTMLElement>('[data-testid="browser-address-suggestions"]');
}

it('hangs under the address field and hides the native view while open', async () => {
  const address = host.querySelector<HTMLInputElement>('input[aria-label="Address"]');
  if (!address) throw new Error('no address field');
  await userEvent.click(address);
  await userEvent.keyboard('test');
  await waitFor(() => list() !== null && list()!.getBoundingClientRect().height > 0, 'the list open');
  const field = address.getBoundingClientRect();
  const painted = list()!.getBoundingClientRect();
  expect(Math.abs(painted.left - field.left)).toBeLessThanOrEqual(1);
  expect(painted.top).toBeGreaterThanOrEqual(field.bottom);
  expect(painted.top - field.bottom).toBeLessThanOrEqual(8);
  const rect = host.querySelector<HTMLElement>('[data-testid="browser-pane-host-rect"]')!.getBoundingClientRect();
  expect(painted.bottom).toBeGreaterThan(rect.top);
  expect(Array.from(list()!.querySelectorAll('[role="option"]')).map((row) => (row as HTMLElement).title)).toEqual([
    'https://example.test/b',
    'https://recent.test/',
  ]);
  await waitFor(() => reports.at(-1)?.visible === false, 'the native view yields to the list');
  // Reload and tab switching stay the browser's, so Mod+R never falls
  // through to reloading the app.
  clearCommandRegistry();
  registerBrowserCommands();
  focusPane('browser-main');
  for (const id of ['browser.reload', 'browser.nextTab', 'browser.prevTab']) {
    expect(isCommandEnabled(id, makeCommandContext(null, {})), id).toBe(true);
  }
  clearCommandRegistry();

  await userEvent.keyboard('{Escape}');
  await waitFor(() => list() === null, 'the list closed');
  await waitFor(() => reports.at(-1)?.visible === true, 'the native view shown again');
  expect(document.activeElement).toBe(address);
  expect(address.value).toBe('test');
});
