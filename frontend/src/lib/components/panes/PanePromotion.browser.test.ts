// Promote and demote in real Chromium: the promoted section stays the same
// node, a placeholder holds its slot so nothing else moves, the clip
// reveal runs per frame and is published to native views, the pane body
// sees one size change each way, and the strip does not scroll under a
// promoted pane.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import '../../../app.css';

vi.mock('../chat/ChatView.svelte', async () => ({
  default: (await import('../../../test/mocks/StubBodyChatView.svelte')).default,
}));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import {
  focusPane,
  registerPaneForTest,
  revealPane,
  togglePanePromotion,
} from '../../stores/panes.svelte';
import { installLayoutMode, setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import {
  getPromotedPaneId,
  removePaneLayoutItem,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { promotedLayerClip, promotedLayerFrame } from '../../utils/panePromotion.svelte';
import { airspaceIntersects } from '../../utils/paneAirspace.svelte';
import { getPaneWidth } from '../../stores/layoutMetrics.svelte';
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
});

async function mountStrip(ids: string[], focused: string): Promise<HTMLElement> {
  for (const id of ids) registerPaneForTest(id, createThreadPane({ paneId: id }));
  setPaneLayoutItemsForTest(ids.map((id) => ({ id, paneId: id, kind: 'thread' as const, widthPx: 480 })));
  focusPane(focused);
  app = mount(PaneHost, { target: host });
  const strip = host.querySelector<HTMLElement>('[data-testid="pane-host"]');
  if (!strip) throw new Error('the pane strip did not mount');
  // The first paint places the focused pane from a frame of its own.
  await tick();
  await raf();
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

function section(id: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`[data-pane-id="${id}"]`);
  if (!el) throw new Error(`no section for ${id}`);
  return el;
}

function body(id: string): HTMLElement {
  const el = host.querySelector<HTMLElement>(`[data-body-of="${id}"]`);
  if (!el) throw new Error(`no body for ${id}`);
  return el;
}

function slots(): HTMLElement[] {
  return [...host.querySelectorAll<HTMLElement>('[data-testid="pane-promoted-slot"]')];
}

function rectOf(el: Element): { left: number; top: number; width: number; height: number } {
  const r = el.getBoundingClientRect();
  return { left: r.left, top: r.top, width: r.width, height: r.height };
}

function paddingBox(strip: HTMLElement): { left: number; top: number; width: number; height: number } {
  const r = strip.getBoundingClientRect();
  return { left: r.left + strip.clientLeft, top: r.top + strip.clientTop, width: strip.clientWidth, height: strip.clientHeight };
}

// Everything a demote must put back exactly.
function stripLayout(strip: HTMLElement, ids: string[]) {
  return {
    scrollLeft: strip.scrollLeft,
    scrollWidth: strip.scrollWidth,
    panes: ids.map((id) => ({ id, offset: section(id).offsetLeft, rect: rectOf(section(id)) })),
  };
}

function settledPromoted(id: string): boolean {
  const el = section(id);
  return el.dataset.panePromoted === 'true' && el.style.clipPath === '';
}

function settledDemoted(): boolean {
  return slots().length === 0 && host.querySelectorAll('[data-pane-promoted="true"]').length === 0;
}

// Size changes of an element after its observer's first delivery.
async function observeSizes(el: HTMLElement): Promise<{ sizes: string[]; stop: () => void }> {
  const sizes: string[] = [];
  let primed = false;
  const observer = new ResizeObserver((entries) => {
    const box = entries[0]?.contentRect;
    if (!box) return;
    if (!primed) {
      primed = true;
      return;
    }
    sizes.push(`${box.width}x${box.height}`);
  });
  observer.observe(el);
  await waitFor(() => primed, 'the observer primed');
  return { sizes, stop: () => observer.disconnect() };
}

it('promote lays the pane over the strip in one pass, and demote restores the exact layout', async () => {
  const ids = ['a', 'b', 'c'];
  const strip = await mountStrip(ids, 'b');
  const before = stripLayout(strip, ids);
  const node = section('b');
  const slotRect = rectOf(node);
  const measuredWidth = getPaneWidth('b');
  const bodySize = `${body('b').clientWidth}x${body('b').clientHeight}`;
  const observed = await observeSizes(body('b'));

  togglePanePromotion('b');
  await tick();
  // First frame: the layout has changed once and the clip is the slot.
  expect(section('b')).toBe(node);
  expect(slots()).toHaveLength(1);
  expect(rectOf(slots()[0])).toEqual(slotRect);
  expect(rectOf(node)).toEqual(paddingBox(strip));
  expect(node.style.clipPath).not.toBe('');
  expect(strip.scrollLeft).toBe(before.scrollLeft);
  expect(strip.scrollWidth).toBe(before.scrollWidth);
  // The width a divider drag starts from stays the slot's.
  expect(getPaneWidth('b')).toBe(measuredWidth);
  for (const id of ['a', 'c']) {
    expect(section(id).offsetLeft).toBe(before.panes.find((p) => p.id === id)?.offset);
    expect(rectOf(section(id))).toEqual(before.panes.find((p) => p.id === id)?.rect);
  }

  // The reveal publishes a widening clip every frame until it lands.
  const widths: number[] = [];
  let frames = promotedLayerFrame();
  let frameChanges = 0;
  await waitFor(() => {
    if (promotedLayerFrame() !== frames) frameChanges += 1;
    frames = promotedLayerFrame();
    const clip = promotedLayerClip(body('b'));
    if (clip) widths.push(clip.right - clip.left);
    return settledPromoted('b');
  }, 'the promote landed');
  expect(widths.length).toBeGreaterThan(3);
  for (let i = 1; i < widths.length; i += 1) expect(widths[i]).toBeGreaterThanOrEqual(widths[i - 1]);
  expect(frameChanges).toBeGreaterThan(3);
  expect(promotedLayerClip(body('b'))).toBeNull();
  await raf();
  await raf();
  expect(observed.sizes).toEqual([`${body('b').clientWidth}x${body('b').clientHeight}`]);
  expect(getPaneWidth('b')).toBe(measuredWidth);

  // The promoted layer hides a native view under it, never its own. A view
  // of pane a placed where the strip shows is under the layer.
  const box = paddingBox(strip);
  const aRect = { left: box.left, top: box.top, right: box.left + box.width, bottom: box.top + box.height };
  expect(airspaceIntersects(aRect, section('a'))).toBe(true);
  const own = body('b').getBoundingClientRect();
  expect(airspaceIntersects(own, body('b'))).toBe(false);

  togglePanePromotion('b');
  await waitFor(settledDemoted, 'the demote landed');
  await raf();
  await raf();
  expect(section('b')).toBe(node);
  expect(stripLayout(strip, ids)).toEqual(before);
  expect(observed.sizes).toHaveLength(2);
  expect(observed.sizes[1]).toBe(bodySize);
  expect(airspaceIntersects(aRect, section('a'))).toBe(false);
  observed.stop();
});

it('the strip does not scroll under a promoted pane', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'b');
  const locked = strip.scrollLeft;
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');

  strip.scrollLeft = 0;
  await raf();
  await raf();
  expect(strip.scrollLeft).toBe(locked);
  expect(rectOf(section('b'))).toEqual(paddingBox(strip));

  const wheel = (target: Element, init: WheelEventInit): boolean => {
    const event = new WheelEvent('wheel', { bubbles: true, cancelable: true, ...init });
    target.dispatchEvent(event);
    return event.defaultPrevented;
  };
  const title = section('b').querySelector('[data-testid="chat-header-title"]');
  if (!title) throw new Error('no title');
  expect(wheel(title, { deltaX: 80 })).toBe(true);
  expect(wheel(title, { deltaY: 80, shiftKey: true })).toBe(true);
  expect(wheel(title, { deltaY: 80 })).toBe(false);
  // A horizontal scroller inside the pane keeps its own wheel.
  expect(wheel(body('b').firstElementChild as Element, { deltaX: 80 })).toBe(false);

  togglePanePromotion('b');
  await waitFor(settledDemoted, 'the demote landed');
  expect(wheel(title, { deltaX: 80 })).toBe(false);
});

it('the title drag handle is inert while its pane is promoted', async () => {
  await mountStrip(['a', 'b'], 'b');
  const title = (): HTMLElement => section('b').querySelector<HTMLElement>('[data-testid="chat-header-title"]')!;
  expect(title().draggable).toBe(true);
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');
  expect(title().draggable).toBe(false);
  togglePanePromotion('b');
  await waitFor(settledDemoted, 'the demote landed');
  expect(title().draggable).toBe(true);
});

it('revealing another pane demotes first and then reveals it', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the promote landed');
  revealPane('c');
  expect(getPromotedPaneId()).toBeNull();
  await waitFor(settledDemoted, 'the demote landed');
  const c = section('c');
  await waitFor(
    () => c.offsetLeft + c.offsetWidth <= strip.scrollLeft + strip.clientWidth + 0.5,
    'the revealed pane on screen',
  );
});

it('focusing another pane demotes', async () => {
  await mountStrip(['a', 'b'], 'a');
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the promote landed');
  focusPane('b');
  await tick();
  expect(getPromotedPaneId()).toBeNull();
  await waitFor(settledDemoted, 'the demote landed');
});

it('a demote during the promote reverses and lands at the exact slot; a promote during the demote reverses too', async () => {
  const ids = ['a', 'b', 'c'];
  const strip = await mountStrip(ids, 'b');
  const before = stripLayout(strip, ids);
  togglePanePromotion('b');
  await raf();
  await raf();
  expect(section('b').style.clipPath).not.toBe('');
  togglePanePromotion('b');
  await waitFor(settledDemoted, 'the reversed promote landed');
  expect(stripLayout(strip, ids)).toEqual(before);

  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');
  togglePanePromotion('b');
  await raf();
  await raf();
  expect(slots()).toHaveLength(1);
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the reversed demote landed');
  expect(slots()).toHaveLength(1);
  expect(rectOf(section('b'))).toEqual(paddingBox(strip));
});

it('promoting another pane demotes the first and then promotes the second', async () => {
  const ids = ['a', 'b', 'c'];
  const strip = await mountStrip(ids, 'b');
  const nodeB = section('b');
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'b promoted');
  togglePanePromotion('c');
  await waitFor(() => settledPromoted('c'), 'c promoted');
  expect(slots()).toHaveLength(1);
  expect(slots()[0].dataset.paneSlot).toBe('c');
  expect(section('b')).toBe(nodeB);
  expect(section('b').dataset.panePromoted).toBe('false');
  expect(rectOf(section('c'))).toEqual(paddingBox(strip));
});

it('closing the promoted pane ends the promotion and its layer', async () => {
  await mountStrip(['a', 'b', 'c'], 'b');
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');
  removePaneLayoutItem('b');
  await tick();
  expect(getPromotedPaneId()).toBeNull();
  expect(slots()).toHaveLength(0);
  const a = section('a').getBoundingClientRect();
  expect(airspaceIntersects(a, section('a'))).toBe(false);
});

it('reduced motion lands in one pass with one size change', async () => {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: query.includes('reduced-motion'),
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
  await mountStrip(['a', 'b', 'c'], 'b');
  const observed = await observeSizes(body('b'));
  const frame = promotedLayerFrame();
  togglePanePromotion('b');
  await tick();
  // Landed in the task that asked, with no clip left to animate.
  expect(settledPromoted('b')).toBe(true);
  expect(promotedLayerClip(body('b'))).toBeNull();
  await raf();
  await raf();
  expect(observed.sizes).toHaveLength(1);
  const framesForPromote = promotedLayerFrame() - frame;
  expect(framesForPromote).toBeLessThanOrEqual(2);
  togglePanePromotion('b');
  await tick();
  expect(settledDemoted()).toBe(true);
  await raf();
  await raf();
  expect(observed.sizes).toHaveLength(2);
  observed.stop();
});

it('entering compact clears the promotion at once', async () => {
  const listeners = new Set<() => void>();
  const mql = {
    matches: false,
    addEventListener: (_type: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_type: string, fn: () => void) => listeners.delete(fn),
  };
  vi.stubGlobal('matchMedia', () => mql);
  uninstallLayout = installLayoutMode();
  await mountStrip(['a', 'b'], 'b');
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');
  mql.matches = true;
  for (const fn of listeners) fn();
  await tick();
  expect(settledDemoted()).toBe(true);
  mql.matches = false;
  for (const fn of listeners) fn();
  await tick();
  expect(getPromotedPaneId()).toBeNull();
  expect(settledDemoted()).toBe(true);
});

it('a resize while promoted keeps the pane over the strip and demote finds the new slot', async () => {
  const ids = ['a', 'b', 'c'];
  const strip = await mountStrip(ids, 'c');
  togglePanePromotion('c');
  await waitFor(() => settledPromoted('c'), 'the promote landed');
  // Wider: the strip's scroll range shrinks under its position.
  host.style.width = '1200px';
  await waitFor(() => strip.clientWidth === 1200, 'the strip resized');
  await raf();
  await raf();
  expect(rectOf(section('c'))).toEqual(paddingBox(strip));
  const slot = rectOf(slots()[0]);
  togglePanePromotion('c');
  await waitFor(settledDemoted, 'the demote landed');
  expect(rectOf(section('c'))).toEqual(slot);
});

it('focus moving into a pane under the promoted one demotes and reveals it', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  const before = strip.scrollLeft;
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the promote landed');
  // What Tab does: focus lands on b's first control, scrolling as it goes.
  const title = section('b').querySelector<HTMLElement>('[data-testid="chat-header-title"]');
  if (!title) throw new Error('no title');
  title.focus();
  await waitFor(settledDemoted, 'the demote landed');
  const b = section('b');
  await waitFor(
    () => Math.abs(strip.scrollLeft - (b.offsetLeft + b.offsetWidth - strip.clientWidth)) < 1,
    'the focused pane revealed',
  );
  expect(strip.scrollLeft).not.toBe(before);
  expect(document.activeElement).toBe(title);
});

it('a reveal superseded by a new promotion does not run on a later restore', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  const before = strip.scrollLeft;
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the promote landed');
  revealPane('c');
  await raf();
  await raf();
  expect(section('a').style.clipPath).not.toBe('');
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the re-promote landed');
  togglePanePromotion('a');
  await waitFor(settledDemoted, 'the restore landed');
  for (let i = 0; i < 20; i += 1) await raf();
  expect(strip.scrollLeft).toBe(before);
});

// The clip edges published each frame until `settled` holds.
async function recordClips(id: string, settled: () => boolean): Promise<{ left: number; right: number }[]> {
  const clips: { left: number; right: number }[] = [];
  await waitFor(() => {
    const clip = promotedLayerClip(body(id));
    if (clip) clips.push({ left: clip.left, right: clip.right });
    return settled();
  }, 'the transition landed');
  return clips;
}

function largestStep(clips: { left: number; right: number }[]): number {
  let largest = 0;
  for (let i = 1; i < clips.length; i += 1) {
    largest = Math.max(largest, Math.abs(clips[i].left - clips[i - 1].left), Math.abs(clips[i].right - clips[i - 1].right));
  }
  return largest;
}

it('a resize during a demote retargets the clip to the new slot and lands without a jump', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'c');
  togglePanePromotion('c');
  await waitFor(() => settledPromoted('c'), 'the promote landed');
  togglePanePromotion('c');
  await raf();
  await raf();
  host.style.width = '1200px';
  const clips = await recordClips('c', settledDemoted);
  const landed = section('c').getBoundingClientRect();
  expect(strip.clientWidth).toBe(1200);
  const last = clips.at(-1);
  if (!last) throw new Error('no clip recorded');
  expect(Math.abs(last.left - landed.left)).toBeLessThan(40);
  expect(Math.abs(last.right - landed.right)).toBeLessThan(40);
  expect(largestStep(clips)).toBeLessThan(250);
});

it('a resize during a promote retargets the clip to the new box and slot', async () => {
  host.style.width = '1200px';
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  togglePanePromotion('b');
  await raf();
  await raf();
  host.style.width = '700px';
  const clips: { left: number; right: number; slotRight: number; boxRight: number }[] = [];
  // Only clips the controller published after it could see the new width.
  let resizedAt = -1;
  await waitFor(() => {
    const clip = promotedLayerClip(body('b'));
    const slot = slots()[0]?.getBoundingClientRect();
    const box = paddingBox(strip);
    if (resizedAt < 0 && strip.clientWidth === 700) resizedAt = promotedLayerFrame();
    if (clip && slot && resizedAt >= 0 && promotedLayerFrame() > resizedAt) {
      const boxRight = box.left + box.width;
      clips.push({ left: clip.left, right: clip.right, slotRight: Math.min(slot.right, boxRight), boxRight });
    }
    return settledPromoted('b');
  }, 'the promote landed');
  expect(clips.length).toBeGreaterThan(2);
  for (const clip of clips) {
    expect(clip.right).toBeLessThanOrEqual(clip.boxRight + 0.5);
    expect(clip.right).toBeGreaterThanOrEqual(clip.slotRight - 2);
  }
  expect(largestStep(clips)).toBeLessThan(250);
  expect(rectOf(section('b'))).toEqual(paddingBox(strip));
});

it('a second focus move during the demote is the one revealed', async () => {
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  togglePanePromotion('a');
  await waitFor(() => settledPromoted('a'), 'the promote landed');
  const titleOf = (id: string): HTMLElement => section(id).querySelector<HTMLElement>('[data-testid="chat-header-title"]')!;
  titleOf('b').focus();
  await raf();
  titleOf('c').focus();
  await waitFor(settledDemoted, 'the demote landed');
  const c = section('c');
  await waitFor(
    () => Math.abs(strip.scrollLeft - (c.offsetLeft + c.offsetWidth - strip.clientWidth)) < 1,
    'the last focused pane revealed',
  );
  expect(document.activeElement).toBe(titleOf('c'));
});

interface Sample {
  clip: { left: number; right: number };
  target: { left: number; right: number };
}

// Widens the host by `step` px every frame until `settled`, sampling the
// published clip and the live target after the controller drew the frame.
async function widenEachFrame(id: string, step: number, target: () => { left: number; right: number }, settled: () => boolean): Promise<Sample[]> {
  const samples: Sample[] = [];
  const deadline = performance.now() + 5000;
  while (!settled()) {
    if (performance.now() > deadline) throw new Error('the transition did not land');
    await raf();
    const clip = promotedLayerClip(body(id));
    if (clip) samples.push({ clip: { left: clip.left, right: clip.right }, target: target() });
    host.style.width = `${host.getBoundingClientRect().width + step}px`;
  }
  return samples;
}

function distance(sample: Sample): number {
  return Math.max(Math.abs(sample.clip.left - sample.target.left), Math.abs(sample.clip.right - sample.target.right));
}

function expectConverging(samples: Sample[]): void {
  expect(samples.length).toBeGreaterThan(4);
  for (let i = 1; i < samples.length; i += 1) {
    expect(distance(samples[i])).toBeLessThanOrEqual(distance(samples[i - 1]) + 0.5);
  }
  expect(distance(samples.at(-1)!)).toBeLessThan(30);
}

it('a resize on every frame of a demote still converges on the live slot and lands without a snap', async () => {
  host.style.width = '1200px';
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  togglePanePromotion('b');
  await waitFor(() => settledPromoted('b'), 'the promote landed');
  togglePanePromotion('b');
  await tick();
  const slotTarget = (): { left: number; right: number } => {
    const slot = slots()[0].getBoundingClientRect();
    const box = paddingBox(strip);
    return { left: Math.max(slot.left, box.left), right: Math.min(slot.right, box.left + box.width) };
  };
  const samples = await widenEachFrame('b', 8, slotTarget, settledDemoted);
  expectConverging(samples);
  const landed = section('b').getBoundingClientRect();
  const last = samples.at(-1)!.clip;
  expect(Math.abs(last.left - landed.left)).toBeLessThan(30);
  expect(Math.abs(last.right - landed.right)).toBeLessThan(30);
});

it('a resize on every frame of a promote still converges on the live box', async () => {
  host.style.width = '1200px';
  const strip = await mountStrip(['a', 'b', 'c'], 'a');
  togglePanePromotion('b');
  await tick();
  const boxTarget = (): { left: number; right: number } => {
    const box = paddingBox(strip);
    return { left: box.left, right: box.left + box.width };
  };
  const samples = await widenEachFrame('b', 8, boxTarget, () => settledPromoted('b'));
  expectConverging(samples);
  expect(rectOf(section('b'))).toEqual(paddingBox(strip));
});
