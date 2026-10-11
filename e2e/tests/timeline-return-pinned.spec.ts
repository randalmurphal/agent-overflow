// A reader pinned to the bottom of a long thread, who leaves while its turn
// runs and returns after it finished, lands on the newest content or is
// offered the jump-to-latest chip.
//
// The return pins the fetched tail. Its rows are heavy in bytes and short on
// screen, so the viewport fill pages older history in under a paused
// auto-scroll. A row at the tail grows while that page is in flight (here a
// diagram finishing its render), below the pinned viewport. The older page
// lands as a head splice, which arms the virtualizer's reading hold, and the
// pause release pins to the new bottom. The hold must not then pull the
// viewport back up by the growth (docs/architecture/frontend-scroll.md,
// Load Paging).
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const MAIN = 'Pinned return main';
const OTHER = 'Pinned return other';
const BEFORE_LEAVE = 'Before leaving.';
const FINISHED = 'Finished while away.';
// methodRoutes.ts: ListItemsBeforeCursor.
const LIST_ITEMS_BEFORE_CURSOR = 162135710;

const paragraph = (n: number) =>
  `Paragraph ${n}. ` + 'The seeded history is long enough that the first page is a slice of it. '.repeat(6);
const heavySummary = Array.from({ length: 24 }, (_, n) => paragraph(n)).join(' ');

function toolCall(n: number): string[] {
  const id = `toolu_${n}`;
  return [
    toolUseLine(`msg_tool_${n}`, id, 'Bash', { command: `echo step ${n}`, description: `Step ${n}` }),
    toolResultLine(id, `step ${n}\n`),
  ];
}

function scenario(): unknown {
  const calls = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => toolCall(from + i)).flat();
  const diagram = ['```mermaid', 'graph TD', ...Array.from({ length: 3 }, (_, n) => `  S${n}[Stage ${n}] --> S${n + 1}[Stage ${n + 1}]`), '```'].join('\n');
  const finished = `${FINISHED}\n\n${diagram}`;
  return claudeScenario('timeline-return-pinned', [
    emit([...textLines('msg_before', BEFORE_LEAVE), ...calls(1, 6)]),
    { waitSignal: { name: 'away' } },
    emit([...calls(7, 20), ...textLines('msg_finished', finished), RESULT_LINE]),
  ]);
}

async function seedThreads(harness: HarnessApp): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'timeline-return-pinned',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
      threads: [
        {
          title: MAIN,
          provider: 'claude',
          // Tool calls collapse into short runs while their summaries fill
          // the page's byte ceiling, so the first page is shorter than the
          // viewport fill wants.
          turns: [{
            userText: 'keep going',
            repeat: 300,
            items: [
              ...Array.from({ length: 4 }, (_, n) => ({ kind: 'tool_call', toolName: 'Bash', summary: `step ${n} ${heavySummary}` })),
              { kind: 'assistant_text', summary: 'Done with this pass.' },
            ],
          }],
        },
        { title: OTHER, provider: 'claude', turns: [{ userText: 'elsewhere', items: [{ kind: 'assistant_text', summary: 'Other ready.' }] }] },
      ],
    }],
  });
  return seed.projects[0].threadIds[0];
}

// Holds the backend's answers to the page's older-history reads for one
// thread until released, so the test decides what lands while one is in
// flight.
async function holdOlderPages(page: Page, threadId: string) {
  let holding = false;
  const calls = new Set<string>();
  const held: Array<string | Buffer> = [];
  let send: ((message: string | Buffer) => void) | undefined;
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    send = (message) => socket.send(message);
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (holding && frame.type === 'rpc' && frame.methodId === LIST_ITEMS_BEFORE_CURSOR && frame.params?.[0] === threadId) {
        calls.add(frame.id);
      }
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && calls.has(frame.id)) held.push(message);
      else socket.send(message);
    });
  });
  return {
    hold: () => { holding = true; },
    heldCount: () => held.length,
    release: () => {
      holding = false;
      calls.clear();
      for (const message of held.splice(0)) send?.(message);
    },
  };
}

async function geometry(page: Page) {
  return page.getByTestId('message-timeline-scroll').evaluate((el) => ({
    distance: Math.round(el.scrollHeight - el.scrollTop - el.clientHeight),
    scrollHeight: el.scrollHeight,
    chip: document.querySelector('[data-testid="scroll-to-bottom"]') !== null,
  }));
}

// Every painted frame once the older page has landed (the content grows
// past `fromHeight`): how far the viewport sat above the bottom while no
// jump-to-latest chip was offered. The splice and the pause release run in
// one task, so no frame paints between them.
async function sampleStrandedFrames(page: Page, fromHeight: number): Promise<() => Promise<{ frames: number; worst: number }>> {
  await page.evaluate((fromHeight) => {
    const el = document.querySelector('[data-testid="message-timeline-scroll"]') as HTMLElement;
    const state = { frames: 0, worst: 0, running: true };
    (window as unknown as { __aoStranded: typeof state }).__aoStranded = state;
    const tick = () => {
      if (!state.running) return;
      if (el.scrollHeight <= fromHeight) {
        requestAnimationFrame(tick);
        return;
      }
      state.frames += 1;
      const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
      if (document.querySelector('[data-testid="scroll-to-bottom"]') === null) {
        state.worst = Math.max(state.worst, Math.round(distance));
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, fromHeight);
  return () => page.evaluate(() => {
    const state = (window as unknown as { __aoStranded: { frames: number; worst: number; running: boolean } }).__aoStranded;
    state.running = false;
    return { frames: state.frames, worst: state.worst };
  });
}

// Settled: the content height and position held still for a stretch of
// frames, so a late correction cannot land after the assertion.
async function settledGeometry(page: Page) {
  let last = '';
  let stable = 0;
  let sample = await geometry(page);
  for (let i = 0; i < 300 && stable < 20; i++) {
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(resolve)));
    sample = await geometry(page);
    const key = JSON.stringify(sample);
    stable = key === last ? stable + 1 : 0;
    last = key;
  }
  expect(stable, `geometry settles: ${last}`).toBeGreaterThanOrEqual(20);
  return sample;
}

test('a pinned reader whose turn finishes while the return loads history lands on the latest', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: scenario() });
  const threadId = await seedThreads(harness);
  const olderPages = await holdOlderPages(page, threadId);
  await harness.open(page);
  const timeline = page.getByTestId('message-timeline-scroll');
  const open = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();

  await open(MAIN);
  await expect(timeline).toBeVisible();
  const mockId = await startMock(harness, threadId);
  const awayGate = waitForGate(harness, 'away');
  await harness.rpc('SendMessage', threadId, 'one more pass', null);
  await awayGate;
  await expect(timeline.getByText(BEFORE_LEAVE)).toBeVisible();
  expect((await settledGeometry(page)).distance, 'pinned before leaving').toBeLessThanOrEqual(2);

  await open(OTHER);
  await expect(timeline.getByText('Other ready.')).toBeVisible();
  const completed = harness.waitForEvent('provider:turn_completed');
  await advance(harness, mockId, 'away');
  await completed;

  olderPages.hold();
  await open(MAIN);
  await expect.poll(() => olderPages.heldCount(), { message: 'the return pages older history in' }).toBeGreaterThan(0);
  await expect(timeline.getByText(FINISHED)).toBeAttached();
  await expect(timeline.locator('.streamdown-mermaid-host svg').first()).toBeAttached({ timeout: 30_000 });
  // The precondition: the diagram grew the tail while the fill's pause held
  // the pinned viewport, so the release has somewhere to pin to.
  const beforeSplice = await settledGeometry(page);
  expect(beforeSplice.distance, 'tail grew under the paused fill').toBeGreaterThan(2);
  expect(beforeSplice.chip, 'follow intent kept through the pause').toBe(false);
  // The older page adds far more than any relayout below the viewport.
  const stranded = await sampleStrandedFrames(page, beforeSplice.scrollHeight + 200);
  olderPages.release();

  const returned = await settledGeometry(page);
  expect(returned.chip || returned.distance <= 2, JSON.stringify(returned)).toBe(true);
  // The splice above and its measurement must not move a pinned viewport
  // off the bottom for even one painted frame.
  const frames = await stranded();
  expect(frames.frames).toBeGreaterThan(10);
  expect(frames.worst, 'largest distance from the bottom with no chip, in px').toBeLessThanOrEqual(2);
});
