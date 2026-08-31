import {
  test,
  expect,
  type Browser,
  type Page,
  type WebSocketRoute,
} from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startCompositorTrace, summarizeCompositorWindow } from '../src/compositorTrace.js';

const THREAD_TITLE = 'Delayed history presentation';
const SEEDED_TURNS = 260;
const LIST_ITEMS_BEFORE_CURSOR_ID = fnv1a32('main.App.ListItemsBeforeCursor');

interface HeldRpc {
  waitForResponse(): Promise<void>;
  release(): void;
}

interface CollisionResult {
  readonly animationName: string;
  readonly anchorDrift: number;
  readonly frameCount: number;
  readonly invalidFrames: number;
  readonly compositor: ReturnType<typeof summarizeCompositorWindow>;
}

interface ElementBounds {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

interface PresentationSample {
  readonly frame: number;
  readonly hasLayout: boolean;
  readonly hasText: boolean;
  readonly intersectsViewport: boolean;
  readonly clipped: boolean;
}

function fnv1a32(value: string): number {
  let hash = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(value)) {
    hash = Math.imul(hash ^ byte, 0x01000193) >>> 0;
  }
  return hash;
}

function asText(message: string | Buffer): string {
  return typeof message === 'string' ? message : message.toString('utf8');
}

async function delayPagingResponse(page: Page): Promise<HeldRpc> {
  let pageSocket: WebSocketRoute | null = null;
  let heldResponse: string | Buffer | null = null;
  let heldRequestId: string | null = null;
  let responseReady!: () => void;
  const response = new Promise<void>((resolve) => {
    responseReady = resolve;
  });

  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    pageSocket = socket;
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const text = asText(message);
      const frame = JSON.parse(text) as { type?: string; id?: string; methodId?: number };
      if (frame.type === 'rpc' && frame.methodId === LIST_ITEMS_BEFORE_CURSOR_ID) {
        if (heldRequestId !== null) {
          throw new Error('a second ListItemsBeforeCursor request overlapped the held one');
        }
        heldRequestId = frame.id ?? null;
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const text = asText(message);
      const frame = JSON.parse(text) as { type?: string; id?: string };
      if (heldRequestId !== null && frame.type === 'rpc' && frame.id === heldRequestId) {
        heldResponse = message;
        responseReady();
        return;
      }
      socket.send(message);
    });
  });

  return {
    waitForResponse: () => response,
    release() {
      if (!pageSocket || heldResponse === null) {
        throw new Error('paging response released before it was intercepted');
      }
      pageSocket.send(heldResponse);
      heldResponse = null;
      heldRequestId = null;
    },
  };
}

async function startPresentationMonitor(page: Page, anchorId: string): Promise<() => Promise<readonly PresentationSample[]>> {
  await page.evaluate((id) => {
    const state = { frame: 0, samples: [] as PresentationSample[], raf: 0 };
    const tick = () => {
      state.frame += 1;
      const scroller = document.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
      const anchor = document.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(id)}"]`);
      const viewport = scroller?.getBoundingClientRect();
      const rect = anchor?.getBoundingClientRect();
      const hasLayout = !!rect && rect.width > 0 && rect.height > 0;
      const intersectsViewport = !!rect && !!viewport
        && rect.bottom > viewport.top && rect.top < viewport.bottom;
      const clipped = !!rect && !!viewport
        && (rect.top < viewport.top || rect.bottom > viewport.bottom);
      state.samples.push({
        frame: state.frame,
        hasLayout,
        hasText: !!anchor?.textContent?.trim(),
        intersectsViewport,
        clipped,
      });
      if (state.samples.length > 120) state.samples.shift();
      state.raf = requestAnimationFrame(tick);
    };
    state.raf = requestAnimationFrame(tick);
    (window as typeof window & { __aoPresentationMonitor?: typeof state }).__aoPresentationMonitor = state;
  }, anchorId);
  return async () => page.evaluate(() => {
    const win = window as typeof window & {
      __aoPresentationMonitor?: { raf: number; samples: PresentationSample[] };
    };
    const monitor = win.__aoPresentationMonitor;
    if (!monitor) throw new Error('presentation monitor was not installed');
    cancelAnimationFrame(monitor.raf);
    delete win.__aoPresentationMonitor;
    return monitor.samples;
  });
}

async function scrollToHistoryHead(page: Page): Promise<void> {
  const scroller = page.getByTestId('message-timeline-scroll');
  await scroller.hover();
  for (let attempt = 0; attempt < 12; attempt += 1) {
    await page.mouse.wheel(0, -5000);
    if (await scroller.evaluate((element) => element.scrollTop <= 1)) return;
  }
  throw new Error('timeline never reached its history head');
}

async function waitForScrollSettle(page: Page): Promise<void> {
  await page.getByTestId('message-timeline-scroll').evaluate(async (scroller) => {
    let previous = scroller.scrollTop;
    let stableFrames = 0;
    for (let frame = 0; frame < 120; frame += 1) {
      await new Promise(requestAnimationFrame);
      const current = scroller.scrollTop;
      stableFrames = Math.abs(current - previous) <= 0.01 ? stableFrames + 1 : 0;
      previous = current;
      if (stableFrames >= 30) return;
    }
    throw new Error('timeline scroll gesture did not settle');
  });
}

async function visibleAnchor(page: Page): Promise<{ id: string; bounds: ElementBounds }> {
  const candidate = await page.getByTestId('message-timeline-scroll').evaluate((scroller) => {
    const viewport = scroller.getBoundingClientRect();
    return [...scroller.querySelectorAll<HTMLElement>('[data-item-id]')]
      .map((element) => ({ id: element.dataset.itemId ?? '', rect: element.getBoundingClientRect() }))
      .find(({ id, rect }) => id !== '' && rect.top >= viewport.top + 100 && rect.bottom <= viewport.bottom - 100)?.id;
  });
  if (!candidate) throw new Error('no fully visible presentation anchor found');
  const locator = page.locator(`[data-item-id="${candidate}"]`);
  const bounds = await locator.boundingBox();
  if (!bounds) throw new Error(`presentation anchor ${candidate} has no box`);
  return { id: candidate, bounds };
}

async function emitStreamingPressure(harness: HarnessApp, threadId: string, suffix: string): Promise<void> {
  const itemId = `presentation-stream-${suffix}`;
  await harness.rpc('HarnessEmit', 'provider:item_event', {
    action: 'upsert',
    threadId,
    item: {
      id: itemId,
      threadId,
      turnIndex: SEEDED_TURNS,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'streaming',
      createdAt: Date.now(),
      updatedAt: Date.now(),
    },
  });
  for (let index = 0; index < 20; index += 1) {
    await harness.rpc('HarnessEmit', 'provider:item_event', {
      action: 'delta',
      threadId,
      itemId,
      kind: 'assistant_text',
      delta: ` ${index}`,
      updatedAt: Date.now() + index + 1,
    });
  }
}

async function runCollision(
  browser: Browser,
  harness: HarnessApp,
  threadId: string,
  disableSpinner: boolean,
): Promise<CollisionResult> {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 1.5,
  });
  try {
    const page = await context.newPage();
    const held = await delayPagingResponse(page);
    await harness.open(page);
    await page.getByText(THREAD_TITLE).click();
    await scrollToHistoryHead(page);
    if (disableSpinner) {
      await page.addStyleTag({ content: '.animate-spin { animation: none !important; }' });
    }

    const button = page.getByTestId('load-older-messages');
    // Reaching the history head gesture-arms the production auto-load gate.
    // The request is already in flight here, held by the WebSocket proxy.
    await held.waitForResponse();
    // Chromium's wheel scrolling is compositor-driven on macOS and can keep
    // settling after scrollTop first reaches zero. The collision starts only
    // after that reader gesture is genuinely over; otherwise its residual
    // motion is (correctly) indistinguishable from anchor drift.
    await waitForScrollSettle(page);
    const spinner = button.locator('.animate-spin');
    await expect(spinner).toBeVisible();
    const animationName = await spinner.evaluate((element) => getComputedStyle(element).animationName);
    expect(animationName === 'none').toBe(disableSpinner);

    const anchor = await visibleAnchor(page);
    const anchorHandle = await page.locator(`[data-item-id="${anchor.id}"]`).elementHandle();
    if (!anchorHandle) throw new Error('presentation anchor disappeared before monitoring');
    const stopMonitor = await startPresentationMonitor(page, anchor.id);
    const compositorTrace = await startCompositorTrace(page);
    await page.evaluate(() => performance.mark('ao-load-older-start'));

    const pressure = emitStreamingPressure(harness, threadId, disableSpinner ? 'control' : 'animated');
    if (disableSpinner) {
      for (let step = 0; step < 8; step += 1) {
        await spinner.evaluate((element, value) => {
          (element as HTMLElement).style.opacity = value % 2 === 0 ? '0.75' : '1';
        }, step);
        await page.evaluate(() => new Promise(requestAnimationFrame));
      }
    } else {
      await expect.poll(() => page.evaluate(() => {
        const monitor = (window as typeof window & {
          __aoPresentationMonitor?: { samples: unknown[] };
        }).__aoPresentationMonitor;
        return monitor?.samples.length ?? 0;
      })).toBeGreaterThanOrEqual(5);
    }

    held.release();
    await pressure;
    await expect(spinner).toHaveCount(0);
    await page.evaluate(async () => {
      for (let frame = 0; frame < 12; frame += 1) await new Promise(requestAnimationFrame);
    });
    await page.evaluate(() => performance.mark('ao-load-older-end'));
    const compositor = summarizeCompositorWindow(
      await compositorTrace.stop(),
      'ao-load-older-start',
      'ao-load-older-end',
    );
    expect(compositor.eventCount, 'trace must contain events during the prepend').toBeGreaterThan(0);
    expect(compositor.renderPasses, 'prepend must reach the compositor').toBeGreaterThan(0);
    expect(compositor.prepareDraws, 'prepend must produce compositor draws').toBeGreaterThan(0);
    expect(compositor.rasterTasks, 'prepend must produce raster work').toBeGreaterThan(0);
    expect(compositor.missingTileSignals, 'prepend must not expose missing tiles').toBe(0);
    expect(compositor.checkerboardSignals, 'prepend must not expose checkerboard content').toBe(0);
    expect(compositor.blankRenderPasses, 'prepend must not expose blank render passes').toBe(0);
    const samples = await stopMonitor();
    expect(samples.length, 'rAF monitor must observe the prepend').toBeGreaterThanOrEqual(3);
    const invalidFrames = samples.filter(
      (sample) => !sample.hasLayout || !sample.hasText || !sample.intersectsViewport || sample.clipped,
    ).length;
    expect(invalidFrames, 'the retained anchor must stay in laid-out, unclipped DOM').toBe(0);

    const anchorAfter = page.locator(`[data-item-id="${anchor.id}"]`);
    const anchorAfterHandle = await anchorAfter.elementHandle();
    if (!anchorAfterHandle) throw new Error('presentation anchor has no DOM handle after prepend');
    const sameNode = await anchorHandle.evaluate((before, after) => before === after, anchorAfterHandle);
    expect(sameNode, 'head prepend must preserve the anchor DOM node').toBe(true);
    const afterBounds = await anchorAfter.boundingBox();
    if (!afterBounds) throw new Error('presentation anchor disappeared after prepend');
    const anchorDrift = Math.abs(afterBounds.y - anchor.bounds.y);
    expect(
      anchorDrift,
      `anchor moved from y=${anchor.bounds.y} to y=${afterBounds.y} while loading older`,
    ).toBeLessThanOrEqual(1);

    return { animationName, anchorDrift, frameCount: samples.length, invalidFrames, compositor };
  } finally {
    await context.close();
  }
}

test('delayed load-older prepend keeps the retained anchor laid out while a turn streams', async ({ browser }) => {
  const harness = await launchHarness();
  try {
    const turns = Array.from({ length: SEEDED_TURNS }, (_, index) => ({
      userText: `Historical question ${index}`,
      items: [{
        kind: 'assistant_text',
        summary: `Historical response ${index} with enough stable text to keep the anchor laid out.`,
      }],
    }));
    const seed = await harness.rpc<{
      projects: Array<{ threadIds: string[] }>;
    }>('HarnessSeed', {
      projects: [{
        name: 'delayed-history-presentation',
        repo: {},
        threads: [{ title: THREAD_TITLE, turns }],
      }],
    });
    const threadId = seed.projects[0].threadIds[0];

    const control = await runCollision(browser, harness, threadId, true);
    const animated = await runCollision(browser, harness, threadId, false);
    expect(animated.invalidFrames).toBe(control.invalidFrames);
    expect(animated.frameCount).toBeGreaterThan(0);
    expect(animated.compositor.renderPasses).toBeGreaterThan(0);
  } finally {
    await harness.close();
  }
});
