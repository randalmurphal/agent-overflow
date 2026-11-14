// A Claude narration block is shown as prose, not as thinking.
//
// Claude sends some of its between-step notes as a thinking block whose
// signature carries a `narration` tag (docs/references/claude-wire.md). The
// tag arrives when the block closes, so the block streams as a thinking row
// inside the activity run and then becomes a prose row:
//
//   - the row streams in the run, then a stage presents the change over
//     several frames, and only then the prose row stands on its own with a
//     "Summarized" tag,
//   - the tool call queued behind the block mounts only after that,
//   - a thinking block without the tag stays a thinking row,
//   - a reload shows the stored row as prose, with no presentation.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  claudeScenario,
  seedAgentThread,
  startMock,
  textLines,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';

const j = (value: unknown): string => JSON.stringify(value);

const NARRATION =
  'The migration applied cleanly and the first two checks pass. The third fails because the fixture still ' +
  'names the old column, so I will update the fixture and rerun only that check before touching anything else.';
const PLAIN_REASONING = 'Plain reasoning about the rerun stays a thinking row.';

// Signatures are built by hand in the observed layout: field 2 holds a
// header (field 1) whose field 8 is the tag. A signature from a real session
// is never committed.
function varint(value: number): Buffer {
  const out: number[] = [];
  let rest = value;
  while (rest >= 0x80) {
    out.push((rest & 0x7f) | 0x80);
    rest >>>= 7;
  }
  out.push(rest);
  return Buffer.from(out);
}

function bytesField(field: number, value: Buffer): Buffer {
  return Buffer.concat([varint((field << 3) | 2), varint(value.length), value]);
}

function intField(field: number, value: number): Buffer {
  return Buffer.concat([varint(field << 3), varint(value)]);
}

function signature(tag: string): string {
  const header = Buffer.concat([intField(1, 3), intField(2, 7), bytesField(8, Buffer.from(tag))]);
  const body = Buffer.concat([bytesField(1, header), bytesField(2, Buffer.from('opaque-two'))]);
  return Buffer.concat([bytesField(1, Buffer.from('opaque-top')), bytesField(2, body)]).toString('base64');
}

const stream = (data: { type: string } & Record<string, unknown>): string =>
  j({ type: 'stream_event', event: data.type, data });

/** A streamed thinking block that closes with `tag` in its signature. */
function taggedThinkingLines(messageId: string, thinking: string, tag: string): string[] {
  const sig = signature(tag);
  const words = thinking.match(/\S+\s*/g) ?? [thinking];
  return [
    stream({ type: 'message_start', message: { id: messageId, role: 'assistant' } }),
    stream({ type: 'content_block_start', index: 0, content_block: { type: 'thinking', thinking: '' } }),
    ...words.map((text) => stream({
      type: 'content_block_delta', index: 0, delta: { type: 'thinking_delta', thinking: text },
    })),
    stream({ type: 'content_block_delta', index: 0, delta: { type: 'signature_delta', signature: sig } }),
    stream({ type: 'content_block_stop', index: 0 }),
    stream({ type: 'message_stop' }),
    j({
      type: 'assistant',
      message: {
        id: messageId, role: 'assistant', model: 'claude-mock-1',
        content: [{ type: 'thinking', thinking, signature: sig }],
      },
    }),
  ];
}

function scenario(): unknown {
  return claudeScenario('narration-promotion', [
    { emit: { lines: [
      toolUseLine('msg-tool-1', 'toolu-1', 'Bash', { command: 'go test ./internal/store' }),
      toolResultLine('toolu-1', 'ok'),
    ], delayBetweenMs: 5 } },
    // The narration block, with the next tool call right behind its stop.
    { emit: { lines: [
      ...taggedThinkingLines('msg-narration', NARRATION, 'narration'),
      toolUseLine('msg-tool-2', 'toolu-2', 'Bash', { command: 'echo queued-behind-the-block' }),
      toolResultLine('toolu-2', 'queued-behind-the-block'),
    ], delayBetweenMs: 8 } },
    { emit: { lines: [
      ...taggedThinkingLines('msg-thinking', PLAIN_REASONING, 'thinking'),
      ...textLines('msg-final', 'All done.'),
      RESULT_LINE,
    ], delayBetweenMs: 5 } },
  ]);
}

interface Sample {
  frames: number;
  /** Frames the narration was a thinking row inside an activity run, no stage. */
  thinkingFrames: number;
  stageFrames: number;
  proseFrames: number;
  /** The queued tool call was mounted before the prose row stood alone. */
  queuedEarly: boolean;
  /** A stage frame followed a prose frame: the change ran backwards. */
  regressed: boolean;
  stageHeights: number[];
}

async function installSampler(page: import('@playwright/test').Page): Promise<void> {
  await page.evaluate((needle) => {
    const state: Sample = {
      frames: 0, thinkingFrames: 0, stageFrames: 0, proseFrames: 0,
      queuedEarly: false, regressed: false, stageHeights: [],
    };
    (window as unknown as { __aoNarration: Sample }).__aoNarration = state;
    const tick = () => {
      state.frames += 1;
      const scroll = document.querySelector('[data-testid="message-timeline-scroll"]');
      const stage = scroll?.querySelector('[data-testid="row-promotion-stage"]') ?? null;
      const inRun = [...(scroll?.querySelectorAll('[data-testid="activity-run"] [data-item-id]') ?? [])]
        .some((el) => (el.textContent ?? '').includes(needle));
      const prose = [...(scroll?.querySelectorAll('[data-testid="assistant-message-body"]') ?? [])]
        .some((el) => (el.textContent ?? '').includes(needle) && el.closest('[data-testid="row-promotion-stage"]') === null);
      const queued = (scroll?.textContent ?? '').includes('queued-behind-the-block');
      if (stage) {
        state.stageFrames += 1;
        state.stageHeights.push(stage.getBoundingClientRect().height);
        if (state.proseFrames > 0) state.regressed = true;
      } else if (prose) {
        state.proseFrames += 1;
      } else if (inRun) {
        state.thinkingFrames += 1;
      }
      if (queued && !prose) state.queuedEarly = true;
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }, NARRATION.slice(0, 40));
}

const sample = (page: import('@playwright/test').Page): Promise<Sample> =>
  page.evaluate(() => (window as unknown as { __aoNarration: Sample }).__aoNarration);

test('a narration block streams in the run, then stands as summarized prose', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: scenario() });
  const threadId = await seedAgentThread(harness, 'narration-promotion', 'Narration promotion');
  await harness.open(page);
  const timeline = page.getByTestId('message-timeline-scroll');
  await page.getByTestId('thread-row').getByText('Narration promotion', { exact: true }).click();
  await expect(timeline.getByText('Ready.')).toBeVisible();
  await startMock(harness, threadId);
  await installSampler(page);

  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'run the checks', null);
  await completed;
  await expect(timeline.getByText('All done.')).toBeVisible({ timeout: 20_000 });

  // The row on its own, as prose, tagged.
  const prose = timeline.locator('[data-item-id]').filter({ hasText: NARRATION.slice(0, 40) });
  await expect(prose).toHaveCount(1);
  await expect(prose).toContainText(NARRATION);
  await expect(prose.getByTestId('assistant-message-summarized')).toHaveText('Summarized');
  await expect(prose.locator('[data-item-kind="assistant_text"]')).toHaveCount(1);
  expect(await prose.evaluate((el) => el.closest('[data-testid="activity-run"]') === null)).toBe(true);
  await expect(timeline.getByTestId('row-promotion-stage')).toHaveCount(0);

  // It split the activity: a run before it and a run after it.
  const runs = timeline.getByTestId('activity-run');
  await expect(runs).toHaveCount(2);
  const order = await timeline.evaluate((scroll, needle) => {
    const rows = [...scroll.querySelectorAll('[data-row-index]')];
    const index = (predicate: (row: Element) => boolean) => rows.findIndex(predicate);
    return {
      firstRun: index((row) => row.querySelector('[data-testid="activity-run"]') !== null),
      prose: index((row) => row.querySelector('[data-testid="activity-run"]') === null
        && (row.textContent ?? '').includes(needle)),
      lastRun: rows.length - 1 - [...rows].reverse()
        .findIndex((row) => row.querySelector('[data-testid="activity-run"]') !== null),
    };
  }, NARRATION.slice(0, 40));
  expect(order.firstRun).toBeGreaterThanOrEqual(0);
  expect(order.prose).toBe(order.firstRun + 1);
  expect(order.lastRun).toBe(order.prose + 1);

  // The store holds the reclassified row, and the untagged block as thinking.
  const stored = await harness.rpc<Array<{ kind: string; summary: string; meta?: string }>>(
    'ListItems', threadId, true,
  );
  const narration = stored.find((item) => item.summary.includes(NARRATION.slice(0, 40)));
  expect(narration?.kind).toBe('assistant_text');
  expect(narration?.summary).toBe(NARRATION);
  expect(JSON.parse(narration?.meta ?? '{}').narration).toBe(true);
  const plain = stored.find((item) => item.summary.includes('Plain reasoning'));
  expect(plain?.kind).toBe('thinking');
  // On screen the untagged block is a row of the later run, not prose.
  expect(await runs.nth(1).evaluate(
    (run, needle) => [...run.querySelectorAll('[data-item-id]')].some((el) => (el.textContent ?? '').includes(needle)),
    'Plain reasoning',
  )).toBe(true);

  // How it got there, frame by frame.
  const sampled = await sample(page);
  expect(sampled.thinkingFrames, 'frames the block streamed inside the run').toBeGreaterThan(5);
  expect(sampled.stageFrames, 'frames the promotion was presented').toBeGreaterThanOrEqual(10);
  expect(sampled.queuedEarly, 'the queued tool call mounted before the prose row').toBe(false);
  expect(sampled.regressed, 'a stage after the prose row').toBe(false);
  for (let i = 1; i < sampled.stageHeights.length; i += 1) {
    expect(sampled.stageHeights[i]! - sampled.stageHeights[i - 1]!).toBeGreaterThanOrEqual(-0.01);
    expect(sampled.stageHeights[i]! - sampled.stageHeights[i - 1]!).toBeLessThanOrEqual(28);
  }

  // The store holds prose: a reload shows it at once, with no presentation.
  await page.reload();
  await page.getByTestId('thread-row').getByText('Narration promotion', { exact: true }).click();
  await installSampler(page);
  const restored = page.getByTestId('message-timeline-scroll').locator('[data-item-id]')
    .filter({ hasText: NARRATION.slice(0, 40) });
  await expect(restored).toHaveCount(1);
  await expect(restored.getByTestId('assistant-message-summarized')).toHaveText('Summarized');
  expect(await restored.evaluate((el) => el.closest('[data-testid="activity-run"]') === null)).toBe(true);
  await expect(page.getByTestId('message-timeline-scroll').getByText('All done.')).toBeVisible();
  const afterReload = await sample(page);
  expect(afterReload.stageFrames).toBe(0);
  expect(afterReload.thinkingFrames).toBe(0);
});
