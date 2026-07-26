import { describe, expect, it } from 'vitest';
import {
  registerNestedScroller,
  scrollDeltaConsumedBelow,
  touchDragConsumedBelow,
  wheelConsumedBelow,
} from './wheelAttribution';

// happy-dom reports zero geometry, so every scrollable box in these tests
// declares its own. `scrollTop` is a plain writable property; the other two
// need getters.
function scroller(
  parent: Element,
  geometry: { scrollTop: number; clientHeight: number; scrollHeight: number },
): HTMLDivElement {
  const el = document.createElement('div');
  parent.appendChild(el);
  el.scrollTop = geometry.scrollTop;
  Object.defineProperty(el, 'clientHeight', {
    configurable: true,
    get: () => geometry.clientHeight,
  });
  Object.defineProperty(el, 'scrollHeight', {
    configurable: true,
    get: () => geometry.scrollHeight,
  });
  return el;
}

function tree(inner: { scrollTop: number; clientHeight?: number; scrollHeight?: number }) {
  const boundary = document.createElement('div');
  document.body.appendChild(boundary);
  const box = scroller(boundary, {
    scrollTop: inner.scrollTop,
    clientHeight: inner.clientHeight ?? 100,
    scrollHeight: inner.scrollHeight ?? 500,
  });
  const leaf = document.createElement('span');
  box.appendChild(leaf);
  return { boundary, box, leaf };
}

const UP = -10;
const DOWN = 10;

describe('scrollDeltaConsumedBelow', () => {
  it('attributes an upward gesture to a registered box that can still scroll up', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 200 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(true);
  });

  it('chains outward when the registered box is already at its top', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 0 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });

  it('attributes a downward gesture to a box with room below', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 0, clientHeight: 100, scrollHeight: 500 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, DOWN)).toBe(true);
  });

  it('chains outward when the registered box is already at its bottom', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 400, clientHeight: 100, scrollHeight: 500 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, DOWN)).toBe(false);
  });

  it('ignores unregistered scrollable ancestors', () => {
    const { boundary, leaf } = tree({ scrollTop: 200 });

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });

  it('never attributes to the boundary itself', () => {
    const { boundary, leaf } = tree({ scrollTop: 0 });
    boundary.scrollTop = 300;
    registerNestedScroller(boundary);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });

  it('stops at the boundary rather than consulting scrollers above it', () => {
    const outer = scroller(document.body, {
      scrollTop: 300,
      clientHeight: 100,
      scrollHeight: 900,
    });
    registerNestedScroller(outer);
    const boundary = document.createElement('div');
    outer.appendChild(boundary);
    const leaf = document.createElement('span');
    boundary.appendChild(leaf);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });

  it('picks the nearest capable scroller when boxes are stacked', () => {
    const { boundary, box } = tree({ scrollTop: 0 });
    registerNestedScroller(box);
    const innerBox = scroller(box, { scrollTop: 50, clientHeight: 50, scrollHeight: 300 });
    registerNestedScroller(innerBox);
    const leaf = document.createElement('span');
    innerBox.appendChild(leaf);

    // Inner can scroll up, outer box cannot — the inner one owns it.
    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(true);
  });

  it('skips an exhausted inner box and attributes to a capable one further up', () => {
    const { boundary, box } = tree({ scrollTop: 200 });
    registerNestedScroller(box);
    const innerBox = scroller(box, { scrollTop: 0, clientHeight: 50, scrollHeight: 300 });
    registerNestedScroller(innerBox);
    const leaf = document.createElement('span');
    innerBox.appendChild(leaf);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(true);
  });

  it('treats a sub-pixel resting offset as pinned, not scrollable', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 0.4 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });

  it('returns false for a zero delta', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 200 });
    registerNestedScroller(box);

    expect(scrollDeltaConsumedBelow(leaf, boundary, 0)).toBe(false);
  });

  it('returns false for a target outside the boundary subtree', () => {
    const { boundary } = tree({ scrollTop: 200 });
    const orphan = document.createElement('span');
    document.body.appendChild(orphan);

    expect(scrollDeltaConsumedBelow(orphan, boundary, UP)).toBe(false);
  });

  it('returns false for a non-element target', () => {
    const { boundary } = tree({ scrollTop: 200 });

    expect(scrollDeltaConsumedBelow(null, boundary, UP)).toBe(false);
  });

  it('stops attributing once the scroller unregisters', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 200 });
    const release = registerNestedScroller(box);
    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(true);

    release();

    expect(scrollDeltaConsumedBelow(leaf, boundary, UP)).toBe(false);
  });
});

describe('wheelConsumedBelow', () => {
  it('reads deltaY off the event', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 200 });
    registerNestedScroller(box);
    const event = new WheelEvent('wheel', { bubbles: true, deltaY: -10 });
    leaf.dispatchEvent(event);

    expect(wheelConsumedBelow(event, boundary)).toBe(true);
  });
});

describe('touchDragConsumedBelow', () => {
  // A finger moving DOWN (positive dy) pulls earlier content into view —
  // the same direction as a negative wheel delta.
  it('maps a downward finger drag to an upward scroll', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 200 });
    registerNestedScroller(box);

    expect(touchDragConsumedBelow(leaf, boundary, 40)).toBe(true);
  });

  it('maps an upward finger drag to a downward scroll', () => {
    const { boundary, box, leaf } = tree({
      scrollTop: 0,
      clientHeight: 100,
      scrollHeight: 500,
    });
    registerNestedScroller(box);

    expect(touchDragConsumedBelow(leaf, boundary, -40)).toBe(true);
  });

  it('chains out when the box cannot move that way', () => {
    const { boundary, box, leaf } = tree({ scrollTop: 0 });
    registerNestedScroller(box);

    expect(touchDragConsumedBelow(leaf, boundary, 40)).toBe(false);
  });
});
