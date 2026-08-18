import { describe, it, expect, afterEach } from 'vitest';
// The REAL production stylesheet: every assertion here is coupled to the
// timeline transition kill rule in app.css. Delete that rule and this file
// fails — that is the fails-without / passes-with guard for the activity-run
// expand flicker (2026-08-17): a CSS transition starting in the same commit
// as a bottom-held toggle put the compositor in animation-priority mode,
// licensing it to present the frame before the toggle's re-rastered tiles
// were ready, blanking the text below the run. Full mechanism: the rule's
// comment in app.css.
import '../../../app.css';

// Computed style resolution and getAnimations() need a real style engine;
// happy-dom reports neither. This file runs in the `browser` vitest project
// (real Chromium via Playwright); see frontend/vitest.config.ts.

const mounted: HTMLElement[] = [];

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
});

/** A stand-in for MessageTimeline's scroller with one row inside. */
function mountScroller(): { scroller: HTMLElement; row: HTMLElement } {
  const scroller = document.createElement('div');
  scroller.setAttribute('data-testid', 'message-timeline-scroll');
  const row = document.createElement('div');
  row.setAttribute('data-row-index', '0');
  // MessageTimeline's real wrapper classes: the edit-and-resend dim fade.
  row.className = 'transition-opacity duration-200';
  scroller.appendChild(row);
  document.body.appendChild(scroller);
  mounted.push(scroller);
  return { scroller, row };
}

describe('timeline transition kill (expand-flicker fix)', () => {
  it('zeroes transition-duration for elements inside the scroller, inline styles included', () => {
    const { row } = mountScroller();
    const el = document.createElement('div');
    // Inline style, the strongest non-important declaration — the rule must
    // still win, or a future component styling via `style=` regresses.
    el.style.transition = 'opacity 150ms ease-out';
    row.appendChild(el);

    expect(getComputedStyle(el).transitionDuration).toBe('0s');
    expect(getComputedStyle(el).transitionDelay).toBe('0s');
  });

  it('leaves the same element transitioning outside the scroller (the rule is scoped, not global)', () => {
    const outside = document.createElement('div');
    outside.style.transition = 'opacity 150ms ease-out';
    document.body.appendChild(outside);
    mounted.push(outside);

    expect(getComputedStyle(outside).transitionDuration).toBe('0.15s');
  });

  it('carves out the [data-row-index] wrapper itself: the pending-cut dim keeps its fade', () => {
    const { row } = mountScroller();
    expect(getComputedStyle(row).transitionDuration).toBe('0.2s');
    // But only the wrapper element — its descendants are killed (previous test).
  });

  it('the carve-out does not extend to the wrapper\'s pseudo-elements', () => {
    const { row } = mountScroller();
    row.classList.add('pseudo-probe');
    const sheet = document.createElement('style');
    sheet.textContent =
      '.pseudo-probe::before { content: ""; transition: opacity 150ms ease-out; }';
    document.head.appendChild(sheet);
    mounted.push(sheet);

    expect(getComputedStyle(row, '::before').transitionDuration).toBe('0s');
  });

  it('kills transitions on the scroller element itself, not only its contents', () => {
    const { scroller } = mountScroller();
    scroller.style.transition = 'opacity 150ms ease-out';

    expect(getComputedStyle(scroller).transitionDuration).toBe('0s');
  });

  it('a property flip inside the scroller creates no CSSTransition', () => {
    const { row } = mountScroller();
    const el = document.createElement('div');
    el.style.transition = 'opacity 150ms ease-out';
    el.style.opacity = '0';
    row.appendChild(el);
    // Flush the initial style so the flip below is a transitionable change.
    void getComputedStyle(el).opacity;

    el.style.opacity = '1';
    // getAnimations() forces a style update, which is when transitions start.
    const transitions = document
      .getAnimations()
      .filter(
        (a) =>
          a instanceof CSSTransition &&
          a.effect instanceof KeyframeEffect &&
          el.contains(a.effect.target),
      );
    expect(transitions).toHaveLength(0);
  });

  it('control: the same flip outside the scroller does create a CSSTransition', () => {
    // Proves the assertion above can fail — without this, a broken
    // getAnimations() rig would pass the kill test vacuously.
    const outside = document.createElement('div');
    outside.style.transition = 'opacity 150ms ease-out';
    outside.style.opacity = '0';
    document.body.appendChild(outside);
    mounted.push(outside);
    void getComputedStyle(outside).opacity;

    outside.style.opacity = '1';
    const transitions = document
      .getAnimations()
      .filter(
        (a) =>
          a instanceof CSSTransition &&
          a.effect instanceof KeyframeEffect &&
          a.effect.target === outside,
      );
    expect(transitions).toHaveLength(1);
  });
});
