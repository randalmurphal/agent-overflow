import { describe, it, expect, afterEach } from 'vitest';
import { mount, unmount } from 'svelte';
import { raf, wait, waitFor } from '../helpers/browserFrames';
import VirtuaBufferRetentionHost, {
  type VirtuaBufferRetentionControls,
} from './virtua-patch-fixtures/VirtuaBufferRetentionHost.svelte';

// Regression guard for the streaming settle flicker's ROOT cause and for the
// virtua@0.49.1 pnpm patch that fixes it (patches/virtua@0.49.1.patch).
//
// virtua classifies any scroll event as USER scrolling unless its internal
// manual flag was set first; while the latched direction is "down" it drops
// the entire above-viewport buffer, unmounting every buffered row. During
// streaming, useStickToBottom's per-beat pin writes fired exactly that
// misclassification — dozens of rows unmounted per beat and remounted in one
// flush at scrollend, whose re-floored heights produced the visible twitch
// (docs/architecture/settle-flicker-analysis.md).
//
// Two tests, deliberately paired:
//  - "unmarked" proves this harness still reproduces the buffer drop. If a
//    future virtua version stops dropping the buffer on unmarked programmatic
//    writes, THIS test failing is the signal the patch may be droppable.
//  - "marked" proves the patched markProgrammaticScroll() prevents the drop —
//    the fail-without/pass-with guard for the fix itself.

const mounted: Array<{ app: object; host: HTMLElement }> = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function nextScrollEvent(scrollEl: HTMLElement): Promise<void> {
  return new Promise((resolve) =>
    scrollEl.addEventListener('scroll', () => resolve(), { once: true }),
  );
}

// virtua resets its scroll-direction and manual flags on scrollend, which its
// scroller synthesizes 150ms after the last scroll event. Waiting well past
// that guarantees each test's write starts from a neutral store.
const VIRTUA_SETTLE_MS = 400;

async function mountSettledNearBottom(): Promise<VirtuaBufferRetentionControls> {
  const host = document.createElement('div');
  document.body.appendChild(host);
  let controls: VirtuaBufferRetentionControls | undefined;
  const app = mount(VirtuaBufferRetentionHost, {
    target: host,
    props: {
      registerControls: (c: VirtuaBufferRetentionControls) => {
        controls = c;
      },
    },
  });
  mounted.push({ app, host });
  await waitFor(() => controls !== undefined, 'host controls to register');
  const { scrollEl } = controls!;
  await waitFor(() => scrollEl.scrollHeight > 4000, 'virtua to lay out the list');

  // Park near — not at — the bottom so a later small downward write still has
  // room to move and therefore fires a real scroll event. The positioning
  // write itself is an unmarked scroll; the settle wait lets its scrollend
  // clear the store before the test's measured phase begins.
  scrollEl.scrollTop = scrollEl.scrollHeight - scrollEl.clientHeight - 50;
  await wait(VIRTUA_SETTLE_MS);
  controls!.counters.mounts = 0;
  controls!.counters.destroys = 0;
  return controls!;
}

// One pin-style beat: a small downward scrollTop write, awaited through its
// scroll event plus two frames so virtua's store update and Svelte's template
// flush have both run before the caller inspects the counters.
async function pinWriteDown(scrollEl: HTMLElement, px: number): Promise<void> {
  const scrolled = nextScrollEvent(scrollEl);
  scrollEl.scrollTop += px;
  await scrolled;
  await raf();
  await raf();
}

describe('virtua backward-buffer retention across programmatic scroll writes', () => {
  it('unmarked direct scrollTop write down drops the above-viewport buffer (patch drop-rule tripwire)', async () => {
    const { scrollEl, counters } = await mountSettledNearBottom();

    await pinWriteDown(scrollEl, 8);

    // bufferSize 600px / 40px rows = 15 buffered rows above the viewport;
    // the misclassified write unmounts the whole band. ≥10 keeps the
    // assertion robust to boundary rounding while staying far above any
    // legitimate single-row window shift.
    expect(counters.destroys).toBeGreaterThanOrEqual(10);
  });

  it('markProgrammaticScroll() before the write keeps the buffer mounted (the settle-flicker fix)', async () => {
    const { scrollEl, handle, counters } = await mountSettledNearBottom();

    handle.markProgrammaticScroll();
    await pinWriteDown(scrollEl, 8);

    // An 8px move may legitimately shift the rendered window across one row
    // boundary; anything beyond that is the buffer drop this fix removes.
    expect(counters.destroys).toBeLessThanOrEqual(1);
  });
});
