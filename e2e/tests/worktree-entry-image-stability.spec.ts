// A local image already on screen stays painted when the thread's agent
// enters a worktree mid-turn.
//
// Claude's EnterWorktree moves the session's working directory, and the
// thread's workspace path follows it (internal/app/app_worktree_follow.go).
// The image a reply wrote earlier is the same file at the same absolute
// path, so the move must not make it a different image: a refetch paints
// the "Loading image…" placeholder, the row loses the image's height, the
// bottom-pinned viewport clamps upward by that much, and the follow glides
// back when the bytes land.
//
// The turn is the one that showed it: reasoning, a narration block that is
// promoted out of its activity run, then the worktree entry behind it.
import { execFileSync } from 'node:child_process';
import { writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import { solidPng } from './attachment-fixture.js';
import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  claudeTurnsScenario,
  startMock,
  textLines,
  thinkingLines,
  toolResultLine,
  toolUseLine,
  type ScenarioStep,
} from './agent-visibility-helpers.js';

const TITLE = 'Worktree entry image stability';
const IMAGE_PX = 400;
const NARRATION =
  'I am going to enter a fresh worktree for this build, named something like read-aloud-paragraphs, ' +
  'and then look through the files and lay out how to split the work.';

const j = (value: unknown): string => JSON.stringify(value);

// A signature in the observed layout (docs/references/claude-wire.md): field
// 2 holds a header (field 1) whose field 8 is the tag. Built by hand.
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
const bytesField = (field: number, value: Buffer): Buffer =>
  Buffer.concat([varint((field << 3) | 2), varint(value.length), value]);
const intField = (field: number, value: number): Buffer => Buffer.concat([varint(field << 3), varint(value)]);
function signature(tag: string): string {
  const header = Buffer.concat([intField(1, 3), intField(2, 7), bytesField(8, Buffer.from(tag))]);
  const body = Buffer.concat([bytesField(1, header), bytesField(2, Buffer.from('opaque-two'))]);
  return Buffer.concat([bytesField(1, Buffer.from('opaque-top')), bytesField(2, body)]).toString('base64');
}

const stream = (data: { type: string } & Record<string, unknown>): string =>
  j({ type: 'stream_event', event: data.type, data });

function narrationLines(messageId: string, thinking: string): string[] {
  const sig = signature('narration');
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

interface Frame {
  top: number;
  height: number;
  client: number;
  image: number;
  loading: number;
}

async function installSampler(page: Page): Promise<void> {
  await page.evaluate(() => {
    const scroller = document.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]');
    if (!scroller) throw new Error('timeline scroller not found');
    const frames: Frame[] = [];
    (window as unknown as { __aoFrames: Frame[] }).__aoFrames = frames;
    const tick = (): void => {
      const img = scroller.querySelector<HTMLImageElement>('img[data-markdown-image-src]');
      frames.push({
        top: scroller.scrollTop,
        height: scroller.scrollHeight,
        client: scroller.clientHeight,
        image: img ? img.getBoundingClientRect().height : -1,
        loading: scroller.querySelectorAll('[data-streamdown-image-loading]').length,
      });
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
}

interface Summary {
  frames: number;
  /** Largest single-frame scrollTop decrease. */
  maxTopDrop: number;
  /** Largest scrollHeight drop below its running peak. */
  maxHeightDip: number;
  /** Largest distance from the bottom. */
  maxDistance: number;
  /** Smallest image height while one was mounted. */
  minImage: number;
  framesWithoutImage: number;
  framesLoading: number;
}

async function summarize(page: Page): Promise<Summary> {
  const frames = await page.evaluate(() => (window as unknown as { __aoFrames: Frame[] }).__aoFrames);
  const out: Summary = {
    frames: frames.length, maxTopDrop: 0, maxHeightDip: 0, maxDistance: 0,
    minImage: Number.POSITIVE_INFINITY, framesWithoutImage: 0, framesLoading: 0,
  };
  let peak = 0;
  let prevTop = frames[0]?.top ?? 0;
  for (const frame of frames) {
    out.maxTopDrop = Math.max(out.maxTopDrop, prevTop - frame.top);
    prevTop = frame.top;
    peak = Math.max(peak, frame.height);
    out.maxHeightDip = Math.max(out.maxHeightDip, peak - frame.height);
    out.maxDistance = Math.max(out.maxDistance, frame.height - frame.client - frame.top);
    if (frame.image < 0) out.framesWithoutImage += 1;
    else out.minImage = Math.min(out.minImage, frame.image);
    if (frame.loading > 0) out.framesLoading += 1;
  }
  return out;
}

test('a local image stays painted when the agent enters a worktree', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'worktree-entry-image',
      repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] },
      threads: [{
        title: TITLE,
        provider: 'claude',
        turns: [{
          userText: 'How does the review pane lay out its sections?',
          items: [{ kind: 'assistant_text', summary: 'The sections share one inset, so the margins cannot drift apart.' }],
          repeat: 12,
        }],
      }],
    }],
  });
  const project = seed.projects[0]!;
  const threadId = project.threadIds[0]!;
  const imagePath = join(project.path, 'overview-open.png');
  await writeFile(imagePath, solidPng(IMAGE_PX, IMAGE_PX, [40, 80, 160]));
  const worktreePath = join(project.path, '.claude', 'worktrees', 'read-aloud');
  execFileSync('git', ['-C', project.path, 'worktree', 'add', '-q', '-b', 'worktree-read-aloud', worktreePath]);

  const imageTurn: ScenarioStep[] = [{ emit: { lines: [
    ...textLines('msg-image', `Fresh screenshot:\n\n![overview](${imagePath})\n\nSections and slab share the same edges.`),
    RESULT_LINE,
  ], delayBetweenMs: 5 } }];
  const worktreeTurn: ScenarioStep[] = [
    { emit: { lines: [
      ...thinkingLines('msg-think', 'Reading the request again before starting.'),
      ...narrationLines('msg-narration', NARRATION),
      toolUseLine('msg-enter', 'toolu-enter', 'EnterWorktree', { name: 'read-aloud' }),
      toolResultLine('toolu-enter', `Created worktree at ${worktreePath} on branch worktree-read-aloud`, {
        toolUseResult: { worktreePath, worktreeBranch: 'worktree-read-aloud', message: 'Created worktree' },
      }),
      toolUseLine('msg-bash', 'toolu-bash', 'Bash', { command: 'ls frontend/src' }),
      toolResultLine('toolu-bash', 'lib\ntest'),
    ], delayBetweenMs: 8 } },
    { delayMs: 1500 },
    { emit: { lines: [...textLines('msg-done', 'Done with the build.'), RESULT_LINE], delayBetweenMs: 5 } },
  ];
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('worktree-entry-image', [imageTurn, worktreeTurn]),
  });

  await harness.open(page);
  const timeline = page.getByTestId('message-timeline-scroll');
  await page.getByTestId('thread-row').getByText(TITLE, { exact: true }).click();
  await startMock(harness, threadId);

  let completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'show me', null);
  await completed;
  const image = timeline.locator('img[data-markdown-image-src]');
  await expect(image).toHaveCount(1);
  await expect
    .poll(async () => await image.evaluate((img: HTMLImageElement) => img.complete && img.naturalHeight))
    .toBe(IMAGE_PX);
  // Settled at the bottom before anything is measured.
  await expect
    .poll(async () => await timeline.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop <= 1))
    .toBe(true);
  await page.waitForTimeout(500);

  await installSampler(page);
  completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'build it', null);
  await completed;
  await expect(timeline.getByText('Done with the build.')).toBeVisible({ timeout: 20_000 });
  await page.waitForTimeout(1500);

  // The turn did what it is here to do: the narration left its run as
  // prose, and the thread followed the agent into the worktree.
  await expect(timeline.getByTestId('assistant-message-summarized')).toHaveCount(1);
  const moved = await harness.rpc<{ workspacePath: string }>('GetThread', threadId);
  expect(moved.workspacePath).toContain(join('.claude', 'worktrees', 'read-aloud'));

  const summary = await summarize(page);
  expect(summary.framesLoading, 'frames showing the image loading placeholder').toBe(0);
  expect(summary.framesWithoutImage, 'frames with no image mounted').toBe(0);
  expect(summary.minImage, 'smallest image height').toBeGreaterThanOrEqual(IMAGE_PX - 1);
  expect(summary.maxHeightDip, 'largest scroll height dip').toBeLessThanOrEqual(8);
  expect(summary.maxTopDrop, 'largest single-frame upward move').toBeLessThanOrEqual(8);
});
