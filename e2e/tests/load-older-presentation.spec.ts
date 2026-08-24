import {
  test,
  expect,
  type Browser,
  type BrowserContext,
  type Page,
  type WebSocketRoute,
} from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';

const THREAD_TITLE = 'Delayed history presentation';
const SEEDED_TURNS = 260;
const LIST_ITEMS_BEFORE_CURSOR_ID = fnv1a32('main.App.ListItemsBeforeCursor');

interface HeldRpc {
  waitForResponse(): Promise<void>;
  release(): void;
}

interface FrameCapture {
  readonly frames: string[];
  stop(): Promise<void>;
}

interface ContrastSummary {
  readonly baseline: number;
  readonly minimum: number;
  readonly frameCount: number;
}

interface CollisionResult extends ContrastSummary {
  readonly animationName: string;
  readonly anchorDrift: number;
}

interface ElementBounds {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
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

async function startFrameCapture(context: BrowserContext, page: Page): Promise<FrameCapture> {
  const cdp = await context.newCDPSession(page);
  const frames: string[] = [];
  const acknowledgementErrors: Error[] = [];
  cdp.on('Page.screencastFrame', (event) => {
    if (frames.length < 120) frames.push(event.data);
    void cdp.send('Page.screencastFrameAck', { sessionId: event.sessionId }).catch((error) => {
      acknowledgementErrors.push(error instanceof Error ? error : new Error(String(error)));
    });
  });
  await cdp.send('Page.startScreencast', {
    format: 'jpeg',
    quality: 90,
    everyNthFrame: 1,
  });
  return {
    frames,
    async stop() {
      await cdp.send('Page.stopScreencast');
      await cdp.detach();
      if (acknowledgementErrors.length > 0) throw acknowledgementErrors[0];
    },
  };
}

async function analyzeContrast(
  page: Page,
  frames: readonly string[],
  bounds: { x: number; y: number; width: number; height: number },
): Promise<ContrastSummary> {
  const contrasts = await page.evaluate(async ({ encodedFrames, bounds, viewport }) => {
    const values: number[] = [];
    for (const encoded of encodedFrames) {
      const image = new Image();
      image.src = `data:image/jpeg;base64,${encoded}`;
      await image.decode();
      const canvas = document.createElement('canvas');
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const context = canvas.getContext('2d', { willReadFrequently: true });
      if (!context) throw new Error('2D canvas unavailable while reading compositor frames');
      context.drawImage(image, 0, 0);
      const scaleX = canvas.width / viewport.width;
      const scaleY = canvas.height / viewport.height;
      const x = Math.max(0, Math.floor(bounds.x * scaleX));
      const y = Math.max(0, Math.floor(bounds.y * scaleY));
      const width = Math.min(canvas.width - x, Math.max(1, Math.ceil(bounds.width * scaleX)));
      const height = Math.min(canvas.height - y, Math.max(1, Math.ceil(bounds.height * scaleY)));
      const pixels = context.getImageData(x, y, width, height).data;
      let sum = 0;
      let squareSum = 0;
      const count = pixels.length / 4;
      for (let offset = 0; offset < pixels.length; offset += 4) {
        const luminance = 0.2126 * pixels[offset]
          + 0.7152 * pixels[offset + 1]
          + 0.0722 * pixels[offset + 2];
        sum += luminance;
        squareSum += luminance * luminance;
      }
      const mean = sum / count;
      values.push(Math.sqrt(Math.max(0, squareSum / count - mean * mean)));
    }
    return values;
  }, {
    encodedFrames: frames,
    bounds,
    viewport: page.viewportSize()!,
  });
  if (contrasts.length === 0) throw new Error('screencast produced no compositor frames');
  const baselineFrames = contrasts.slice(0, Math.min(5, contrasts.length)).sort((a, b) => a - b);
  const baseline = baselineFrames[Math.floor(baselineFrames.length / 2)];
  return {
    baseline,
    minimum: Math.min(...contrasts),
    frameCount: contrasts.length,
  };
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
    await page.goto(harness.url);
    await page.getByText(THREAD_TITLE).click();
    await scrollToHistoryHead(page);
    if (disableSpinner) {
      await page.addStyleTag({ content: '.animate-spin { animation: none !important; }' });
    }

    const button = page.getByTestId('load-older-messages');
    // Reaching the history head gesture-arms the production auto-load gate.
    // The request is already in flight here, held by the WebSocket proxy.
    await held.waitForResponse();
    const spinner = button.locator('.animate-spin');
    await expect(spinner).toBeVisible();
    const animationName = await spinner.evaluate((element) => getComputedStyle(element).animationName);
    expect(animationName === 'none').toBe(disableSpinner);

    const anchor = await visibleAnchor(page);
    const anchorHandle = await page.locator(`[data-item-id="${anchor.id}"]`).elementHandle();
    if (!anchorHandle) throw new Error('presentation anchor disappeared before capture');
    const capture = await startFrameCapture(context, page);

    const pressure = emitStreamingPressure(harness, threadId, disableSpinner ? 'control' : 'animated');
    if (disableSpinner) {
      for (let step = 0; step < 8; step += 1) {
        await spinner.evaluate((element, value) => {
          (element as HTMLElement).style.opacity = value % 2 === 0 ? '0.75' : '1';
        }, step);
        await page.evaluate(() => new Promise(requestAnimationFrame));
      }
    } else {
      await expect.poll(() => capture.frames.length).toBeGreaterThanOrEqual(5);
    }

    held.release();
    await pressure;
    await expect(spinner).toHaveCount(0);
    await page.evaluate(async () => {
      for (let frame = 0; frame < 12; frame += 1) await new Promise(requestAnimationFrame);
    });
    await expect.poll(() => capture.frames.length).toBeGreaterThanOrEqual(disableSpinner ? 3 : 10);
    await capture.stop();

    const anchorAfter = page.locator(`[data-item-id="${anchor.id}"]`);
    const anchorAfterHandle = await anchorAfter.elementHandle();
    if (!anchorAfterHandle) throw new Error('presentation anchor has no DOM handle after prepend');
    const sameNode = await anchorHandle.evaluate((before, after) => before === after, anchorAfterHandle);
    expect(sameNode, 'head prepend must preserve the anchor DOM node').toBe(true);
    const afterBounds = await anchorAfter.boundingBox();
    if (!afterBounds) throw new Error('presentation anchor disappeared after prepend');
    const anchorDrift = Math.abs(afterBounds.y - anchor.bounds.y);
    expect(anchorDrift).toBeLessThanOrEqual(1);

    const contrast = await analyzeContrast(page, capture.frames, anchor.bounds);
    expect(contrast.baseline, 'anchor crop must contain visible ink').toBeGreaterThan(4);
    expect(
      contrast.minimum,
      `anchor crop blanked in a compositor frame (${contrast.minimum} vs baseline ${contrast.baseline})`,
    ).toBeGreaterThanOrEqual(contrast.baseline * 0.4);
    return { ...contrast, animationName, anchorDrift };
  } finally {
    await context.close();
  }
}

test('delayed load-older prepend never presents blank tiles while a turn streams', async ({ browser }) => {
  const harness = await launchHarness();
  try {
    const turns = Array.from({ length: SEEDED_TURNS }, (_, index) => ({
      userText: `Historical question ${index}`,
      items: [{
        kind: 'assistant_text',
        summary: `Historical response ${index} with enough stable text to leave visible raster ink.`,
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
    expect(animated.minimum / animated.baseline).toBeGreaterThanOrEqual(
      (control.minimum / control.baseline) * 0.8,
    );
  } finally {
    await harness.close();
  }
});
