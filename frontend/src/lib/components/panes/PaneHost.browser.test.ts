// The pane strip in real Chromium, where scrollLeft reads back rounded: a
// glide in flight yields to a scroll it did not make, and on compact the
// pane recorded as shown is the pane the strip shows after the layout mode
// or the strip's width changes.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest, revealPane } from '../../stores/panes.svelte';
import {
  compactShownPaneId,
  installLayoutMode,
  isPaneOnScreen,
  onScreenCompactPaneId,
  setCompactLayoutForTest,
} from '../../stores/layoutMode.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

let host: HTMLDivElement;
let app: ReturnType<typeof mount> | null = null;
let uninstallLayout: (() => void) | null = null;

beforeEach(() => {
  host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:500px;height:400px;display:flex';
  document.body.append(host);
});

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host.remove();
  uninstallLayout?.();
  uninstallLayout = null;
  vi.unstubAllGlobals();
  setCompactLayoutForTest(false);
  resetPaneLayoutForTest();
  resetPanesForTest();
});

it('a reveal glide yields to a scroll it did not make', async () => {
  for (const id of ['a', 'b', 'c']) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(['a', 'b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  focusPane('a');
  app = mount(PaneHost, { target: host });
  const strip = host.querySelector<HTMLElement>('[data-testid="pane-host"]');
  const c = host.querySelector<HTMLElement>('[data-pane-id="c"]');
  if (!strip || !c) throw new Error('the pane strip did not mount');
  await waitFor(() => strip.scrollWidth - strip.clientWidth > 600, 'a strip wider than its viewport');
  // The mount places the focused pane; wait for that to settle.
  let settled = -1;
  await waitFor(() => {
    const now = strip.scrollLeft;
    const still = now === settled;
    settled = now;
    return still;
  }, 'the strip at rest');
  const before = strip.scrollLeft;

  // Focusing the last pane starts a glide toward it.
  c.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, isPrimary: true }));
  await waitFor(() => strip.scrollLeft > before + 20, 'the glide under way');
  const target = c.offsetLeft + c.offsetWidth - strip.clientWidth;
  expect(strip.scrollLeft).toBeLessThan(target - 20);

  // A scroll-into-view moves the strip mid-glide.
  strip.scrollLeft = before;
  const placed = strip.scrollLeft;
  for (let i = 0; i < 20; i += 1) await raf();
  expect(strip.scrollLeft).toBe(placed);
});

// The compact query, driven by the test: a viewport crossing it is the
// media query's change event, as a narrowed window or a rotation delivers.
function installViewportQuery(matches: boolean): (next: boolean) => void {
  const listeners = new Set<() => void>();
  const mql = {
    matches,
    addEventListener: (_type: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_type: string, fn: () => void) => listeners.delete(fn),
  };
  vi.stubGlobal('matchMedia', () => mql);
  uninstallLayout = installLayoutMode();
  return (next) => {
    mql.matches = next;
    for (const fn of listeners) fn();
  };
}

async function mountStrip(ids: string[], width: number): Promise<HTMLElement> {
  host.style.width = `${width}px`;
  for (const id of ids) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(ids.map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  focusPane(ids[0]);
  app = mount(PaneHost, { target: host });
  const strip = host.querySelector<HTMLElement>('[data-testid="pane-host"]');
  if (!strip) throw new Error('the pane strip did not mount');
  await stripAtRest(strip);
  return strip;
}

async function stripAtRest(strip: HTMLElement): Promise<void> {
  let settled = -1;
  await waitFor(() => {
    const now = strip.scrollLeft;
    const still = now === settled;
    settled = now;
    return still;
  }, 'the strip at rest');
}

function paneLeft(id: string): number {
  return host.querySelector<HTMLElement>(`[data-pane-id="${id}"]`)?.offsetLeft ?? -1;
}

// The record and the strip agree: the pane recorded as shown is the one
// under the strip, aligned to it, and the only pane on screen.
function expectShown(strip: HTMLElement, id: string, others: string[]): void {
  expect(compactShownPaneId()).toBe(id);
  expect(onScreenCompactPaneId()).toBe(id);
  expect(strip.scrollLeft).toBe(paneLeft(id));
  expect(isPaneOnScreen(id)).toBe(true);
  for (const other of others) expect(isPaneOnScreen(other)).toBe(false);
}

it('entering compact records the pane the strip shows, though a reveal named the other', async () => {
  const enterCompact = installViewportQuery(false);
  const strip = await mountStrip(['thread', 'companion'], 1200);
  // Both panes fit the wide strip, so revealing the companion records it
  // and leaves the strip at its start.
  revealPane('companion');
  await stripAtRest(strip);
  expect(strip.scrollLeft).toBe(0);
  expect(compactShownPaneId()).toBe('companion');

  enterCompact(true);
  await raf();
  await stripAtRest(strip);
  expectShown(strip, 'thread', ['companion']);
});

it('entering compact records the pane the strip shows, though a reveal named the first', async () => {
  const enterCompact = installViewportQuery(false);
  const strip = await mountStrip(['thread', 'companion'], 600);
  revealPane('thread');
  await stripAtRest(strip);
  // The person scrolled the wide strip toward the companion.
  strip.scrollLeft = 360;
  await stripAtRest(strip);
  expect(compactShownPaneId()).toBe('thread');

  enterCompact(true);
  await raf();
  await stripAtRest(strip);
  // The strip reflowed with the companion under its centre.
  expectShown(strip, 'companion', ['thread']);
});

it('a rotation keeps the recorded pane on a strip that widened under it', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 360);
  revealPane('c');
  await waitFor(() => strip.scrollLeft === paneLeft('c'), 'the strip at c');
  expectShown(strip, 'c', ['a', 'b']);

  // At the wider width the old offset puts b under the centre.
  host.style.width = '640px';
  await raf();
  await stripAtRest(strip);
  expectShown(strip, 'c', ['a', 'b']);
});

it('a rotation keeps the recorded pane on a strip that narrowed under it', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 640);
  revealPane('b');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  expectShown(strip, 'b', ['a', 'c']);

  // At the narrower width the old offset puts c under the centre.
  host.style.width = '360px';
  await raf();
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a', 'c']);
});

// The reconcile after the strip gains a pane leaves a reveal in flight to
// land: the record already names the new pane, and its glide follows.
it('a pane opened by a reveal on compact glides in rather than snapping', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  registerPaneForTest('c', createThreadPane({ paneId: 'c' }));
  setPaneLayoutItemsForTest(['a', 'b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  revealPane('c');
  let midGlide = false;
  await waitFor(() => {
    const target = paneLeft('c');
    if (strip.scrollLeft > 0 && strip.scrollLeft < target) midGlide = true;
    return strip.scrollLeft === target;
  }, 'the strip at c');
  expect(midGlide).toBe(true);
  expectShown(strip, 'c', ['a', 'b']);
});
