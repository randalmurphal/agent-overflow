import { describe, expect, it } from 'vitest';
import {
  ACTIVITY_RUN_CAP_CSS,
  activityRunClipMaxHeight,
  activityRunExpandedBodies,
  activityRunExpandedHeight,
} from './activityRunClip';

// Which bodies lift a run's cap, and by how much. happy-dom reports zero
// geometry, so the heights here are stamped explicitly and the REAL cap
// arithmetic is asserted in activityRunClip.browser.test.ts; what this file
// covers is the selection, which is pure DOM structure: a body outside the run
// must not lift this run's cap, and a nested one must not be counted twice.

function clipWith(html: string): HTMLElement {
  const clip = document.createElement('div');
  clip.innerHTML = html;
  return clip;
}

function disclosure(bodyId: string, expanded: boolean): string {
  return `<button aria-expanded="${expanded}" aria-controls="${bodyId}"></button>`;
}

/** happy-dom lays nothing out, so a body's contribution is stamped on it. */
function stampHeight(body: HTMLElement, px: number): HTMLElement {
  Object.defineProperty(body, 'offsetHeight', { value: px, configurable: true });
  return body;
}

describe('activityRunClipMaxHeight', () => {
  it('is the bare cap with nothing expanded', () => {
    expect(activityRunClipMaxHeight(0)).toBe(ACTIVITY_RUN_CAP_CSS);
  });

  it('adds exactly what expansion asked for', () => {
    expect(activityRunClipMaxHeight(220)).toBe(`calc(${ACTIVITY_RUN_CAP_CSS} + 220px)`);
    // Fractional heights are real (borders, DPR); the style value stays integral
    // so a sub-pixel jitter cannot re-write the declaration every frame.
    expect(activityRunClipMaxHeight(220.4)).toBe(`calc(${ACTIVITY_RUN_CAP_CSS} + 220px)`);
  });
});

describe('activityRunExpandedBodies', () => {
  it('finds the body an expanded disclosure points at', () => {
    const clip = clipWith(`${disclosure('body-1', true)}<div id="body-1"></div>`);

    expect(activityRunExpandedBodies(clip).map((b) => b.id)).toEqual(['body-1']);
  });

  it('ignores a collapsed disclosure', () => {
    const clip = clipWith(`${disclosure('body-1', false)}<div id="body-1"></div>`);

    expect(activityRunExpandedBodies(clip)).toEqual([]);
  });

  it('ignores a body outside this run', () => {
    // The id resolves through the clip, never through `document` — otherwise
    // one run's open diff would lift every other run's cap.
    const outside = document.createElement('div');
    outside.id = 'body-elsewhere';
    document.body.appendChild(outside);
    const clip = clipWith(disclosure('body-elsewhere', true));

    expect(activityRunExpandedBodies(clip)).toEqual([]);
    outside.remove();
  });

  it('ignores a stale pointer at an unmounted body', () => {
    const clip = clipWith(disclosure('body-gone', true));

    expect(activityRunExpandedBodies(clip)).toEqual([]);
  });

  it('counts a body once when two disclosures point at it', () => {
    const clip = clipWith(
      `${disclosure('body-1', true)}${disclosure('body-1', true)}<div id="body-1"></div>`,
    );

    expect(activityRunExpandedBodies(clip).map((b) => b.id)).toEqual(['body-1']);
  });

  it('drops a body nested inside another expanded body', () => {
    // A tool row expanded inside an expanded subagent card: the card's height
    // already includes it, so counting both would lift the cap twice for one
    // expansion.
    const clip = clipWith(`
      ${disclosure('card', true)}
      <div id="card">
        ${disclosure('inner', true)}
        <div id="inner"></div>
      </div>
    `);

    expect(activityRunExpandedBodies(clip).map((b) => b.id)).toEqual(['card']);
  });
});

describe('activityRunExpandedHeight', () => {
  it('sums the bodies, because each one lifts the cap by its own height', () => {
    const first = stampHeight(document.createElement('div'), 120);
    const second = stampHeight(document.createElement('div'), 80);

    expect(activityRunExpandedHeight([first, second])).toBe(200);
  });

  it('is zero with nothing expanded', () => {
    expect(activityRunExpandedHeight([])).toBe(0);
  });
});
