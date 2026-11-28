// The pane strip in real Chromium, where scrollLeft reads back rounded: a
// glide in flight yields to a scroll it did not make, and on compact the
// pane recorded as shown is the pane the strip shows after the layout mode
// or the strip's width changes, only a reveal moves the strip, and only the
// pane it shows or reveals is reachable.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, revealPane } from '../../stores/panes.svelte';
import {
  compactShownPaneId,
  installLayoutMode,
  onScreenCompactPaneId,
  setCompactLayoutForTest,
  showCompactThread,
} from '../../stores/layoutMode.svelte';
import { isPaneOnScreen, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { userEvent } from 'vitest/browser';
import { carryCaretIntoPane } from './paneComposerFocus';

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

async function mountStrip(ids: string[], width: number, focused = ids[0]): Promise<HTMLElement> {
  host.style.width = `${width}px`;
  for (const id of ids) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(ids.map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  focusPane(focused);
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

function section(id: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`[data-pane-id="${id}"]`);
  if (!el) throw new Error(`no pane ${id}`);
  return el;
}

// The record and the strip agree: the pane recorded as shown is the one
// under the strip, aligned to it, the only pane on screen and the only
// one reachable.
function expectShown(strip: HTMLElement, id: string, others: string[]): void {
  expect(compactShownPaneId()).toBe(id);
  expect(onScreenCompactPaneId()).toBe(id);
  expect(strip.scrollLeft).toBe(paneLeft(id));
  expect(isPaneOnScreen(id)).toBe(true);
  expect(section(id).inert).toBe(false);
  for (const other of others) {
    expect(isPaneOnScreen(other)).toBe(false);
    expect(section(other).inert).toBe(true);
  }
}

// The pane a focused element is in, null outside every pane.
function focusedPane(): string | null {
  return document.activeElement?.closest<HTMLElement>('[data-pane-id]')?.dataset.paneId ?? null;
}

it('entering compact records the pane the strip shows, though a reveal named the other', async () => {
  const enterCompact = installViewportQuery(false);
  const strip = await mountStrip(['thread', 'companion'], 1200);
  // Both panes fit the wide strip, so revealing the companion leaves the
  // strip at its start; the desktop keeps no record.
  revealPane('companion');
  await stripAtRest(strip);
  expect(strip.scrollLeft).toBe(0);
  expect(compactShownPaneId()).toBeNull();

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
  expect(compactShownPaneId()).toBeNull();

  enterCompact(true);
  await raf();
  await stripAtRest(strip);
  // The strip reflowed with the companion under its centre.
  expectShown(strip, 'companion', ['thread']);
});

// At the new width the old offset puts the pane's neighbour under the centre.
it.each([
  { change: 'widened', from: 360, to: 640, shown: 'c', others: ['a', 'b'] },
  { change: 'narrowed', from: 640, to: 360, shown: 'b', others: ['a', 'c'] },
])('a rotation keeps the recorded pane on a strip that $change under it', async ({ from, to, shown, others }) => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], from);
  revealPane(shown);
  await waitFor(() => strip.scrollLeft === paneLeft(shown), `the strip at ${shown}`);
  expectShown(strip, shown, others);

  host.style.width = `${to}px`;
  await raf();
  await stripAtRest(strip);
  expectShown(strip, shown, others);
});

// Narrowing clamps the strip. An engine may deliver the clamp's scroll
// event before the ResizeObserver callback (Chromium here runs the observer
// first, so the test delivers it): at the new width against the offsets of
// the old one, the strip's centre lies over b.
it('a rotation that clamps the strip under the last pane keeps that pane', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 640);
  revealPane('c');
  await waitFor(() => strip.scrollLeft === paneLeft('c'), 'the strip at c');
  await stripAtRest(strip);
  expectShown(strip, 'c', ['a', 'b']);

  host.style.width = '360px';
  expect(strip.scrollLeft).toBe(720);
  strip.dispatchEvent(new Event('scroll'));
  expect(compactShownPaneId()).toBe('c');
  await stripAtRest(strip);
  expectShown(strip, 'c', ['a', 'b']);
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

it('a reveal on compact names its pane once the strip lands there, not before', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b']);

  revealPane('b');
  // The strip has not moved yet, so the record still names a.
  expect(compactShownPaneId()).toBe('a');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
});

// The compact counterpart of the interrupted glide above. Nobody scrolls
// the compact strip by hand, so a scroll under a glide is a control
// scrolled into view, in the pane gliding in or the one it leaves, and
// the reveal the person asked for still lands.
it.each([
  { where: 'back on the first pane', left: 0 },
  { where: 'between the two panes', left: 86 },
  { where: 'short of the pane revealed', left: 348 },
])('a reveal on compact lands though a scroll left the strip $where, through a resize', async ({ left }) => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);

  revealPane('b');
  await waitFor(() => strip.scrollLeft > 20, 'the glide under way');
  strip.scrollLeft = left;
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);

  // The keyboard opens: the strip shortens.
  host.style.height = '250px';
  await raf();
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
});

// A control placed in a pane, 100px from its left edge.
function controlIn(id: string): HTMLButtonElement {
  const pane = section(id);
  pane.style.position = 'relative';
  const control = document.createElement('button');
  control.style.cssText = 'position:absolute;left:100px;top:0;width:40px;height:10px';
  pane.append(control);
  return control;
}

it('a reveal on compact lands though a control of its pane is scrolled into view on the way', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  const control = controlIn('b');

  revealPane('b');
  await waitFor(() => strip.scrollLeft > 20, 'the glide under way');
  const before = strip.scrollLeft;
  expect(before).toBeLessThan(140);
  control.scrollIntoView({ block: 'nearest' });
  // The browser moved the strip just far enough to show the control.
  expect(strip.scrollLeft).toBe(140);
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
});

it('a horizontal wheel over the compact strip does not stop a reveal', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);

  revealPane('b');
  await waitFor(() => strip.scrollLeft > 20, 'the glide under way');
  strip.dispatchEvent(new WheelEvent('wheel', { deltaX: 30, bubbles: true }));
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
});

// The glide has no pane to land on, and the strip may not rest where it
// stopped, between a and the pane that moved up beside it.
it.each([
  { where: 'before its middle', past: 20, shown: 'a', other: 'c' },
  { where: 'past its middle', past: 200, shown: 'c', other: 'a' },
])('a pane closed while the compact strip glides to it, $where, leaves the strip on a pane', async ({ past, shown, other }) => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 360);
  revealPane('a');
  await stripAtRest(strip);

  revealPane('b');
  await waitFor(() => strip.scrollLeft > past, 'the glide under way');
  expect(strip.scrollLeft).toBeLessThan(past === 20 ? 180 : paneLeft('b'));
  setPaneLayoutItemsForTest(['a', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  // In the flush that removes the pane, before a frame or a scroll event.
  await tick();
  expectShown(strip, shown, [other]);
  await stripAtRest(strip);
  expectShown(strip, shown, [other]);
});

// The snap that places the focused pane records it in its own task, so the
// pane is reachable before the snap's scroll event: a composer that mounts
// in that frame can take the caret.
it('the compact strip first shows the focused pane, reachable as soon as the strip is on it', async () => {
  installViewportQuery(true);
  let atSnap: { recorded: string | null; inert: boolean } | null = null;
  const seen = (event: Event): void => {
    const strip = event.target;
    if (atSnap || !(strip instanceof HTMLElement) || strip.dataset.testid !== 'pane-host' || strip.scrollLeft === 0) return;
    atSnap = { recorded: compactShownPaneId(), inert: section('b').inert };
  };
  // Capture: before the strip's own scroll listener measures.
  document.addEventListener('scroll', seen, true);
  try {
    const strip = await mountStrip(['a', 'b'], 360, 'b');
    showCompactThread();
    await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
    await stripAtRest(strip);
    expectShown(strip, 'b', ['a']);
    expect(atSnap).toEqual({ recorded: 'b', inert: false });
  } finally {
    document.removeEventListener('scroll', seen, true);
  }
});

// The strip keeps its offset, so no scroll event follows: the structure
// change itself measures.
it('closing a pane before the one on screen on compact names the pane that moved under the strip', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 360);
  revealPane('b');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a', 'c']);

  setPaneLayoutItemsForTest(['b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  await raf();
  await stripAtRest(strip);
  expectShown(strip, 'c', ['b']);
});

// A resting strip is put back in the scroll's own event: the record never
// names the other pane, and the frame after the scroll paints the strip
// where it was.
it.each([
  { where: 'between the two panes', left: 86 },
  { where: 'on the other pane', left: 360 },
])('a scroll on compact that no reveal made, to $where, is undone', async ({ left }) => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b']);
  const recorded: (string | null)[] = [];
  strip.addEventListener('scroll', () => recorded.push(compactShownPaneId()));

  strip.scrollLeft = left;
  await raf();
  expect(strip.scrollLeft).toBe(0);
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b']);
  expect(recorded.length).toBeGreaterThan(0);
  expect(recorded.every((id) => id === 'a')).toBe(true);
});

// What moves a resting strip in the app: a pane the strip does not show
// scrolls one of its controls into view.
it('a control of a pane the compact strip does not show, scrolled into view, leaves the strip on its pane', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  const control = controlIn('b');

  control.scrollIntoView({ block: 'nearest' });
  // The browser moved the strip to show the control.
  expect(strip.scrollLeft).toBeGreaterThan(0);
  await raf();
  expect(strip.scrollLeft).toBe(0);
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b']);
});

// A pane the compact strip does not show is out of reach, as the screen
// not shown is: no script focuses into it and Tab passes over it.
it('a pane the compact strip does not show takes no focus, from a script or from Tab', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b', 'c'], 360);
  revealPane('a');
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b', 'c']);
  const shown = controlIn('a');
  const hidden = controlIn('b');

  hidden.focus();
  expect(document.activeElement).not.toBe(hidden);
  expect(strip.scrollLeft).toBe(0);

  shown.focus();
  expect(document.activeElement).toBe(shown);
  for (let i = 0; i < 4; i += 1) {
    await userEvent.keyboard('{Tab}');
    expect([null, 'a']).toContain(focusedPane());
  }
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b', 'c']);
});

// A pane move reveals its pane and then puts the caret in it, in one task
// (paneNavigation): the pane is reachable from the request, not from the
// frame the strip starts moving in.
it('the pane a compact reveal is asked for takes focus in the same task, and the pane it leaves loses it', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  const left = controlIn('a');
  const reached = controlIn('b');
  left.focus();
  expect(document.activeElement).toBe(left);

  revealPane('b');
  expect(section('b').inert).toBe(false);
  reached.focus({ preventScroll: true });
  expect(document.activeElement).toBe(reached);
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
  expect(document.activeElement).toBe(reached);

  // Back on a: the control that held focus in b, now off screen, loses it.
  revealPane('a');
  await waitFor(() => strip.scrollLeft === paneLeft('a'), 'the strip at a');
  await stripAtRest(strip);
  expectShown(strip, 'a', ['b']);
  await waitFor(() => document.activeElement !== reached, 'focus gone from the pane left');
});

it('a caret carried into a pane on compact brings the strip to it', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  controlIn('a').focus();
  expect(focusedPane()).toBe('a');

  carryCaretIntoPane('b');
  expect(focusedPane()).toBe('b');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
  expect(focusedPane()).toBe('b');
});

it('a pane added off screen on compact is out of reach from its first frame', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);
  registerPaneForTest('c', createThreadPane({ paneId: 'c' }));
  setPaneLayoutItemsForTest(['a', 'b', 'c'].map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  await waitFor(() => host.querySelector('[data-pane-id="c"]') !== null, 'pane c mounted');
  expectShown(strip, 'a', ['b', 'c']);
});

it('a resize during a reveal on compact lets the reveal land', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('a');
  await stripAtRest(strip);

  revealPane('b');
  await waitFor(() => strip.scrollLeft > 20, 'the glide under way');
  host.style.height = '250px';
  await raf();
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expectShown(strip, 'b', ['a']);
});

it('leaving compact ends the record of the pane it showed', async () => {
  const setCompact = installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('b');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expect(compactShownPaneId()).toBe('b');

  setCompact(false);
  await raf();
  expect(compactShownPaneId()).toBeNull();
  expect(isPaneOnScreen('a')).toBe(true);
  expect(isPaneOnScreen('b')).toBe(true);
  expect(section('a').inert).toBe(false);
  expect(section('b').inert).toBe(false);
});

it('unmounting the compact strip ends the record of the pane it showed', async () => {
  installViewportQuery(true);
  const strip = await mountStrip(['a', 'b'], 360);
  revealPane('b');
  await waitFor(() => strip.scrollLeft === paneLeft('b'), 'the strip at b');
  await stripAtRest(strip);
  expect(compactShownPaneId()).toBe('b');

  if (app) await unmount(app);
  app = null;
  expect(compactShownPaneId()).toBeNull();
});
