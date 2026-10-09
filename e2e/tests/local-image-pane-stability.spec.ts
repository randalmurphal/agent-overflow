// A settled local image in one pane stays put while the app does things
// that are not about that pane: a side-chat fork beside it, another pane's
// composer taking focus, a send from that other pane, focus coming back.
// And when the reader scrolls away far enough for the virtualizer to drop
// the row and comes back, the row remounts with its image already painted
// at its natural size: no placeholder, no second fetch.
//
// Coverage: the `![x](/abs/path.png)` an agent writes renders through
// StreamdownImageHost. Once its bytes have landed, no later pane-level event
// may tear the host down and refetch (which paints the "Loading image…"
// placeholder, drops the row by the image's height, clamps the viewport,
// then grows it back as a visible glide), and the source pane's viewport
// must not move at all on events that change none of its layout (another
// pane's composer focus). A remount reads the bytes and pixel size from the
// media blob cache in the frame it mounts. Per-frame scroll geometry and a
// DOM mutation counter on the source pane's timeline are the evidence; the
// thresholds follow remountReturn.browser.test.ts (8px dip, 2px frame
// reversal). Spec: docs/architecture/frontend-scroll.md (Row And Payload
// State: async-short remount content is bridged at the content layer).
import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { deflateSync } from 'node:zlib';
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { plainScenario, setScenario } from './thread-tools-helpers.js';

const SOURCE_TITLE = 'Image stability';
const IMAGE_PX = 400;
const SETTLE_FRAMES = 45;
// Enough history above the image turn that a wheel to the head takes the
// image row out of the virtualizer's window at 1280x720.
const FILLER_TURNS = 40;

function sourcePane(page: Page) {
  return page.locator('section[data-pane-kind="thread"]');
}

function sideChatPane(page: Page) {
  return page.locator('section[data-pane-kind="side-chat"]');
}

// Minimal truecolour PNG encoder: one flat colour, so the file is a few
// hundred bytes regardless of its dimensions.
const CRC_TABLE = new Uint32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff]! ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([length, body, crc]);
}

function solidPng(width: number, height: number, rgb: [number, number, number]): Buffer {
  const row = Buffer.alloc(1 + width * 3);
  for (let x = 0; x < width; x++) {
    row[1 + x * 3] = rgb[0];
    row[2 + x * 3] = rgb[1];
    row[3 + x * 3] = rgb[2];
  }
  const raw = Buffer.concat(Array.from({ length: height }, () => row));
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

interface PhaseStats {
  frames: number;
  /** Largest single-frame scrollTop decrease. */
  maxFrameDropPx: number;
  /** Largest distance-from-bottom observed. */
  maxDistancePx: number;
  /** Largest scrollHeight drop below the running peak. */
  maxDipPx: number;
  /** Smallest rendered height of the image element (or -1 when absent). */
  minImageHeightPx: number;
  /** Smallest rendered height of the image element while one was mounted. */
  minMountedImageHeightPx: number;
  /** Frames in which no image element was mounted. */
  framesWithoutImage: number;
  /** `img[data-markdown-image-src]` elements added under the timeline. */
  imageAdds: number;
  /** When each of those adds landed, in ms, for reading a churn pattern. */
  imageAddTimes: number[];
  /** `[data-streamdown-image-loading]` placeholders added under the timeline. */
  loadingAdds: number;
  /** Ancestor layers re-created under the timeline while the phase ran,
   *  outermost first, each as `layer@ms`. A refetch with no entry here is
   *  the image host alone re-running; a `block` entry is its paragraph
   *  being rebuilt; `row` is the virtualizer remounting the message. */
  remountLevels: string[];
}

type Stats = Record<string, PhaseStats>;

interface MonitorWindow {
  __aoImageStability?: { phase: string; stats: Stats };
}

/** Install the per-frame sampler and the mutation counter on the source pane. */
async function installMonitor(page: Page): Promise<void> {
  await page.evaluate(() => {
    const pane = document.querySelector('section[data-pane-kind="thread"]');
    const scroller = pane?.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
    if (!scroller) throw new Error('source pane scroller not found');
    const state: { phase: string; stats: Stats; prevTop: number; heightPeak: number } = {
      phase: 'idle',
      stats: {},
      prevTop: scroller.scrollTop,
      heightPeak: scroller.scrollHeight,
    };
    const phaseStats = (): PhaseStats => {
      let stats = state.stats[state.phase];
      if (!stats) {
        stats = {
          frames: 0,
          maxFrameDropPx: 0,
          maxDistancePx: 0,
          maxDipPx: 0,
          minImageHeightPx: Number.POSITIVE_INFINITY,
          minMountedImageHeightPx: Number.POSITIVE_INFINITY,
          framesWithoutImage: 0,
          imageAdds: 0,
          imageAddTimes: [],
          loadingAdds: 0,
          remountLevels: [],
        };
        state.stats[state.phase] = stats;
      }
      return stats;
    };
    const matches = (node: Node, selector: string): boolean =>
      node instanceof Element && (node.matches(selector) || node.querySelector(selector) !== null);
    // Records are delivered after the whole flush, so a row wrapper, the
    // paragraph inside it and the image span inside that each "contain" the
    // one <img> by the time they are examined. Count each element once.
    const counted = new WeakSet<Element>();
    const countFresh = (node: Element, selector: string): number => {
      const found = node.matches(selector) ? [node] : Array.from(node.querySelectorAll(selector));
      let fresh = 0;
      for (const element of found) {
        if (counted.has(element)) continue;
        counted.add(element);
        fresh += 1;
      }
      return fresh;
    };
    // Outermost first: the first selector an added node is or contains names
    // the layer that was re-created.
    const LEVELS: Array<[string, string]> = [
      ['row', '[data-item-id]'],
      ['markdown-root', '.markdown-body'],
      ['streamdown-root', '.md-committed'],
      ['block', '[data-streamdown-paragraph], .md-blk'],
    ];
    const observer = new MutationObserver((records) => {
      const stats = phaseStats();
      const now = Math.round(performance.now());
      for (const record of records) {
        for (const node of record.addedNodes) {
          if (!(node instanceof Element)) continue;
          const images = countFresh(node, 'img[data-markdown-image-src]');
          stats.imageAdds += images;
          for (let i = 0; i < images; i += 1) stats.imageAddTimes.push(now);
          stats.loadingAdds += countFresh(node, '[data-streamdown-image-loading]');
          const level = LEVELS.find(([, selector]) => matches(node, selector))?.[0];
          if (level) stats.remountLevels.push(`${level}@${now}`);
        }
      }
    });
    observer.observe(scroller, { childList: true, subtree: true });
    const tick = (): void => {
      const stats = phaseStats();
      const top = scroller.scrollTop;
      const height = scroller.scrollHeight;
      const distance = height - scroller.clientHeight - top;
      const drop = state.prevTop - top;
      if (drop > stats.maxFrameDropPx) stats.maxFrameDropPx = drop;
      if (distance > stats.maxDistancePx) stats.maxDistancePx = distance;
      if (height > state.heightPeak) state.heightPeak = height;
      const dip = state.heightPeak - height;
      if (dip > stats.maxDipPx) stats.maxDipPx = dip;
      const img = scroller.querySelector<HTMLImageElement>('img[data-markdown-image-src]');
      const imageHeight = img ? img.getBoundingClientRect().height : -1;
      if (imageHeight < stats.minImageHeightPx) stats.minImageHeightPx = imageHeight;
      if (img) {
        if (imageHeight < stats.minMountedImageHeightPx) stats.minMountedImageHeightPx = imageHeight;
      } else {
        stats.framesWithoutImage += 1;
      }
      stats.frames += 1;
      state.prevTop = top;
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
    (window as MonitorWindow).__aoImageStability = state;
  });
}

async function beginPhase(page: Page, label: string): Promise<void> {
  await page.evaluate((phase) => {
    const monitor = (window as MonitorWindow).__aoImageStability;
    if (!monitor) throw new Error('monitor not installed');
    monitor.phase = phase;
  }, label);
}

async function readStats(page: Page): Promise<Stats> {
  return await page.evaluate(() => {
    const monitor = (window as MonitorWindow).__aoImageStability;
    if (!monitor) throw new Error('monitor not installed');
    return JSON.parse(JSON.stringify(monitor.stats)) as Stats;
  });
}

async function waitFrames(page: Page, count: number): Promise<void> {
  await page.evaluate(
    (n) =>
      new Promise<void>((resolve) => {
        let seen = 0;
        const step = (): void => {
          seen += 1;
          if (seen >= n) resolve();
          else requestAnimationFrame(step);
        };
        requestAnimationFrame(step);
      }),
    count,
  );
}

/** Geometry-quiet and at the bottom for `stableFrames` consecutive frames. */
async function waitForQuietBottom(page: Page, label: string): Promise<void> {
  const ok = await page.evaluate(
    ({ stableFrames, frameBudget }) =>
      new Promise<boolean>((resolve) => {
        const pane = document.querySelector('section[data-pane-kind="thread"]');
        const scroller = pane?.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
        if (!scroller) {
          resolve(false);
          return;
        }
        let stable = 0;
        let lastHeight = -1;
        let frames = 0;
        const step = (): void => {
          frames += 1;
          const height = scroller.scrollHeight;
          const distance = height - scroller.clientHeight - scroller.scrollTop;
          if (height === lastHeight && distance <= 1) {
            stable += 1;
            if (stable >= stableFrames) {
              resolve(true);
              return;
            }
          } else {
            stable = 0;
            lastHeight = height;
          }
          if (frames >= frameBudget) resolve(false);
          else requestAnimationFrame(step);
        };
        requestAnimationFrame(step);
      }),
    { stableFrames: 30, frameBudget: 900 },
  );
  expect(ok, `quiet bottom: ${label}`).toBe(true);
}

test('a settled local image stays put across a side-chat fork, foreign composer focus and a foreign send', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'image-stability',
        repo: {},
        threads: [
          {
            title: SOURCE_TITLE,
            provider: 'claude',
            turns: [
              {
                userText: 'How does the review pane lay out its overview sections and file slabs?',
                items: [
                  {
                    kind: 'assistant_text',
                    summary:
                      'The overview sections and the file slabs share one inset variable, so the page margin beside every review row is the same width and the two cannot drift apart.\n\nTuning it later is a one-line change.',
                  },
                ],
                repeat: FILLER_TURNS,
              },
            ],
          },
        ],
      },
    ],
  });
  const project = seed.projects[0]!;
  const imagePath = join(project.path, 'overview-open.png');
  await writeFile(imagePath, solidPng(IMAGE_PX, IMAGE_PX, [40, 80, 160]));
  await setScenario(
    harness,
    project.path,
    plainScenario({
      name: 'image-stability-source',
      provider: 'claude',
      texts: [`Fresh screenshot from the overview spec:\n\n![overview](${imagePath})\n\nSections and file slab share the same edges.`],
    }),
  );

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: SOURCE_TITLE }).click();
  await sourcePane(page).getByLabel('Message Input').fill('show me');
  await sourcePane(page).getByTestId('composer-send').click();
  await harness.waitForEvent('provider:turn_completed');

  // The image has decoded at its natural size before anything is measured.
  const image = sourcePane(page).locator('img[data-markdown-image-src]');
  await expect(image).toHaveCount(1);
  await expect
    .poll(async () => await image.evaluate((img: HTMLImageElement) => img.complete && img.naturalHeight))
    .toBe(IMAGE_PX);
  expect((await image.boundingBox())?.height).toBeGreaterThanOrEqual(IMAGE_PX - 1);
  await waitForQuietBottom(page, 'after the image turn');
  await installMonitor(page);

  // The side chat's own turns answer plainly; the scenario is looked up by
  // workspace at launch, so this replaces the image reply for later mocks.
  await setScenario(
    harness,
    project.path,
    plainScenario({ name: 'image-stability-side', provider: 'claude', texts: ['Side answer.'] }),
  );

  await beginPhase(page, 'fork');
  await sourcePane(page).getByLabel('Message Input').fill('/side-chat');
  await sourcePane(page).getByTestId('composer-send').click();
  await expect(sideChatPane(page)).toHaveCount(1);
  await expect(sideChatPane(page).getByTestId('side-chat-preparing')).toHaveCount(0);
  await expect(sideChatPane(page).locator('img[data-markdown-image-src]')).toHaveCount(1);
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'focus-side');
  await sideChatPane(page).getByLabel('Message Input').click();
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'send-side');
  await sideChatPane(page).getByLabel('Message Input').fill('and from here?');
  await sideChatPane(page).getByTestId('composer-send').click();
  await harness.waitForEvent('provider:turn_completed');
  await expect(sideChatPane(page).getByTestId('assistant-message-body').last()).toContainText('Side answer.');
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'focus-source');
  await sourcePane(page).getByLabel('Message Input').click();
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'focus-side-again');
  await sideChatPane(page).getByLabel('Message Input').click();
  await waitFrames(page, SETTLE_FRAMES);

  // The reader wheels to the head of the history, far enough that the
  // virtualizer drops the image row, then wheels back to the bottom.
  const scroller = sourcePane(page).getByTestId('message-timeline-scroll');
  await beginPhase(page, 'scroll-away');
  await scroller.hover();
  for (let attempt = 0; attempt < 12; attempt += 1) {
    await page.mouse.wheel(0, -5000);
    if (await scroller.evaluate((element) => element.scrollTop <= 1)) break;
  }
  await expect(image).toHaveCount(0);
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'return');
  for (let attempt = 0; attempt < 12; attempt += 1) {
    await page.mouse.wheel(0, 5000);
    const atBottom = await scroller.evaluate(
      (element) => element.scrollHeight - element.clientHeight - element.scrollTop <= 1,
    );
    if (atBottom) break;
  }
  await expect(image).toHaveCount(1);
  await expect
    .poll(async () => await image.evaluate((img: HTMLImageElement) => img.complete && img.naturalHeight))
    .toBe(IMAGE_PX);
  await waitFrames(page, SETTLE_FRAMES);

  await beginPhase(page, 'done');
  const stats = await readStats(page);
  const report = JSON.stringify(stats, null, 2);

  for (const phase of ['fork', 'focus-side', 'send-side', 'focus-source', 'focus-side-again']) {
    const s = stats[phase];
    expect(s, `phase ${phase} recorded\n${report}`).toBeDefined();
    expect(s!.frames, `phase ${phase} frames`).toBeGreaterThanOrEqual(SETTLE_FRAMES);
    // The image host is never torn down and refetched behind the reader.
    expect(s!.loadingAdds, `phase ${phase}: loading placeholder re-added\n${report}`).toBe(0);
    expect(s!.imageAdds, `phase ${phase}: image element re-added\n${report}`).toBe(0);
    expect(s!.minImageHeightPx, `phase ${phase}: image shrank\n${report}`).toBeGreaterThanOrEqual(IMAGE_PX - 1);
    // Nothing above the viewport collapses: the content never gets shorter.
    expect(s!.maxDipPx, `phase ${phase}: scrollHeight dipped\n${report}`).toBeLessThanOrEqual(8);
  }
  // Another pane's composer focus changes none of this pane's layout, so its
  // viewport has no reason to move at all.
  for (const phase of ['focus-side', 'focus-source', 'focus-side-again']) {
    const s = stats[phase]!;
    expect(s.maxFrameDropPx, `phase ${phase}: viewport moved\n${report}`).toBeLessThanOrEqual(2);
    expect(s.maxDistancePx, `phase ${phase}: viewport left the bottom\n${report}`).toBeLessThanOrEqual(2);
  }
  // The scroll away really dropped the row (otherwise the return proves
  // nothing), and the remount painted the image from the cache: one new
  // element, at its natural size from its first frame, with no placeholder.
  // A cache miss would paint "Loading image…" before the bytes, so a zero
  // placeholder count is also the proof that nothing was fetched again.
  const away = stats['scroll-away']!;
  expect(away.framesWithoutImage, `scroll-away: image row never left the window\n${report}`).toBeGreaterThan(0);
  const back = stats['return']!;
  expect(back.loadingAdds, `return: loading placeholder painted on remount\n${report}`).toBe(0);
  expect(back.imageAdds, `return: image element added\n${report}`).toBe(1);
  expect(back.minMountedImageHeightPx, `return: image mounted short\n${report}`).toBeGreaterThanOrEqual(IMAGE_PX - 1);
});
