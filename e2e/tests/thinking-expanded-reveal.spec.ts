// An expanded thinking row streams at the reveal's pace.
//
// A burst of reasoning lands faster than the reveal shows it, and the row is
// expanded while the reveal is still behind. Expanding loads the row's
// payload, and the backend writes its stream buffers before that read, so the
// loaded text leads the reveal. The expanded body must still end where the
// reveal is and grow with it; showing the loaded text at once lands the rest
// of the burst as one block.
//
// Every rendered frame is sampled while the row is expanded: the body's
// numbered tokens stay consecutive, and its last token advances by at most a
// few tokens per frame.
import { test, expect } from './fixtures.js';
import { RESULT_LINE, advance, seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

const TOKENS = 200;
// The reveal shows about 320 characters a second (PerItemSmoother's
// MAX_ADAPTIVE_CHARS_PER_SEC), about one five-character token per frame.
const MAX_TOKENS_PER_FRAME = 8;

function token(n: number): string {
  return `w${String(n).padStart(3, '0')} `;
}

function scenario(): unknown {
  const tokens = Array.from({ length: TOKENS }, (_, i) => token(i + 1));
  const thinking = tokens.join('');
  const stream = (data: unknown) => j({ type: 'stream_event', event: (data as { type: string }).type, data });
  return {
    version: 1,
    name: 'thinking-expanded-reveal',
    provider: 'claude',
    turns: [{
      label: 'burst',
      steps: [
        { emit: { lines: [
          stream({ type: 'message_start', message: { id: 'msg-burst', role: 'assistant' } }),
          stream({ type: 'content_block_start', index: 0, content_block: { type: 'thinking', thinking: '' } }),
        ] } },
        { emit: { lines: tokens.map((text) => stream({
          type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: text },
        })), delayBetweenMs: 2 } },
        { waitSignal: { name: 'hold' } },
        { emit: { lines: [
          stream({ type: 'content_block_stop', index: 0 }),
          stream({ type: 'content_block_start', index: 1, content_block: { type: 'text', text: '' } }),
          stream({ type: 'content_block_delta', index: 1, delta: { type: 'text_delta', text: 'Done thinking.' } }),
          stream({ type: 'content_block_stop', index: 1 }),
          stream({ type: 'message_stop' }),
          j({ type: 'assistant', message: { id: 'msg-burst', role: 'assistant', model: 'claude-mock-1', content: [
            { type: 'thinking', thinking }, { type: 'text', text: 'Done thinking.' },
          ] } }),
          RESULT_LINE,
        ], delayBetweenMs: 5 } },
      ],
    }],
    afterTurns: 'silent',
  };
}

interface Sampler {
  frames: number;
  /** Last token of the previous expanded frame, or 0. */
  last: number;
  maxStep: number;
  gaps: Array<{ frame: number; after: number; next: number }>;
}

test('an expanded thinking row streams at the reveal when its payload leads it', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: scenario() });
  const threadId = await seedAgentThread(harness, 'thinking-expanded-reveal', 'Expanded reveal');
  await harness.open(page);
  const timeline = page.getByTestId('message-timeline-scroll');
  await page.getByTestId('thread-row').getByText('Expanded reveal', { exact: true }).click();
  await expect(timeline.getByText('Ready.')).toBeVisible();
  const mockId = await startMock(harness, threadId);

  await page.evaluate(() => {
    const state: Sampler = { frames: 0, last: 0, maxStep: 0, gaps: [] };
    (window as unknown as { __aoExpanded: Sampler }).__aoExpanded = state;
    const tick = () => {
      state.frames += 1;
      const toggle = document.querySelector('[data-testid="thinking-toggle"]');
      const body = document.querySelector('[data-testid="thinking-body"]');
      if (toggle?.getAttribute('aria-expanded') === 'true' && body) {
        const numbers = [...(body.textContent ?? '').matchAll(/w(\d{3})/g)].map((match) => Number(match[1]));
        for (let i = 1; i < numbers.length; i++) {
          if (numbers[i] !== numbers[i - 1]! + 1 && state.gaps.length < 20) {
            state.gaps.push({ frame: state.frames, after: numbers[i - 1]!, next: numbers[i]! });
            break;
          }
        }
        const end = numbers.at(-1) ?? 0;
        if (state.last > 0) state.maxStep = Math.max(state.maxStep, end - state.last);
        state.last = end;
      } else {
        state.last = 0;
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });

  const held = waitForGate(harness, 'hold');
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'think hard', null);
  await held;
  // The whole burst is on the wire; the reveal is still near its start.
  const body = timeline.getByTestId('thinking-body');
  await expect(body).toContainText('w001');
  const toggle = timeline.getByTestId('thinking-toggle');
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  // Judged on the sampled frames, not the DOM: the body must reach the last
  // token through frames the sampler saw.
  const sample = () => page.evaluate(() => (window as unknown as { __aoExpanded: Sampler }).__aoExpanded);
  await expect.poll(async () => (await sample()).last, { timeout: 20_000 }).toBe(TOKENS);

  const sampled = await sample();
  expect(sampled.gaps, 'expanded frames with a skipped token').toEqual([]);
  expect(sampled.maxStep, 'most tokens the expanded body gained in one frame').toBeLessThanOrEqual(MAX_TOKENS_PER_FRAME);

  await advance(harness, mockId, 'hold');
  await completed;
  await expect(timeline.getByText('Done thinking.')).toBeVisible();
  const numbers = [...(await body.innerText()).matchAll(/w(\d{3})/g)].map((match) => Number(match[1]));
  expect(numbers).toEqual(Array.from({ length: TOKENS }, (_, i) => i + 1));
});
