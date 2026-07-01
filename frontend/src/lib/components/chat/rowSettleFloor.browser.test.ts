import { describe, it, expect, afterEach } from 'vitest';
// Import the REAL production stylesheet so the geometry runs against the actual
// cascade (row / content-box sizing, `[data-row-geometry-content]` rules), the
// same coupling the sibling rowMarginContainment.browser.test.ts relies on.
import '../../../app.css';
import { waitFor } from '../../../test/helpers/browserFrames';
import {
  createTimelineRowGeometryReservation,
  ROW_GEOMETRY_CONTENT_ATTR,
  type TimelineRowGeometryCache,
  type TimelineRowGeometryReservationParams,
} from './timelineRowGeometry';
import { timelineRowGeometryCacheKey } from '../../stores/threadRowUiState.svelte';

// happy-dom reports zero geometry, so the settle "twitch" -- a row whose content
// box GROWS when the shell signature churns AFTER the row already settled -- is
// invisible to the unit suite. The unit tests lock the state machine (the
// min-height string); this locks the user-visible OUTCOME in a real layout
// engine: once measured, the rendered box must not jump when a later update()
// carries a changed signature. That outcome guard survives a future refactor
// that changes HOW the floor is written, which a min-height-string assertion
// would not. Runs in the `browser` vitest project (real Chromium via
// Playwright); see frontend/vitest.config.ts.

const mounted: HTMLElement[] = [];
const handles: Array<{ destroy?: () => void }> = [];

// A faithful slice of the virtua row chain (mirrors rowMarginContainment's):
//   item  -- virtua's item wrapper stand-in (its own containment context).
//   row   -- [data-row-index]: the element the reservation writes min-height onto.
//   geo   -- [data-row-geometry-content]: the element the reservation's RO measures.
//   child -- carries an explicit FRACTIONAL height so the natural box is a value
//            happy-dom could never represent, proving the real-engine need.
function mountRow(childHeightPx: number): { row: HTMLElement; geo: HTMLElement } {
  const item = document.createElement('div');
  item.style.cssText = 'contain: layout style; width: 800px;';
  const row = document.createElement('div');
  row.setAttribute('data-row-index', '0');
  const geo = document.createElement('div');
  geo.setAttribute(ROW_GEOMETRY_CONTENT_ATTR, '');
  const child = document.createElement('div');
  child.style.cssText = `height: ${childHeightPx}px;`;
  geo.appendChild(child);
  row.appendChild(geo);
  item.appendChild(row);
  document.body.appendChild(item);
  mounted.push(item);
  return { row, geo };
}

function rowKey(): TimelineRowGeometryReservationParams {
  return { key: 'l:thread-a:item-a', signature: 'signature-a', width: 800, ownerItemIds: ['item-a'] };
}

// Minimal cache mirroring the unit suite's makeCache -- a plain keyed map, no
// pruning; the browser test only needs deterministic hit / miss. Keyed with
// the production key builder so this fake cannot drift from real cache
// semantics.
function makeCache(
  entries: Array<[TimelineRowGeometryReservationParams, number]>,
): TimelineRowGeometryCache {
  const heights = new Map<string, number>();
  for (const [key, height] of entries) heights.set(timelineRowGeometryCacheKey(key), height);
  return {
    cachedTimelineRowHeight: (key) => heights.get(timelineRowGeometryCacheKey(key)),
    rememberTimelineRowHeight: (key, height) => {
      heights.set(timelineRowGeometryCacheKey(key), height);
    },
  };
}

afterEach(() => {
  for (const h of handles.splice(0)) h.destroy?.();
  for (const el of mounted.splice(0)) el.remove();
});

describe('timeline row geometry settle floor (settle-flicker fix)', () => {
  it('does not grow a settled row when a later update carries a changed signature', async () => {
    // Natural content box is fractional (235.4px) -- the real twitch is an
    // integer floor written over a fractional natural height. The cache holds
    // 235 for the current signature (the cold-mount floor, <= natural, so the
    // real RO settles rather than holds) and a much larger 350 for the churned
    // signature -- a taller stale entry, exactly what the streaming tail's cache
    // legitimately holds from an earlier, taller wave.
    const { row } = mountRow(235.4);
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([
      [key, 235],
      [changedKey, 350],
    ]);

    const action = createTimelineRowGeometryReservation(cache);
    const handle = action(row, key);
    handles.push(handle ?? {});

    // Cold mount writes the 235 floor; it has no visual effect (natural 235.4 >
    // 235) and releases to '' once the real ResizeObserver measures the row --
    // the observable settle signal.
    expect(row.style.minHeight).toBe('235px');
    await waitFor(() => row.style.minHeight === '', 'cold-mount floor to release after settle');

    const settledHeight = row.getBoundingClientRect().height;
    expect(settledHeight).toBeGreaterThan(230);
    expect(settledHeight).toBeLessThan(300);

    // Streaming churns the shell signature. Without the settled-height gate this
    // re-reserves the 350 stale entry -> min-height:350 -> the row's box GROWS
    // ~115px (the twitch). With the gate the box is untouched.
    // getBoundingClientRect forces a synchronous layout, so a re-floor write (if
    // the gate regressed) is already reflected here -- no extra frame needed.
    handle?.update?.(changedKey);
    const afterChurn = row.getBoundingClientRect().height;

    // The fails-without / passes-with guard: with the gate the box stays at its
    // settled fractional height; drop `if (state.hasSettledHeight) return;` and
    // this becomes 350.
    expect(afterChurn).toBeGreaterThan(230);
    expect(afterChurn).toBeLessThan(300);
    expect(row.style.minHeight).toBe('');
  });
});
