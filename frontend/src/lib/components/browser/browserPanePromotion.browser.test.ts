// The browser pane's geometry under promotion, in real Chromium with the
// real pane strip: a browser pane that is promoted reports its rect cropped
// to the promoted layer's animated clip, a new report every frame of the
// reveal, and is never hidden by its own layer; a browser pane under another
// pane's promoted layer reports itself hidden until the demote lands. The
// engines crop and hide the native view from exactly these reports
// (internal/browser `paneCanShow`).

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from '../panes/PaneHost.svelte';
// Loaded up front, so the companion's lazy import of it resolves at once.
import './BrowserPane.svelte';
import { createPane, focusPane, resetPanesForTest, togglePanePromotion } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { installCompanionPanes, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { promotedLayerClip } from '../../utils/panePromotion.svelte';
import { resetAirspaceForTest } from '../../utils/paneAirspace.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { __setTransportStatusForTest, getTransportStatus } from '../../stores/transportStatus.svelte';

const THREAD = 'thread-browser';
const BROWSER_PANE = 'browser-main';

interface Report {
  x: number; y: number; width: number; height: number;
  clipX: number; clipY: number; clipWidth: number; clipHeight: number;
  visible: boolean;
}

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let reports: Report[];
let transportBefore: ReturnType<typeof getTransportStatus>;

beforeEach(async () => {
  await page.viewport(1400, 700);
  transportBefore = getTransportStatus();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  reports = [];
  resetBrowserCompanionForTest();
  setBindingMock('BrowserCompanionPaneAttach', async () => ({
    id: 'mount-1',
    state: {
      kind: 'state', threadId: THREAD, activePageId: 'a', visible: true,
      pages: [{ id: 'a', url: 'https://example.test/a', title: 'Page a', canGoBack: false, canGoForward: false }],
    },
  }));
  setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
  setBindingMock('BrowserCompanionDo', async () => undefined);
  setBindingMock('BrowserCompanionPaneRect', async (_id: string, rect: Report) => {
    reports.push({ ...rect });
  });
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:1200px;height:500px;display:flex';
  document.body.append(host);
  installCompanionPanes();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 500 }]);
  createPane('main').replaceThread(makeThread({ id: THREAD }));
  openCompanion('main', 'browser');
  app = mount(PaneHost, { target: host });
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown');
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  resetBrowserCompanionForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  resetPanesForTest();
  resetAirspaceForTest();
  resetBindingMocks();
  __setTransportStatusForTest(transportBefore);
});

function hostRect(): HTMLElement {
  const el = host.querySelector<HTMLElement>('[data-testid="browser-pane-host-rect"]');
  if (!el) throw new Error('no browser host rect');
  return el;
}

function section(id: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`section[data-pane-id="${id}"], [data-pane-id="${id}"]`);
  if (!el) throw new Error(`no section for ${id}`);
  return el;
}

function settledPromoted(id: string): boolean {
  const el = section(id);
  return el.dataset.panePromoted === 'true' && el.style.clipPath === '';
}

function settledDemoted(): boolean {
  return host.querySelectorAll('[data-pane-promoted="true"]').length === 0
    && host.querySelectorAll('[data-testid="pane-promoted-slot"]').length === 0;
}

const near = (a: number, b: number): boolean => Math.abs(a - b) <= 0.02;

it('a promoted browser pane reports its rect cropped to the reveal, every frame, and is never hidden by its own layer', async () => {
  focusPane(BROWSER_PANE);
  const before = reports.length;
  togglePanePromotion(BROWSER_PANE);
  const layerClips: { left: number; right: number; top: number; bottom: number }[] = [];
  await waitFor(() => {
    const clip = promotedLayerClip(hostRect());
    if (clip) layerClips.push({ left: clip.left, right: clip.right, top: clip.top, bottom: clip.bottom });
    return settledPromoted(BROWSER_PANE);
  }, 'the promote landed');
  await waitFor(() => {
    const last = reports.at(-1)!;
    return near(last.clipWidth, last.width);
  }, 'the full rect reported');
  const during = reports.slice(before);

  // A new report on each frame of the reveal: as many distinct clips as
  // frames, give or take the frame the RPC was still in flight.
  expect(layerClips.length).toBeGreaterThan(3);
  const widths = new Set(during.map((r) => r.clipWidth));
  expect(widths.size).toBeGreaterThan(3);

  // Every report made while the layer was cropped is the host rect cut by
  // one of the layer's frames, never wider than the layer showed.
  const cropped = during.filter((r) => !near(r.clipWidth, r.width));
  expect(cropped.length).toBeGreaterThan(2);
  for (const r of cropped) {
    const match = layerClips.some((l) =>
      near(r.clipX, Math.max(r.x, l.left))
      && near(r.clipX + r.clipWidth, Math.min(r.x + r.width, l.right)));
    expect(match, `report clip ${r.clipX}+${r.clipWidth} matches a layer frame`).toBe(true);
  }

  // Its own layer never hides it.
  expect(during.every((r) => r.visible)).toBe(true);
  const last = reports.at(-1)!;
  expect(last.visible).toBe(true);
  // Landed: the whole promoted rect, uncropped.
  const box = hostRect().getBoundingClientRect();
  expect(near(last.width, Math.round(box.width * 100) / 100)).toBe(true);
  expect(near(last.clipWidth, last.width)).toBe(true);

  togglePanePromotion(BROWSER_PANE);
  await waitFor(settledDemoted, 'the demote landed');
  await raf();
  await raf();
  expect(reports.at(-1)!.visible).toBe(true);
});

it('a browser pane under another pane\'s promoted layer reports itself hidden until the demote', async () => {
  focusPane('main');
  togglePanePromotion('main');
  await waitFor(() => settledPromoted('main'), 'the promote landed');
  await waitFor(() => reports.at(-1)?.visible === false, 'the browser view hidden under the layer');
  // Still hidden at rest: nothing re-shows it while the layer stands.
  for (let i = 0; i < 4; i += 1) await raf();
  expect(reports.at(-1)!.visible).toBe(false);

  togglePanePromotion('main');
  await waitFor(settledDemoted, 'the demote landed');
  await waitFor(() => reports.at(-1)?.visible === true, 'the browser view shown again');
});

// The host rect takes DOM focus as a stand-in for the page when a key leaves
// the address. It is not a control, and a ring on it would frame the native
// view that sits exactly over it.
it('draws no focus ring on the page area when a key moves focus there', async () => {
  const address = host.querySelector<HTMLInputElement>('input[aria-label="Address"]');
  if (!address) throw new Error('no address field');
  address.focus();
  await userEvent.keyboard('{Escape}');
  await waitFor(() => document.activeElement === hostRect(), 'focus on the page area');
  expect(hostRect().matches(':focus-visible')).toBe(true);
  expect(getComputedStyle(hostRect()).outlineStyle).toBe('none');
});
