// Shared real-Chromium harness for the MessageTimeline outcome suites:
// streamingOutcome.browser.test.ts and remountReturn.browser.test.ts
// (src/lib/components/chat/). Both mount the REAL MessageTimeline over a real
// pane (real engine windowing, real ResizeObserver timing, real fonts/layout)
// and assert user-visible outcomes; this module owns the plumbing they must
// keep behaviorally aligned — the mount ritual + teardown registry, the
// seeded-transcript shape, the quiet-point wait loops, and the removed-row
// MutationObserver counter. Outcome thresholds, settle policies, and scenario
// choreography are test-local policy and stay in the test files.
// Cascade-coupled suites import `app.css` themselves (components/chat/
// AGENTS.md "Test Notes"); the harness does not force the production cascade
// on its importers.
import { afterEach, beforeEach, expect } from 'vitest';
import { mount, unmount } from 'svelte';
import MessageTimeline from '../../lib/components/chat/MessageTimeline.svelte';
import { loadSettings } from '../../lib/stores/settings.svelte';
import type { ThreadPane } from '../../lib/stores/thread.svelte';
import type { Item } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { buildPane, makeItem, makeThread } from './chat';
import { raf, waitFor } from './browserFrames';
import { makeSettings } from './settings';

// ~44 markdown rows at 100-200px rendered height each ≈ 6-8k px of scrollback
// — comfortably past the 600px viewport + the virtualizer's 2×1800px buffers,
// so real windowing is active: a streaming above-viewport buffer drop has rows
// to drop, and a scroll-away genuinely unmounts the tail.
export const SEED_COUNT = 44;
const VIEWPORT_W_PX = 800;
const VIEWPORT_H_PX = 600;

export function distanceToBottom(el: HTMLElement): number {
  return el.scrollHeight - el.clientHeight - el.scrollTop;
}

// ---------------------------------------------------------------------------
// Mount registry + suite hooks
// ---------------------------------------------------------------------------
export interface MountedEntry {
  app: object;
  host: HTMLElement;
  // Tests hang their monitor teardown here so a failed assertion can't leak a
  // running rAF sampler into the next test.
  stop?: () => Promise<void>;
}

const mounted: MountedEntry[] = [];

// Suite bootstrap: settings load before each test, mount + monitor teardown
// after. Call once at module scope in each test file that uses mountTimeline.
export function setupTimelineHarness(): void {
  beforeEach(async () => {
    // Geometry/outcome suites begin with runs open, then exercise collapse as
    // an explicit action. The shipped default is now collapsed, so pin the
    // fixture policy instead of inheriting a product preference accidentally.
    setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'expanded' }));
    setBindingMock('GetThreadUserMessageTicks', async () => []);
    await loadSettings();
  });
  afterEach(async () => {
    for (const { app, host, stop } of mounted.splice(0)) {
      await stop?.();
      unmount(app);
      host.remove();
    }
  });
}

// ---------------------------------------------------------------------------
// Seed transcript
// ---------------------------------------------------------------------------
// Per-suite flavor text for the shared seed shape. Only the prose differs
// between suites; the structural rhythm (user/assistant alternation, list
// rows, double-paragraph rows) is what gives the virtualizer realistic height variance
// and must stay identical so both suites exercise the same geometry.
export interface SeedProse {
  question(i: number): string;
  replyLead(i: number): string;
  replyList: string;
}

// Markdown-ish transcript: alternating user asks and multi-paragraph
// assistant replies with lists/bold/inline code, so rendered row heights vary
// realistically and the markdown pipeline (streamdown) is genuinely in play.
export function seedTimelineItems(threadId: string, prose: SeedProse): Item[] {
  const items: Item[] = [];
  for (let i = 0; i < SEED_COUNT; i++) {
    const isUser = i % 2 === 0;
    let summary: string;
    if (isUser) {
      summary = prose.question(i);
    } else {
      const parts = [prose.replyLead(i)];
      if (i % 3 === 0) {
        parts.push(prose.replyList);
      }
      if (i % 4 === 1) {
        parts.push(`A second paragraph for reply ${i} so consecutive assistant rows do not all share one height bucket and the engine's estimate-to-measure corrections have real work to do.`);
      }
      summary = parts.join('\n\n');
    }
    items.push(makeItem({
      id: `seed-${i}`,
      threadId,
      turnIndex: i,
      itemIndex: 0,
      kind: isUser ? 'user_text' : 'assistant_text',
      role: isUser ? 'user' : 'assistant',
      status: 'completed',
      summary,
      createdAt: i,
      updatedAt: i,
    }));
  }
  return items;
}

// ---------------------------------------------------------------------------
// Quiet-point waits
// ---------------------------------------------------------------------------
export interface FrameSettleOptions {
  stableFrames: number;
  frameBudget: number;
}

export interface QuietBottomOptions extends FrameSettleOptions {
  // Suite-calibrated distance-to-bottom tolerance (the test files'
  // QUIET_BOTTOM_EPSILON_PX constants carry the calibration rationale).
  epsilonPx: number;
}

// Geometry-quiet AND at-bottom for `stableFrames` consecutive frames. Covers
// the virtualizer's 150ms scrollend debounce, the smoother's backlog drain,
// and late measure corrections without a fixed sleep.
export async function waitForQuietBottom(
  scrollEl: HTMLElement,
  label: string,
  opts: QuietBottomOptions,
): Promise<void> {
  let stable = 0;
  let lastHeight = -1;
  for (let i = 0; i < opts.frameBudget; i++) {
    const height = scrollEl.scrollHeight;
    if (height === lastHeight && distanceToBottom(scrollEl) <= opts.epsilonPx) {
      stable += 1;
      if (stable >= opts.stableFrames) return;
    } else {
      stable = 0;
      lastHeight = height;
    }
    await raf();
  }
  throw new Error(`timed out waiting for quiet bottom: ${label}`);
}

// Quiet at the very top AND the tail row genuinely unmounted — the
// precondition for a return leg to be a real remount wave. Throws on timeout
// so a vacuous run (windowing regressed, nothing unmounted) cannot masquerade
// as a passing outcome.
export async function waitForQuietTop(
  scrollEl: HTMLElement,
  tailRowIndex: number,
  label: string,
  opts: FrameSettleOptions,
): Promise<void> {
  let stable = 0;
  let lastHeight = -1;
  for (let i = 0; i < opts.frameBudget; i++) {
    const tailGone = scrollEl.querySelector(`[data-row-index="${tailRowIndex}"]`) === null;
    const height = scrollEl.scrollHeight;
    if (scrollEl.scrollTop <= 1 && height === lastHeight && tailGone) {
      stable += 1;
      if (stable >= opts.stableFrames) return;
    } else {
      stable = 0;
      lastHeight = height;
    }
    await raf();
  }
  throw new Error(`timed out waiting for quiet top (tail row must unmount): ${label}`);
}

// ---------------------------------------------------------------------------
// Removed-row counter
// ---------------------------------------------------------------------------
export interface RemovedRowCounter {
  // Drain records queued but not yet dispatched (MutationObserver delivers on
  // a microtask) through the same counting path as live delivery. Call before
  // closing a counting window so late-queued removals aren't silently
  // dropped.
  flush(): void;
  // Discard queued records WITHOUT counting them — opens a fresh counting
  // window that must not inherit removals from before it began.
  discardPending(): void;
  // flush() + disconnect: final teardown never drops queued records.
  end(): void;
}

// Counts removed [data-row-index] rows under `scrollEl` — the user-visible
// signature of windowing remount churn. Each non-zero removal batch is reported
// through `onBatch`; window slicing (per-phase tallies, cumulative stats,
// batch maxima) stays with the caller.
export function observeRemovedRows(
  scrollEl: HTMLElement,
  onBatch: (removedRows: number) => void,
): RemovedRowCounter {
  const countBatch = (records: MutationRecord[]): void => {
    let batch = 0;
    for (const record of records) {
      for (const node of record.removedNodes) {
        if (!(node instanceof Element)) continue;
        batch += node.matches('[data-row-index]')
          ? 1
          : node.querySelectorAll('[data-row-index]').length;
      }
    }
    if (batch > 0) onBatch(batch);
  };
  const observer = new MutationObserver(countBatch);
  observer.observe(scrollEl, { childList: true, subtree: true });
  return {
    flush() {
      countBatch(observer.takeRecords());
    },
    discardPending() {
      observer.takeRecords();
    },
    end() {
      countBatch(observer.takeRecords());
      observer.disconnect();
    },
  };
}

// ---------------------------------------------------------------------------
// Mount ritual
// ---------------------------------------------------------------------------
export interface MountedTimeline {
  pane: ThreadPane;
  scrollEl: HTMLElement;
  host: HTMLElement;
  entry: MountedEntry;
}

// Mount the real MessageTimeline over a fresh pane and wait through the
// warm-up cascade to the settled, visible, bottom-pinned steady state that
// outcome monitors must start from.
/**
 * Emulated user scroll: a direction-matched wheel event (the controller's
 * escape / return-intent signal) followed by stepped, unmarked `scrollTop`
 * writes (Chromium fires a real scroll event for each).
 *
 * Both halves are load-bearing. Writing `scrollTop` alone is not a reader
 * scrolling — the controller has no gesture to attribute it to, stays sticky,
 * and the next anchored transaction correctly re-pins the bottom instead of
 * holding the row the test moved to.
 */
export async function userScrollTo(
  scrollEl: HTMLElement,
  targetTop: number,
  steps = 8,
): Promise<void> {
  const start = scrollEl.scrollTop;
  const deltaY = targetTop < start ? -120 : 120;
  for (let i = 1; i <= steps; i += 1) {
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
    scrollEl.scrollTop = start + ((targetTop - start) * i) / steps;
    await raf();
    await raf();
  }
}

export async function mountTimeline(
  threadId: string,
  items: Item[],
  quiet: QuietBottomOptions,
  // Provider-sensitive scenarios (Codex detached-launch shapes) override the
  // thread; the id always comes from `threadId`.
  thread?: Partial<Parameters<typeof makeThread>[0]>,
): Promise<MountedTimeline> {
  const pane = await buildPane(makeThread({ ...thread, id: threadId }), items);
  const host = document.createElement('div');
  // Fixed, definite size: MessageTimeline's root is `h-full`, so the host is
  // the viewport. position:fixed keeps document scrollbars out of the test.
  host.style.cssText = `position: fixed; top: 0; left: 0; width: ${VIEWPORT_W_PX}px; height: ${VIEWPORT_H_PX}px; background: #111;`;
  document.body.appendChild(host);
  const app = mount(MessageTimeline, { target: host, props: { pane } });
  const entry: MountedEntry = { app, host };
  mounted.push(entry);

  await waitFor(
    () => host.querySelector('[data-testid="message-timeline-scroll"]') !== null,
    'scroll container to mount',
  );
  const scrollEl = host.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
  await waitFor(
    () => scrollEl.querySelectorAll('[data-row-index]').length > 0,
    'virtualizer to render rows',
  );
  // Real windowing sanity: the whole point of the renderAll test seam is
  // that the browser project does NOT flat-render every row. Without
  // windowing, streams have no buffer to drop, scroll-away unmounts nothing,
  // and every removed-row outcome passes vacuously.
  expect(
    scrollEl.querySelectorAll('[data-row-index]').length,
    'browser project must run real windowing (renderAll seam)',
  ).toBeLessThan(items.length);
  // Warm-up gate: content stays visibility:hidden until the measurement
  // cascade settles; monitors must only start on the visible steady state.
  await waitFor(() => {
    const row = scrollEl.querySelector('[data-row-index]');
    return row !== null && getComputedStyle(row).visibility === 'visible';
  }, 'warm-up reveal');
  await waitForQuietBottom(scrollEl, 'initial mount settle', quiet);
  return { pane, scrollEl, host, entry };
}
