import { describe, it, expect, afterEach } from 'vitest';
import { mount, unmount } from 'svelte';
import { wait, waitFor } from '../helpers/browserFrames';
import VirtuaApplierHost, {
  type VirtuaApplierControls,
} from './virtua-patch-fixtures/VirtuaApplierHost.svelte';

// Drop-rule guard for the scroll-applier hunk of patches/virtua@0.49.1.patch
// (companion to virtua-patch-buffer-retention.browser.test.ts, which guards
// the manual-scroll-marking hunk).
//
// The patch adds a setScrollApplier seam to createScroller: when an applier
// is registered, virtua's internal scroll-jump compensation write
// ($fixScrollJump — the direct scrollTop assignment for above-viewport row
// remeasures) is handed to the applier instead of hitting the DOM, and a
// DECLINED delivery (applier returns false) makes the core poke its own
// store with the current DOM offset so the model cannot desync.
//
// Three assertions, deliberately split:
//  - "receives" proves the applier gets the compensation with the raw
//    (jump, shift) pair AND that the default write no longer fires. If a
//    version bump's re-roll loses the core hunk, this fails loudly
//    (setScrollApplier missing or the default write landing).
//  - "decline pokes" proves the decline contract: the store emits its
//    scroll event (model re-sync) with no DOM scroll having happened.
//  - "unregistered" proves the seam is inert without an applier — stock
//    compensation behavior, so non-controller consumers are unaffected.

const mounted: Array<{ app: object; host: HTMLElement }> = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

// virtua resets scroll-direction/manual state on scrollend (150ms after the
// last scroll event); waiting well past that starts each phase neutral.
const VIRTUA_SETTLE_MS = 400;

// Park mid-list: viewport shows rows ~100-110, so rows ~85-99 are mounted
// above the viewport inside the 600px buffer — growing one of those accrues
// a compensation jump.
const PARK_OFFSET_PX = 4000;
const ABOVE_VIEWPORT_ROW = 92;
const GROWN_HEIGHT_PX = 140; // +100 over the 40px base

async function mountParkedMidList(): Promise<VirtuaApplierControls> {
  const host = document.createElement('div');
  document.body.appendChild(host);
  let controls: VirtuaApplierControls | undefined;
  const app = mount(VirtuaApplierHost, {
    target: host,
    props: {
      registerControls: (c: VirtuaApplierControls) => {
        controls = c;
      },
    },
  });
  mounted.push({ app, host });
  await waitFor(() => controls !== undefined, 'host controls to register');
  const { scrollEl } = controls!;
  await waitFor(() => scrollEl.scrollHeight > 7000, 'virtua to lay out the list');
  scrollEl.scrollTop = PARK_OFFSET_PX;
  await wait(VIRTUA_SETTLE_MS);
  await waitFor(
    () => scrollEl.querySelectorAll('div').length > 0,
    'rows to render',
  );
  return controls!;
}

describe('virtua scroll-applier routing (patch drop-rule tripwire)', () => {
  it('the applier receives the compensation (target, jump, shift) and the default write no longer fires', async () => {
    const { scrollEl, handle, growRow } = await mountParkedMidList();

    const calls: Array<{ target: number; jump: number; shift: boolean }> = [];
    handle.setScrollApplier((target, jump, shift) => {
      calls.push({ target, jump, shift });
      return true; // "handled" — deliberately writes nothing
    });

    const topBefore = scrollEl.scrollTop;
    growRow(ABOVE_VIEWPORT_ROW, GROWN_HEIGHT_PX);
    await waitFor(() => calls.length > 0, 'applier to receive the compensation');

    expect(calls[0].jump).toBe(GROWN_HEIGHT_PX - 40);
    expect(calls[0].shift).toBe(false);
    expect(calls[0].target).toBe(topBefore + (GROWN_HEIGHT_PX - 40));
    // Routed: virtua did NOT write scrollTop itself. On a lost core hunk
    // the default write lands and this trips.
    expect(scrollEl.scrollTop).toBe(topBefore);
  });

  it('a declined delivery pokes the store (model re-sync without a DOM scroll)', async () => {
    const { scrollEl, handle, growRow, counters } = await mountParkedMidList();

    let declined = 0;
    handle.setScrollApplier(() => {
      declined += 1;
      return false;
    });

    const topBefore = scrollEl.scrollTop;
    const onscrollBefore = counters.onscroll;
    growRow(ABOVE_VIEWPORT_ROW, GROWN_HEIGHT_PX);
    await waitFor(() => declined > 0, 'applier to decline the compensation');

    // No DOM write happened...
    expect(scrollEl.scrollTop).toBe(topBefore);
    // ...but the store emitted its scroll event anyway: the core's
    // decline-poke dispatched ACTION_SCROLL with the current DOM offset,
    // re-syncing the model (and its offset now matches the DOM).
    await waitFor(() => counters.onscroll > onscrollBefore, 'decline to poke the store');
    expect(Math.abs(handle.getScrollOffset() - scrollEl.scrollTop)).toBeLessThanOrEqual(1);
  });

  it('without a registered applier the compensation writes scrollTop directly (stock behavior)', async () => {
    const { scrollEl, growRow } = await mountParkedMidList();

    const topBefore = scrollEl.scrollTop;
    growRow(ABOVE_VIEWPORT_ROW, GROWN_HEIGHT_PX);
    await waitFor(
      () => scrollEl.scrollTop === topBefore + (GROWN_HEIGHT_PX - 40),
      'default compensation write to land',
    );
  });
});
