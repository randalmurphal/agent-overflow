// A stream the reader leaves and returns to keeps every word.
//
// The pane switches away while a thinking block or a reply streams. The
// stream continues while no delta reaches this client, and the pane comes
// back while it still streams, so the history read races the live deltas:
// the read must hold what the unwatched deltas carried (the backend writes
// its stream buffers before a history read), a delta that arrives before the
// read lands must not be joined to the cached row it does not continue, and
// the read must not be discarded for the rows those deltas touched. The
// settled row must also not claim the stored row's revision over text it
// lacks, or a reopen would verify the damage as fresh. That includes a reply
// left after it settled but before its reveal showed all of it.
//
// Every rendered frame is sampled, collapsed tail and reply alike, and every
// numbered token must be present in order: during the stream, after the turn
// settles, expanded, after a reopen and after a reload.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { RESULT_LINE, advance, startMock, waitForGate } from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const j = (value: unknown): string => JSON.stringify(value);

const MAIN = 'Stream return main';
const OTHER = 'Stream return other';
const REPLY_END = 'REPLY_END';

// Tokens streamed before the switch, while away, and after the return. The
// return phase streams fast and long, so deltas are in flight throughout
// the pane's history read.
const BEFORE = 60;
const AWAY = 120;
const TOTAL = 480;

type Block = 'thinking' | 'text';

function token(block: Block, n: number): string {
  return `${block === 'thinking' ? 'w' : 'p'}${String(n).padStart(3, '0')} `;
}

function tokens(block: Block, from: number, to: number): string[] {
  const out: string[] = [];
  for (let n = from; n <= to; n++) out.push(token(block, n));
  return out;
}

function blockStart(index: number, block: Block): string {
  const content_block = block === 'thinking' ? { type: 'thinking', thinking: '' } : { type: 'text', text: '' };
  return j({ type: 'stream_event', event: 'content_block_start', data: { type: 'content_block_start', index, content_block } });
}

function blockDelta(index: number, block: Block, text: string): string {
  const delta = block === 'thinking' ? { type: 'thinking_delta', thinking: text } : { type: 'text_delta', text };
  return j({ type: 'stream_event', event: 'content_block_delta', data: { type: 'content_block_delta', index, delta } });
}

function blockStop(index: number): string {
  return j({ type: 'stream_event', event: 'content_block_stop', data: { type: 'content_block_stop', index } });
}

// One turn whose `block` streams in three phases around two gates: before
// the switch, while away, and across the return. A thinking turn ends with
// a short reply; a reply turn has no thinking.
function scenario(block: Block): unknown {
  const index = 0;
  const streamed = tokens(block, 1, TOTAL).join('');
  const reply = block === 'thinking' ? `Answer. ${REPLY_END}` : `${streamed}${REPLY_END}`;
  const tail: string[] = [];
  if (block === 'thinking') {
    tail.push(blockStop(0), blockStart(1, 'text'), blockDelta(1, 'text', reply), blockStop(1));
  } else {
    tail.push(blockDelta(0, 'text', REPLY_END), blockStop(0));
  }
  const content = block === 'thinking'
    ? [{ type: 'thinking', thinking: streamed }, { type: 'text', text: reply }]
    : [{ type: 'text', text: reply }];
  tail.push(
    j({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
    j({ type: 'assistant', message: { id: 'msg-return', role: 'assistant', model: 'claude-mock-1', content } }),
    RESULT_LINE,
  );
  const deltas = (from: number, to: number) => tokens(block, from, to).map((text) => blockDelta(index, block, text));
  return {
    version: 1,
    name: `stream-switch-return-${block}`,
    provider: 'claude',
    turns: [{
      label: block,
      steps: [
        { emit: { lines: [
          j({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: { id: 'msg-return', role: 'assistant' } } }),
          blockStart(index, block),
        ] } },
        { emit: { lines: deltas(1, BEFORE), delayBetweenMs: 15 } },
        { waitSignal: { name: 'away' } },
        { emit: { lines: deltas(BEFORE + 1, AWAY), delayBetweenMs: 5 } },
        { waitSignal: { name: 'back' } },
        { emit: { lines: deltas(AWAY + 1, TOTAL), delayBetweenMs: 3 } },
        { emit: { lines: tail, delayBetweenMs: 15 } },
      ],
    }],
    afterTurns: 'silent',
  };
}

async function seedThreads(harness: HarnessApp): Promise<string> {
  const seed = await harness.rpc<{ projects: Array<{ threadIds: string[] }> }>('HarnessSeed', {
    projects: [{
      name: 'stream-switch-return',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
      threads: [
        { title: MAIN, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Main ready.' }] }] },
        { title: OTHER, provider: 'claude', turns: [{ userText: 'elsewhere', items: [{ kind: 'assistant_text', summary: 'Other ready.' }] }] },
      ],
    }],
  });
  return seed.projects[0].threadIds[0];
}

interface Sampler {
  frames: number;
  /** The first frames whose text skipped or reordered a token, by source. */
  gaps: Array<{ source: string; frame: number; after: number; next: number }>;
}

// Per animation frame: the collapsed or expanded thinking body and every
// reply body. Any rendered text whose numbered tokens are not consecutive
// is a dropped or misplaced word.
async function sampleTokens(page: Page, block: Block): Promise<void> {
  await page.evaluate((prefix) => {
    const state: Sampler = { frames: 0, gaps: [] };
    (window as unknown as { __aoTokenGaps: Sampler }).__aoTokenGaps = state;
    const pattern = new RegExp(`${prefix}(\\d{3})`, 'g');
    const check = (source: string, text: string) => {
      const numbers = [...text.matchAll(pattern)].map((match) => Number(match[1]));
      for (let i = 1; i < numbers.length; i++) {
        if (numbers[i] === numbers[i - 1]! + 1) continue;
        if (state.gaps.length < 20) state.gaps.push({ source, frame: state.frames, after: numbers[i - 1]!, next: numbers[i]! });
        return;
      }
    };
    const tick = () => {
      state.frames += 1;
      document.querySelectorAll('[data-testid="thinking-body"]').forEach((node) => check('thinking', node.textContent ?? ''));
      document.querySelectorAll('[data-testid="assistant-message-body"]').forEach((node) => check('reply', node.textContent ?? ''));
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, block === 'thinking' ? 'w' : 'p');
}

function readSampler(page: Page): Promise<Sampler> {
  return page.evaluate(() => (window as unknown as { __aoTokenGaps: Sampler }).__aoTokenGaps);
}

function numbered(block: Block, text: string): number[] {
  const pattern = new RegExp(`${block === 'thinking' ? 'w' : 'p'}(\\d{3})`, 'g');
  return [...text.matchAll(pattern)].map((match) => Number(match[1]));
}

function consecutive(from: number, to: number): number[] {
  const out: number[] = [];
  for (let n = from; n <= to; n++) out.push(n);
  return out;
}

// The block's rendered text once the turn is settled and revealed: the
// collapsed tail ends at the last token, and the expanded thinking body or
// the reply holds every token in order.
async function expectWholeStream(page: Page, block: Block, when: string): Promise<void> {
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect(timeline.getByText(REPLY_END)).toBeVisible();
  if (block === 'text') {
    const reply = timeline.getByTestId('assistant-message-body').filter({ hasText: REPLY_END });
    await expect(reply).toContainText(token('text', TOTAL).trim());
    expect(numbered('text', await reply.innerText()), `reply tokens ${when}`).toEqual(consecutive(1, TOTAL));
    return;
  }
  const run = timeline.getByTestId('activity-run').last();
  await expect(run).toHaveCount(1);
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const body = timeline.getByTestId('thinking-body');
  await expect(body).toContainText(token('thinking', TOTAL).trim());
  const tail = numbered('thinking', await body.innerText());
  expect(tail.at(-1), `collapsed tail end ${when}`).toBe(TOTAL);
  expect(tail, `collapsed tail tokens ${when}`).toEqual(consecutive(tail[0]!, TOTAL));
  await timeline.getByTestId('thinking-toggle').click();
  await expect(body).toContainText(token('thinking', 1).trim());
  expect(numbered('thinking', await body.innerText()), `expanded tokens ${when}`).toEqual(consecutive(1, TOTAL));
  await timeline.getByTestId('thinking-toggle').click();
}

// A reply that reaches the client whole in one burst and settles while its
// reveal still has most of the text to show.
function settledBurstScenario(): unknown {
  const reply = `${tokens('text', 1, TOTAL).join('')}${REPLY_END}`;
  return {
    version: 1,
    name: 'stream-switch-return-settled-burst',
    provider: 'claude',
    turns: [{
      label: 'burst',
      steps: [
        { emit: { lines: [
          j({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: { id: 'msg-burst', role: 'assistant' } } }),
          blockStart(0, 'text'),
          ...tokens('text', 1, TOTAL).map((text) => blockDelta(0, 'text', text)),
          blockDelta(0, 'text', REPLY_END),
          blockStop(0),
          j({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
          j({ type: 'assistant', message: { id: 'msg-burst', role: 'assistant', model: 'claude-mock-1', content: [{ type: 'text', text: reply }] } }),
          RESULT_LINE,
        ] } },
      ],
    }],
    afterTurns: 'silent',
  };
}

test('a reply left while its settled text still reveals is whole on return', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: settledBurstScenario() });
  const threadId = await seedThreads(harness);
  await harness.open(page);
  const pageErrors: string[] = [];
  page.on('pageerror', (err) => pageErrors.push(String(err)));
  const timeline = page.getByTestId('message-timeline-scroll');
  const open = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  await open(MAIN);
  await expect(timeline.getByText('Main ready.')).toBeVisible();
  await startMock(harness, threadId);

  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'stream something', null);
  await completed;
  const reply = timeline.getByTestId('assistant-message-body').filter({ hasText: token('text', 1).trim() });
  await expect(reply).toContainText(token('text', 20).trim());
  // The leave must land while the reveal is still behind the settled text.
  expect(await reply.innerText()).not.toContain(REPLY_END);

  await open(OTHER);
  await expect(timeline.getByText('Other ready.')).toBeVisible();
  await open(MAIN);
  await expectWholeStream(page, 'text', 'after leaving during the reveal');

  await open(OTHER);
  await expect(timeline.getByText('Other ready.')).toBeVisible();
  await open(MAIN);
  await expectWholeStream(page, 'text', 'after a second reopen');
  expect(pageErrors).toEqual([]);
});

for (const block of ['thinking', 'text'] as const) {
  test(`a ${block} stream left and returned to mid-stream keeps every word`, async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { scenario: scenario(block) });
    const threadId = await seedThreads(harness);
    await harness.open(page);
    const pageErrors: string[] = [];
    page.on('pageerror', (err) => pageErrors.push(String(err)));
    const timeline = page.getByTestId('message-timeline-scroll');
    const open = (title: string) => page.getByTestId('thread-row').getByText(title, { exact: true }).click();
    await open(MAIN);
    await expect(timeline.getByText('Main ready.')).toBeVisible();
    const mockId = await startMock(harness, threadId);
    await sampleTokens(page, block);

    const awayGate = waitForGate(harness, 'away');
    const completed = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('SendMessage', threadId, 'stream something', null);
    await awayGate;
    const firstPart = block === 'thinking'
      ? timeline.getByTestId('thinking-body')
      : timeline.getByTestId('assistant-message-body').filter({ hasText: token('text', 1).trim() });
    await expect(firstPart).toContainText(token(block, BEFORE).trim());

    // Leave while the stream is live; it continues with no pane watching.
    await open(OTHER);
    await expect(timeline.getByText('Other ready.')).toBeVisible();
    const backGate = waitForGate(harness, 'back');
    await advance(harness, mockId, 'away');
    await backGate;

    // Return while the stream resumes, so the read and the deltas race.
    await advance(harness, mockId, 'back');
    await open(MAIN);
    await completed;
    await expectWholeStream(page, block, 'after the return');

    const sampled = await readSampler(page);
    expect(sampled.frames).toBeGreaterThan(10);
    expect(sampled.gaps, 'frames with a skipped or reordered token').toEqual([]);

    // A reopen serves the pane's cached window, verified by the backend.
    await open(OTHER);
    await expect(timeline.getByText('Other ready.')).toBeVisible();
    await open(MAIN);
    await expectWholeStream(page, block, 'after a reopen');

    await page.reload();
    await open(MAIN);
    await expectWholeStream(page, block, 'after a reload');
    expect(pageErrors).toEqual([]);
  });
}
