import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { mount, unmount, tick } from 'svelte';
import { cleanup, render } from '@testing-library/svelte';
// Real production cascade: the clamp is `${USER_MESSAGE_CLAMP_LINES}lh`
// against the bubble's own line box, and the fade is a mask that only exists
// in app.css-compiled component styles. Both are invisible to happy-dom,
// which reports zero geometry — so the whole overflow half of this feature
// (does the control appear at all?) can only be proven here, in the `browser`
// vitest project's real Chromium.
import '../../../app.css';
import UserMessage from './UserMessage.svelte';
import { createThreadRowUiState } from '../../stores/threadRowUiState.svelte';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { USER_MESSAGE_CLAMP_LINES } from './userMessageClamp';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import { captureResizeObserverLoopErrors } from '../../../test/helpers/resizeObserverLoopErrors';

const mounted: Array<{ app: object; host: HTMLElement }> = [];
const renderedHosts: HTMLElement[] = [];
let resizeLoopErrors: ReturnType<typeof captureResizeObserverLoopErrors>;

beforeEach(() => {
  resizeLoopErrors = captureResizeObserverLoopErrors();
});

/** No windowed rows to speak of — this suite mounts one message. */
const NO_ROWS_LOADED = {
  getItemById: () => undefined,
  loadedPayloadRefs: () => [],
};

const SHORT = 'ship it';
/** Comfortably over the clamp at any width. */
const LONG = Array.from({ length: 40 }, (_, i) => `line ${i} of a long pasted block`).join('\n');
/** One paragraph: fits inside the clamp when wide, wraps past it when narrow. */
const REWRAPS = Array.from({ length: 16 }, (_, i) => `sentence ${i} about the migration plan`).join(' ');

function makeItem(summary: string): Item {
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary,
    createdAt: 1784039160000,
    updatedAt: 1784039160000,
  };
}

interface TestPane {
  pane: ThreadPane;
  /** Elements handed to the scroll controller as the anchor to hold. */
  anchors: HTMLElement[];
}

/**
 * A pane carrying the REAL row-UI registry, so "survives a remount" is proven
 * against the shipped store rather than a test double, plus a scroll
 * controller that records the anchor each toggle asks it to hold.
 */
function makePane(): TestPane {
  const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
  const anchors: HTMLElement[] = [];
  const pane = {
    paneId: 'pane-1',
    threadId: 'thread-1',
    isUserMessageExpanded: rowUiState.isUserMessageExpanded,
    setUserMessageExpanded: rowUiState.setUserMessageExpanded,
    attachmentCacheFor: () => undefined,
    scrollController: {
      preserveScrollAnchor(anchor: HTMLElement, action: () => void | Promise<void>) {
        anchors.push(anchor);
        return Promise.resolve(action());
      },
    },
  } as unknown as ThreadPane;
  return { pane, anchors };
}

async function mountMessage(
  summary: string,
  opts: { width?: number; pane?: ThreadPane } = {},
): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.cssText = `width: ${opts.width ?? 700}px;`;
  document.body.appendChild(host);
  const app = mount(UserMessage, {
    target: host,
    props: { item: makeItem(summary), pane: opts.pane },
  });
  mounted.push({ app, host });
  await tick();
  // The batched overflow request resolves after the mount flush and before
  // the first paint.
  await raf();
  return host;
}

async function mountRerenderableMessage(
  summary: string,
  opts: { width?: number; pane?: ThreadPane } = {},
) {
  const host = document.createElement('div');
  host.style.cssText = `width: ${opts.width ?? 700}px;`;
  document.body.appendChild(host);
  renderedHosts.push(host);
  const view = render(UserMessage, {
    target: host,
    props: { item: makeItem(summary), pane: opts.pane },
  });
  await raf();
  return { host, rerender: view.rerender };
}

afterEach(async () => {
  for (const { app, host } of mounted.splice(0)) {
    await unmount(app);
    host.remove();
  }
  cleanup();
  for (const host of renderedHosts.splice(0)) host.remove();
  resizeLoopErrors.stop();
  expect(resizeLoopErrors.messages).toEqual([]);
});

const clip = (host: HTMLElement) =>
  host.querySelector<HTMLElement>('[data-testid="user-message-summary"]')!;
const toggle = (host: HTMLElement) =>
  host.querySelector<HTMLButtonElement>('[data-testid="user-message-clamp-toggle"]');

function lineHeightPx(el: HTMLElement): number {
  return Number.parseFloat(getComputedStyle(el).lineHeight);
}

describe('user message clamp', () => {
  it('leaves a short message unclipped, unfaded and with no control', async () => {
    const host = await mountMessage(SHORT);
    const p = clip(host);

    expect(p.scrollHeight).toBe(p.clientHeight);
    expect(getComputedStyle(p).maxHeight).toBe('none');
    expect(getComputedStyle(p).maskImage).toBe('none');
    expect(p.getAttribute('data-clamped')).toBeNull();
    expect(toggle(host)).toBeNull();
  });

  it('clips a long message to the threshold and offers "Show more"', async () => {
    const host = await mountMessage(LONG);
    const p = clip(host);

    expect(p.clientHeight).toBeLessThanOrEqual(
      USER_MESSAGE_CLAMP_LINES * lineHeightPx(p) + 1,
    );
    expect(p.scrollHeight).toBeGreaterThan(p.clientHeight);
    expect(p.getAttribute('data-clamped')).toBe('true');
    expect(toggle(host)?.textContent).toBe('Show more');
    expect(toggle(host)?.getAttribute('aria-expanded')).toBe('false');
    expect(toggle(host)?.getAttribute('aria-controls')).toBe(p.id);
  });

  it('fades the glyphs rather than painting a colour over the bubble', async () => {
    // A gradient overlay would have to reproduce the bubble's translucent
    // `bg-accent/15` tint composited over whatever is behind it; a mask lets
    // the real background through, so it is right in both themes.
    const host = await mountMessage(LONG);
    expect(getComputedStyle(clip(host)).maskImage).toContain('linear-gradient');
  });

  it('round-trips expand and collapse from the control', async () => {
    const { pane } = makePane();
    const host = await mountMessage(LONG, { pane });
    const collapsedHeight = clip(host).clientHeight;

    toggle(host)!.click();
    await waitFor(() => toggle(host)?.textContent === 'Show less', 'expanded');
    expect(clip(host).clientHeight).toBe(clip(host).scrollHeight);
    expect(clip(host).clientHeight).toBeGreaterThan(collapsedHeight);
    expect(getComputedStyle(clip(host)).maskImage).toBe('none');
    expect(toggle(host)!.getAttribute('aria-expanded')).toBe('true');

    toggle(host)!.click();
    await waitFor(() => toggle(host)?.textContent === 'Show more', 'collapsed again');
    expect(clip(host).clientHeight).toBe(collapsedHeight);
    expect(clip(host).getAttribute('data-clamped')).toBe('true');
  });

  it('anchors the toggle on the text, not on the control below it', async () => {
    const { pane, anchors } = makePane();
    const host = await mountMessage(LONG, { pane });
    const p = clip(host);

    toggle(host)!.click();
    await waitFor(() => anchors.length === 1, 'anchor recorded');
    // Holding the button still would slide the message the reader just
    // opened off the top of the viewport.
    expect(anchors[0]).toBe(p);
  });

  it('keeps the expansion across a remount, the way windowing unmounts a row', async () => {
    const { pane } = makePane();
    const first = await mountMessage(LONG, { pane });

    toggle(first)!.click();
    await waitFor(() => toggle(first)?.textContent === 'Show less', 'expanded');

    const { app, host } = mounted.pop()!;
    await unmount(app);
    host.remove();

    const second = await mountMessage(LONG, { pane });
    expect(toggle(second)?.textContent).toBe('Show less');
    expect(clip(second).clientHeight).toBe(clip(second).scrollHeight);
    expect(getComputedStyle(clip(second)).maskImage).toBe('none');
  });

  it('does not carry one message\'s expansion onto another', async () => {
    const { pane } = makePane();
    const host = await mountMessage(LONG, { pane });
    toggle(host)!.click();
    await waitFor(() => toggle(host)?.textContent === 'Show less', 'expanded');

    expect(pane.isUserMessageExpanded('item-1')).toBe(true);
    expect(pane.isUserMessageExpanded('item-2')).toBe(false);
  });

  it('re-measures when a narrower pane re-wraps the text past the threshold', async () => {
    const host = await mountMessage(REWRAPS, { width: 900 });
    expect(toggle(host)).toBeNull();
    expect(clip(host).getAttribute('data-clamped')).toBeNull();

    host.style.width = '260px';
    await waitFor(() => toggle(host) !== null, 'control appears after re-wrap');
    expect(clip(host).getAttribute('data-clamped')).toBe('true');

    host.style.width = '900px';
    await waitFor(() => toggle(host) === null, 'control retires after re-wrap back');
  });

  it('re-measures on collapse after the expanded text stopped overflowing', async () => {
    const { pane } = makePane();
    const host = await mountMessage(REWRAPS, { width: 260, pane });
    expect(toggle(host)?.textContent).toBe('Show more');

    toggle(host)!.click();
    await waitFor(() => toggle(host)?.textContent === 'Show less', 'expanded narrow text');
    host.style.width = '900px';
    await raf();

    toggle(host)!.click();
    await waitFor(
      () => toggle(host) === null,
      'stale overflow state to clear after collapse',
    );
    expect(clip(host).scrollHeight).toBe(clip(host).clientHeight);
    expect(clip(host).getAttribute('data-clamped')).toBeNull();
  });

  it('never applies a previous text\'s overflow result to replacement text', async () => {
    const { host, rerender } = await mountRerenderableMessage(LONG, { width: 900 });
    expect(toggle(host)?.textContent).toBe('Show more');

    await rerender({ item: makeItem(SHORT) });
    expect(toggle(host)).toBeNull();

    const staleInsertions: Element[] = [];
    const mutations = new MutationObserver((records) => {
      for (const record of records) {
        for (const node of record.addedNodes) {
          if (!(node instanceof Element)) continue;
          if (node.matches('[data-testid="user-message-clamp-toggle"]')) {
            staleInsertions.push(node);
          }
          staleInsertions.push(
            ...node.querySelectorAll('[data-testid="user-message-clamp-toggle"]'),
          );
        }
      }
    });
    mutations.observe(host, { childList: true, subtree: true });

    // REWRAPS passes the cheap clampability bound but fits at this width. A
    // cached `true` from LONG must not insert a stale control while the new
    // paragraph waits for its pre-paint geometry measurement.
    await rerender({ item: makeItem(REWRAPS) });
    await raf();
    mutations.disconnect();

    expect(toggle(host)).toBeNull();
    expect(staleInsertions).toEqual([]);
  });
});
