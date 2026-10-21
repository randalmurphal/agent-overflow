// The Pull requests page in real Chromium: the list column never takes
// the detail's room, however wide the remembered width, and widens back
// when the pane does; its handle resizes from the keyboard through the
// same clamp; and Escape on the header's agent menu returns focus to the
// button that opened it.

import { afterEach, beforeEach, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import PullsPane from './PullsPane.svelte';
import { DETAIL_MIN_PX, LIST_KEY_STEP_PX, LIST_MAX_PX, LIST_MIN_PX } from './listWidth.svelte';
import { appStorageGet, appStorageSet } from '../../stores/appStorage';
import { __setTransportStatusForTest, getTransportStatus } from '../../stores/transportStatus.svelte';
import { PULLS_PANE, installPullsPaneMocks, pullRowKey, resetPullsPane } from '../../../test/helpers/pullsPane';
import { waitFor } from '../../../test/helpers/browserFrames';

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let transportBefore: ReturnType<typeof getTransportStatus>;

function q(testId: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`[data-testid="${testId}"]`);
  if (!el) throw new Error(`no ${testId}`);
  return el;
}

function width(testId: string): number {
  return Math.round(q(testId).getBoundingClientRect().width);
}

async function mountAt(px: number): Promise<void> {
  host.style.width = `${px}px`;
  app = mount(PullsPane, { target: host, props: { paneId: PULLS_PANE } });
  await waitFor(() => host.querySelectorAll('[data-testid="pull-row"]').length === 2, 'the rows listed');
}

beforeEach(() => {
  // The list subscription waits for a connected transport; this suite has none.
  transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  resetPullsPane();
  installPullsPaneMocks();
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:1200px;height:600px;display:flex;flex-direction:column';
  document.body.append(host);
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  resetPullsPane();
  __setTransportStatusForTest(transportBefore);
});

it('narrows a remembered wide list to leave the detail its room, and widens it back', async () => {
  appStorageSet('pullsListWidth', '700');
  await mountAt(1200);
  await waitFor(() => width('pulls-list-column') === 700, 'the remembered width');

  host.style.width = '760px';
  await waitFor(() => width('pulls-list-column') === 760 - DETAIL_MIN_PX, 'the list narrowed');
  expect(width('pulls-detail')).toBeGreaterThanOrEqual(DETAIL_MIN_PX);

  // Too narrow for both: they split evenly, and the handle stays inside.
  host.style.width = '500px';
  await waitFor(() => width('pulls-list-column') === 250, 'the even split');
  expect(width('pulls-detail')).toBe(250);
  const handle = q('pulls-list-resize').getBoundingClientRect();
  expect(handle.right).toBeLessThanOrEqual(host.getBoundingClientRect().right);
  expect(handle.left).toBeGreaterThan(0);
  expect(appStorageGet('pullsListWidth')).toBe('700');

  host.style.width = '1200px';
  await waitFor(() => width('pulls-list-column') === 700, 'the request back');
});

it('resizes from the keyboard through the same clamp, with its value announced', async () => {
  await mountAt(1000);
  const handle = q('pulls-list-resize');
  handle.focus();
  expect(document.activeElement).toBe(handle);
  expect(handle.getAttribute('aria-orientation')).toBe('vertical');
  expect(handle.getAttribute('aria-valuemin')).toBe(String(LIST_MIN_PX));
  expect(handle.getAttribute('aria-valuemax')).toBe(String(Math.min(LIST_MAX_PX, 1000 - DETAIL_MIN_PX)));
  const start = Number(handle.getAttribute('aria-valuenow'));

  await userEvent.keyboard('{ArrowRight}');
  await waitFor(() => width('pulls-list-column') === start + LIST_KEY_STEP_PX, 'one step wider');
  expect(handle.getAttribute('aria-valuenow')).toBe(String(start + LIST_KEY_STEP_PX));
  await userEvent.keyboard('{ArrowLeft}{ArrowLeft}');
  await waitFor(() => width('pulls-list-column') === start - LIST_KEY_STEP_PX, 'two steps narrower');

  await userEvent.keyboard('{End}');
  await waitFor(() => width('pulls-list-column') === 1000 - DETAIL_MIN_PX, 'the widest the pane allows');
  expect(appStorageGet('pullsListWidth')).toBe(String(1000 - DETAIL_MIN_PX));
  await userEvent.keyboard('{Home}');
  await waitFor(() => width('pulls-list-column') === LIST_MIN_PX, 'the narrowest');
  expect(appStorageGet('pullsListWidth')).toBe(String(LIST_MIN_PX));

  // Page Down and Page Up move four steps.
  await userEvent.keyboard('{PageDown}');
  await waitFor(() => width('pulls-list-column') === LIST_MIN_PX + 4 * LIST_KEY_STEP_PX, 'four steps wider');
  expect(handle.getAttribute('aria-valuenow')).toBe(String(LIST_MIN_PX + 4 * LIST_KEY_STEP_PX));
  await userEvent.keyboard('{PageUp}');
  await waitFor(() => width('pulls-list-column') === LIST_MIN_PX, 'four steps narrower');
  expect(appStorageGet('pullsListWidth')).toBe(String(LIST_MIN_PX));
});

it("returns focus to the header's agent button on Escape", async () => {
  await mountAt(1200);
  const row = host.querySelector<HTMLElement>(`[data-testid="pull-row"][data-pr-key="${pullRowKey(5)}"] button`)!;
  row.click();
  flushSync();
  await waitFor(() => host.querySelector('[data-testid="pulls-open-with-agent"]') !== null, 'the agent button');
  const trigger = q('pulls-open-with-agent');
  trigger.focus();
  trigger.click();
  await waitFor(() => document.querySelector('[data-testid="open-with-agent-new-thread"]') !== null, 'the menu');
  await userEvent.keyboard('{Escape}');
  await waitFor(() => document.querySelector('[data-testid="open-with-agent-new-thread"]') === null, 'the menu closed');
  expect(document.activeElement).toBe(trigger);
});
