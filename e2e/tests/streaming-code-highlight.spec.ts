// Streaming code blocks are colored by the backend's `highlight:live`
// pushes, and a line that has color never goes plain while the block
// streams.
//
// The unit suites pin the store's delta splicing and the host's cover rule
// in isolation. Only this level runs the assembled path: the router's text
// observer, the incremental parse, the transport's scope filter, the
// store's reveal drain and the host painting each animation frame. The
// sampler reads the rendered DOM every frame, which is what the user sees.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { RESULT_LINE, claudeScenario, seedAgentThread, startMock } from './agent-visibility-helpers.js';
import { readWire, recordWire } from './transport-watch-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

// app.HighlightCode's method id: the per-block highlight request the live
// pushes replace.
const HIGHLIGHT_CODE_METHOD_ID = 4080150350;

const LINES = 80;

// Every line carries a number and a comment, so every complete line has
// color. Each line is split mid-token, so the host and the pushes both see
// partial lines.
function fenceChunks(): string[] {
  const chunks = ['Here is the module:\n\n```python\n'];
  for (let i = 0; i < LINES; i++) {
    chunks.push(`value_${i} = ${i} `, `* 2  # line ${i}\n`);
  }
  chunks.push('```\n\nDone.\n');
  return chunks;
}

function streamedTextLines(messageId: string, chunks: string[]): string[] {
  const full = chunks.join('');
  return [
    j({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: { id: messageId, role: 'assistant' } } }),
    j({ type: 'stream_event', event: 'content_block_start', data: { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } } }),
    ...chunks.map((text) =>
      j({ type: 'stream_event', event: 'content_block_delta', data: { type: 'content_block_delta', delta: { type: 'text_delta', text } } }),
    ),
    j({ type: 'stream_event', event: 'content_block_stop', data: { type: 'content_block_stop', index: 0 } }),
    j({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
    j({ type: 'assistant', message: { id: messageId, role: 'assistant', model: 'claude-mock-1', content: [{ type: 'text', text: full }] } }),
  ];
}

interface Sampler {
  frames: number;
  /** Line indexes seen colored in one frame and fully plain in a later one. */
  lost: Array<{ frame: number; line: number; text: string }>;
  /** Most lines the block rendered in any frame. */
  maxLines: number;
  /** The latest frame's non-empty lines: how many, and how many had color. */
  lastLines: number;
  lastColored: number;
}

// Per animation frame, for the first code block in the timeline: which
// lines have a syntax span. A line colored once and plain later is the
// flicker this spec forbids. Keyed by line position, so a host remount
// that paints plain before its spans arrive counts too.
async function sampleCodeColors(page: Page): Promise<void> {
  await page.evaluate(() => {
    const state: Sampler = { frames: 0, lost: [], maxLines: 0, lastLines: 0, lastColored: 0 };
    const colored = new Set<number>();
    (window as unknown as { __aoCodeColors: Sampler }).__aoCodeColors = state;
    const tick = () => {
      const code = document.querySelector('[data-testid="message-timeline-scroll"] .streamdown-code-host code');
      if (code) {
        state.frames += 1;
        const lines: Array<{ text: string; colored: boolean }> = [{ text: '', colored: false }];
        const walk = (node: Node) => {
          if (node.nodeType === Node.TEXT_NODE) {
            const parts = (node.textContent ?? '').split('\n');
            parts.forEach((part, index) => {
              if (index > 0) lines.push({ text: '', colored: false });
              lines[lines.length - 1]!.text += part;
            });
            return;
          }
          if (node instanceof Element && [...node.classList].some((name) => name.startsWith('syntax-'))) {
            lines[lines.length - 1]!.colored = true;
          }
          node.childNodes.forEach(walk);
        };
        code.childNodes.forEach(walk);
        state.maxLines = Math.max(state.maxLines, lines.length);
        const written = lines.filter((line) => line.text.trim() !== '');
        state.lastLines = written.length;
        state.lastColored = written.filter((line) => line.colored).length;
        lines.forEach((line, index) => {
          if (line.colored) colored.add(index);
          else if (colored.has(index) && line.text.trim() !== '') {
            state.lost.push({ frame: state.frames, line: index, text: line.text });
          }
        });
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
}

function readSampler(page: Page): Promise<Sampler> {
  return page.evaluate(() => (window as unknown as { __aoCodeColors: Sampler }).__aoCodeColors);
}

test('a streaming code block is colored by live pushes and no colored line goes plain', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('streaming-code-highlight', [
      { emit: { lines: [...streamedTextLines('msg-code', fenceChunks()), RESULT_LINE], delayBetweenMs: 12 } },
    ]),
  });
  const threadId = await seedAgentThread(harness, 'streaming-code-app', 'Code stream');
  await recordWire(page);
  await harness.open(page);
  const pageErrors: string[] = [];
  page.on('pageerror', (err) => pageErrors.push(String(err)));
  await page.getByText('Code stream').click();
  await startMock(harness, threadId);
  await sampleCodeColors(page);

  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'write the module', null);
  await completed;

  const block = page.getByTestId('message-timeline-scroll').locator('.streamdown-code-host code').first();
  await expect(block).toContainText(`value_${LINES - 1} = ${LINES - 1} * 2  # line ${LINES - 1}`);
  // The settled block ends fully colored.
  await expect
    .poll(async () => {
      const { lastLines, lastColored } = await readSampler(page);
      return { lastLines, lastColored };
    })
    .toEqual({ lastLines: LINES, lastColored: LINES });

  const sampler = await readSampler(page);
  expect(sampler.maxLines, 'the sampler saw the block grow').toBeGreaterThanOrEqual(LINES);
  expect(sampler.frames).toBeGreaterThan(10);
  expect(sampler.lost, 'lines that lost their color while streaming').toEqual([]);

  const wire = await readWire(page);
  const live = wire.received.filter((event) => event.channel === 'highlight:live' && event.threadId === threadId);
  expect(live.length, 'highlight:live pushes for the thread').toBeGreaterThan(1);
  const highlightCalls = wire.sent.filter((frame) => {
    if (frame.type !== 'rpc') return false;
    return (JSON.parse(frame.text) as { methodId?: number }).methodId === HIGHLIGHT_CODE_METHOD_ID;
  });
  expect(highlightCalls.length, 'HighlightCode requests while the pushes covered the block').toBe(0);
  expect(pageErrors).toEqual([]);
});
