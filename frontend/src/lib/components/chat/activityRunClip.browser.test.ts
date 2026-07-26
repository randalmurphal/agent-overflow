import { afterEach, describe, expect, it } from 'vitest';
// The REAL production stylesheet: every assertion here is coupled to the
// `.activity-run-clip` rules in app.css (zero-width native bar) and to the
// centered-column classes the run renders inside. Delete the scrollbar rules
// and the width guard below fails — that is the point of importing it.
import '../../../app.css';
import {
  ACTIVITY_RUN_CAP_REM_PX,
  activityRunClipMaxHeight,
  observeActivityRunExpansion,
} from '../../utils/activityRunClip';

// happy-dom reports zero geometry, so none of this is observable there: a
// scrollbar that consumes width, a `min(50vh, Nrem)` max-height cap, and a
// `calc()` the browser has to resolve all need a real layout engine. This file
// runs in the `browser` vitest project (real Chromium via Playwright); see
// frontend/vitest.config.ts.

const HOST_H_PX = 600;
/**
 * ACTIVITY_RUN_CAP_CSS as this browser resolves it. `50vh` is measured against
 * the real window, not the fixed host below, so which half of the `min()` wins
 * depends on the runner's window — the formula is asserted, the number is not
 * hardcoded.
 */
const CAP_PX = Math.min(window.innerHeight / 2, ACTIVITY_RUN_CAP_REM_PX);
/** clientHeight is integral; a `50vh` of an odd viewport rounds. */
const ROUND_PX = 1;

const mounted: HTMLElement[] = [];

afterEach(() => {
  for (const el of mounted.splice(0)) el.remove();
});

function expectHeightNear(actual: number, expected: number): void {
  expect(Math.abs(actual - expected)).toBeLessThanOrEqual(ROUND_PX);
}

/**
 * A faithful slice of the column a run renders inside: the timeline's
 * `mx-auto max-w-[62rem] px-6` wrapper, holding a prose row and a run whose
 * rail indents the clip. The prose row is the reference the run's content must
 * stay aligned with — the whole reason the clip cannot use a scrollbar gutter.
 */
function mountColumn(): { prose: HTMLElement; clip: HTMLElement; content: HTMLElement } {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; inset: 0; height: ${HOST_H_PX}px; width: 900px;`;
  const column = document.createElement('div');
  column.className = 'mx-auto w-full max-w-[62rem] px-6';
  const prose = document.createElement('div');
  const run = document.createElement('div');
  // Matches ActivityRun's rail geometry.
  run.className = 'relative ml-[14px] border-l border-border-subtle pl-[18px]';
  const clip = document.createElement('div');
  clip.className = 'activity-run-clip overflow-y-auto overflow-x-hidden';
  clip.style.maxHeight = activityRunClipMaxHeight(0);
  const content = document.createElement('div');
  clip.appendChild(content);
  run.appendChild(clip);
  column.append(prose, run);
  host.appendChild(column);
  document.body.appendChild(host);
  mounted.push(host);
  return { prose, clip, content };
}

function rows(content: HTMLElement, count: number, heightPx = 40): void {
  content.replaceChildren();
  for (let i = 0; i < count; i += 1) {
    const row = document.createElement('div');
    row.style.cssText = `height: ${heightPx}px;`;
    row.textContent = `row ${i}`;
    content.appendChild(row);
  }
}

/** Every style rule in the loaded sheets, `@layer` / `@media` blocks included. */
function styleRules(): CSSStyleRule[] {
  const found: CSSStyleRule[] = [];
  const visit = (rules: CSSRuleList): void => {
    for (const rule of rules) {
      if (rule instanceof CSSStyleRule) found.push(rule);
      if (rule instanceof CSSGroupingRule) visit(rule.cssRules);
    }
  };
  for (const sheet of document.styleSheets) visit(sheet.cssRules);
  return found;
}

describe('activity run clip — scrollbar suppression', () => {
  // This is the one invariant in the file that headless Chromium CANNOT see.
  // It reserves no width for a scrollbar at all — not even a custom one under
  // the app's global `::-webkit-scrollbar { width: 10px }` (measured: a 300px
  // box holding 1000px of content reports clientWidth 300, with or without the
  // suppression, with or without --disable-features=OverlayScrollbar). So the
  // geometric form of this guard — "the clip's content width does not change
  // when it starts overflowing" — passes here with the fix DELETED, and
  // asserting it would be false confidence.
  //
  // What is observable is the declaration, and it is coupled to app.css. Both
  // paths have to suppress the bar because the two engines take different
  // ones: WebKitGTK (production) honors `scrollbar-width`, Chromium honors the
  // pseudo-element. The geometric consequence is checked by hand on a real
  // WebKitGTK build; see docs/architecture/activity-runs.md.
  it('declares away both scrollbar paths', () => {
    const { clip } = mountColumn();

    expect(getComputedStyle(clip).scrollbarWidth).toBe('none');
    const bar = styleRules().find(
      (rule) => rule.selectorText === '.activity-run-clip::-webkit-scrollbar',
    );
    expect(bar?.style.width).toBe('0px');
    expect(bar?.style.height).toBe('0px');
  });

  it('takes no gutter either, so its rows sit on the rail', () => {
    const { prose, clip, content } = mountColumn();
    rows(content, 40);

    // A gutter — the outer scroller's fix for the same problem — would inset
    // the run's rows relative to the prose above and below, knocking them off
    // the rail the run draws. Unlike the bar itself, `scrollbar-gutter` is
    // observable here as a computed value.
    expect(getComputedStyle(clip).scrollbarGutter).toBe('auto');
    const RAIL_PX = 14 + 18 + 1;
    expect(clip.clientWidth).toBe(prose.clientWidth - RAIL_PX);
    expect(content.clientWidth).toBe(clip.clientWidth);
  });
});

describe('activity run clip — the cap', () => {
  it('bounds a long run and leaves it scrollable in place', () => {
    const { clip, content } = mountColumn();
    rows(content, 40);

    expectHeightNear(clip.clientHeight, CAP_PX);
    expect(clip.scrollHeight).toBe(40 * 40);
  });

  it('does not bound a run that fits', () => {
    const { clip, content } = mountColumn();
    rows(content, 3);

    expect(clip.clientHeight).toBe(3 * 40);
  });

  it('grows by exactly what an expanded body added', () => {
    const { clip, content } = mountColumn();
    rows(content, 40);

    // The disclosure contract the cap reads: an expanded header pointing at
    // its body (see utils/activityRunClip.ts for why it is not a marker
    // attribute).
    const trigger = document.createElement('button');
    trigger.setAttribute('aria-expanded', 'true');
    trigger.setAttribute('aria-controls', 'body-1');
    const body = document.createElement('div');
    body.id = 'body-1';
    body.style.cssText = 'height: 220px;';
    content.append(trigger, body);

    let reported = -1;
    const stop = observeActivityRunExpansion(clip, (px) => {
      reported = px;
    });
    clip.style.maxHeight = activityRunClipMaxHeight(reported);

    // Reading a diff inside a run must not mean scroll-within-scroll: the cap
    // gives back exactly the height the expansion asked for.
    expect(reported).toBe(220);
    expectHeightNear(clip.clientHeight, CAP_PX + 220);
    stop();
  });

  it('gives the height back when the body collapses', async () => {
    const { clip, content } = mountColumn();
    rows(content, 40);
    const trigger = document.createElement('button');
    trigger.setAttribute('aria-expanded', 'true');
    trigger.setAttribute('aria-controls', 'body-1');
    const body = document.createElement('div');
    body.id = 'body-1';
    body.style.cssText = 'height: 220px;';
    content.append(trigger, body);

    const seen: number[] = [];
    const stop = observeActivityRunExpansion(clip, (px) => {
      seen.push(px);
    });
    trigger.setAttribute('aria-expanded', 'false');
    // The mutation observer delivers on a microtask.
    await Promise.resolve();

    expect(seen.at(-1)).toBe(0);
    clip.style.maxHeight = activityRunClipMaxHeight(0);
    expectHeightNear(clip.clientHeight, CAP_PX);
    stop();
  });
});

describe('activity run clip — margins', () => {
  it('keeps a row trailing margin inside the scrolled content', () => {
    const { clip, content } = mountColumn();
    const row = document.createElement('div');
    row.style.cssText = 'height: 100px; margin-bottom: 20px;';
    content.appendChild(row);

    // The row keeps the height it had as a top-level row — the clip changes
    // nothing about how it measures, which is what lets its size prior carry
    // over — and `overflow-y: auto` makes the clip a BFC, so the trailing
    // margin lands in the scrolled content instead of collapsing out through
    // the run and into the timeline row's measured height.
    expect(row.offsetHeight).toBe(100);
    expect(clip.clientHeight).toBe(120);
    expect(clip.scrollHeight).toBe(120);
    // The margin does collapse through the plain content wrapper, so the
    // inner controller's contentEl geometry runs a constant under the clip's
    // scrollHeight. Harmless — it reads content height as a GROWTH signal,
    // and a fixed offset cannot change a delta — but pinned so a future
    // reader does not chase the discrepancy.
    expect(content.offsetHeight).toBe(100);
  });
});
