// The background tray's live tail (docs/specs/agent-visibility.md,
// §Background tray). Expanding a running Claude background command's row
// shows the command and, under it, the tail of the output file the CLI
// writes for it (internal/tasktail, provider:background_task_output). The
// tail grows as the file does, follows its bottom until the reader scrolls
// up and follows again when they return, and is released on collapse. A
// task that ends while expanded shows its last bytes; once its completion
// lands, the timeline's card carries the whole output. Screenshots of each
// state in both themes are attached for the design pass.
import { test, expect } from './fixtures.js';
import type { Page, TestInfo } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  advance,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  itemMeta,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
  type ScenarioStep,
} from './agent-visibility-helpers.js';

const TASK = { task_id: 'task-bg', task_type: 'local_bash', description: 'Build in the background' };
const COMMAND = 'make -j8 build 2>&1 | tee build.log';
const OUTPUT_FILE = 'tasks/task-bg.output';

// Output the way a build prints it: a banner, a wide line, color.
const CHUNK_1 = [
  "\u001b[1mmake: Entering directory '/home/ao/repo'\u001b[0m",
  'go build -ldflags "-X main.version=0.9.0 -X main.commit=adbf6e4b3ead95fbe4150f514b45f0bc4d143289 -X main.date=2026-10-16T00:00:00Z -X main.builder=harness" -o bin/agent-overflow .',
  '\u001b[32m✓\u001b[0m compiled internal/tasktail (0.41s)',
  '\u001b[33mwarning:\u001b[0m frontend/src/lib/components/chat/CommandOutput.svelte: unused prop collapsedPreview',
  '',
].join('\n');
const CHUNK_2 = Array.from({ length: 60 }, (_, i) => {
  const pct = String(Math.round(((i + 1) / 60) * 100)).padStart(3, ' ');
  return `[${pct}%] Building CXX object src/CMakeFiles/engine.dir/module_${String(i + 1).padStart(2, '0')}.cpp.o`;
}).join('\n') + '\n';
const CHUNK_3 = 'Linking CXX executable bin/engine\n';
const CHUNK_4 = '\u001b[1;32mBuild succeeded\u001b[0m in 42.3s (0 errors, 1 warning)\n';

function scenario(name: string, afterLaunch: ScenarioStep[]): unknown {
  return claudeScenario(name, [
    // The CLI has the file open before it acks the launch.
    { writeFile: { path: OUTPUT_FILE, content: '' } },
    emit([
      ...textLines('msg-lead', 'Starting the build in the background.'),
      toolUseLine('msg-bg', 'tu-bg', 'Bash', { command: COMMAND, run_in_background: true, description: TASK.description }),
      taskStartedLine('task-bg', 'tu-bg', TASK.description, { taskType: 'local_bash' }),
      toolResultLine(
        'tu-bg',
        `Command running in background with ID: task-bg. Output is being written to: \${CWD}/${OUTPUT_FILE}. You will be notified when it completes.`,
        { toolUseResult: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'task-bg' } },
      ),
      backgroundTasksChangedLine([TASK]),
      RESULT_LINE,
    ]),
    ...afterLaunch,
  ]);
}

const completion: ScenarioStep[] = [
  emit([
    taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835322 }),
    taskNotificationLine('task-bg', 'tu-bg', `Background command "${COMMAND}" completed (exit code 0)`, {
      outputFile: `\${CWD}/${OUTPUT_FILE}`,
      uuid: 'done-bg',
    }),
    backgroundTasksChangedLine([]),
  ]),
];

async function openThreadAndStart(harness: HarnessApp, page: Page, title: string) {
  const threadId = await seedAgentThread(harness, `${title}-app`, title);
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText(title, { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'build it', null);
  await completed;
  await page.getByTestId('activity-rail-background-toggle').click();
  const body = page.getByTestId('activity-rail-background-body');
  const row = body.locator('[data-testid="background-task-tray-row"][data-row-id="tu-bg"]');
  await expect(row.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-status', 'running');
  // The ack's output file is on the launch row, which is what makes the
  // row's chevron live.
  const launch = (await listItems(harness, threadId)).find((item) => item.id === 'tu-bg');
  expect(launch?.meta ?? "").toMatch(/"output_file":"[^"]*\/tasks\/task-bg\.output"/);
  return { threadId, mockId, body, row };
}

/** Switches the page's color scheme and waits for the theme to follow it. */
async function useTheme(page: Page, theme: 'dark' | 'light'): Promise<void> {
  await page.emulateMedia({ colorScheme: theme });
  await expect(page.locator('html')).toHaveClass(new RegExp(`\\b${theme}\\b`));
  // The colour transitions the flip starts must land before a capture
  // (transitions only: the running indicators animate without end).
  await page.evaluate(() => Promise.all(
    document.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => undefined)),
  ));
}

/** One viewport screenshot per theme, named for the design pass. */
async function shoot(page: Page, testInfo: TestInfo, state: string): Promise<void> {
  for (const theme of ['dark', 'light'] as const) {
    await useTheme(page, theme);
    const path = testInfo.outputPath(`tail-${state}-${theme}.png`);
    await page.screenshot({ path });
    await testInfo.attach(`tail-${state}-${theme}`, { path, contentType: 'image/png' });
  }
  await useTheme(page, 'dark');
}

test('an expanded running command shows its live tail, follows the bottom, and yields to the final output', async ({ harness, page }, testInfo) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: scenario('background-task-live-tail', [
      { waitSignal: { name: 'grow-1' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_1, append: true } },
      { waitSignal: { name: 'grow-2' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_2, append: true } },
      { waitSignal: { name: 'grow-3' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_3, append: true } },
      { waitSignal: { name: 'finish' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_4, append: true } },
      ...completion,
    ]),
  });
  const { threadId, mockId, row } = await openThreadAndStart(harness, page, 'Live tail');
  await useTheme(page, 'dark');

  // Collapsed: the row's chevron is live because the launch names its file.
  const toggle = row.getByTestId('command-output-toggle');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await shoot(page, testInfo, 'collapsed');

  // Expanded with nothing written yet: the command, then the waiting line.
  // Expanding is what starts the follow; nothing was read before.
  const firstFrame = harness.waitForEvent<{ launchId: string; append?: string }>(
    'provider:background_task_output',
    (ev: any) => ev.launchId === 'tu-bg',
  );
  await toggle.click();
  const body = row.getByTestId('command-output-live-body');
  await expect(body.getByTestId('command-output-full-command')).toHaveText(COMMAND);
  const tail = body.getByTestId('background-task-live-tail');
  await expect(tail.getByTestId('background-task-live-tail-waiting')).toBeVisible();

  // The file grows: the tail shows the new lines, with their color, and
  // follows the bottom.
  await waitForGate(harness, 'grow-1');
  await advance(harness, mockId, 'grow-1');
  const frame = await firstFrame;
  expect(frame.append).toContain('compiled internal/tasktail');
  const scroll = tail.getByTestId('background-task-live-tail-scroll');
  await expect(scroll).toContainText('compiled internal/tasktail (0.41s)');
  await expect(scroll).toContainText('unused prop collapsedPreview');
  await expect(scroll.locator('.ansi-fg-32')).toHaveText('✓');
  await expect(scroll.locator('.ansi-fg-33')).toHaveText('warning:');
  await expect(tail.getByTestId('background-task-live-tail-waiting')).toHaveCount(0);
  await expect(tail).toHaveAttribute('data-following', 'true');

  await waitForGate(harness, 'grow-2');
  await advance(harness, mockId, 'grow-2');
  await expect(scroll).toContainText('[100%] Building CXX object');
  // Sixty more lines overflow the clip; a follower sits at its bottom.
  await expect.poll(() => scroll.evaluate((el: HTMLElement) => el.scrollHeight > el.clientHeight + 40)).toBe(true);
  await expect.poll(() => scroll.evaluate((el: HTMLElement) => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThan(4);
  // The clip is ten lines of the output tall, measured from the output's
  // own line height, plus the box's padding.
  const clip = await scroll.evaluate((el: HTMLElement) => {
    const box = getComputedStyle(el);
    const output = el.querySelector('.ansi-body');
    return {
      height: el.clientHeight,
      line: output ? parseFloat(getComputedStyle(output).lineHeight) : NaN,
      padding: parseFloat(box.paddingTop) + parseFloat(box.paddingBottom),
    };
  });
  expect(clip.line).toBeGreaterThan(0);
  expect(clip.height).toBeLessThanOrEqual(10 * clip.line + clip.padding + 1);
  expect(clip.height).toBeGreaterThan(9 * clip.line + clip.padding);
  await shoot(page, testInfo, 'expanded');
  // The list grew around the open body: the row's header and the whole
  // clip are in view without the list itself scrolling.
  await expect(body.getByTestId('command-output-full-command')).toBeInViewport();
  await expect(scroll).toBeInViewport({ ratio: 0.99 });

  // Scrolling up pauses the follow: the next growth does not move the
  // reader, and the lines still arrive.
  await scroll.hover();
  await page.mouse.wheel(0, -240);
  await expect(tail).toHaveAttribute('data-following', 'false');
  // The follow pauses on the wheel event, which can come before the scroll
  // it starts lands: the held position is read once the reader is up.
  await expect.poll(() => scroll.evaluate((el: HTMLElement) => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeGreaterThan(200);
  const heldTop = await scroll.evaluate((el: HTMLElement) => el.scrollTop);
  expect(heldTop).toBeGreaterThan(0);
  await waitForGate(harness, 'grow-3');
  await advance(harness, mockId, 'grow-3');
  await expect(scroll).toContainText('Linking CXX executable bin/engine');
  await expect(tail).toHaveAttribute('data-following', 'false');
  expect(Math.abs((await scroll.evaluate((el: HTMLElement) => el.scrollTop)) - heldTop)).toBeLessThan(4);
  await shoot(page, testInfo, 'scrolled');

  // Returning to the bottom resumes the follow.
  await page.mouse.wheel(0, 4000);
  await expect(tail).toHaveAttribute('data-following', 'true');
  await expect.poll(() => scroll.evaluate((el: HTMLElement) => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThan(4);

  // Collapsing releases the follow: no frame for the next growth reaches
  // this client, and nothing of the tail stays mounted.
  await toggle.click();
  await expect(row.getByTestId('command-output-live-body')).toHaveCount(0);
  const isTail = (ev: any) => ev.launchId === 'tu-bg';
  const framesBeforeCollapse = harness.countEvents('provider:background_task_output', isTail);
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  // The completion lands: the row leaves the tray, and the timeline's
  // card carries the whole file, first line to last.
  await expect(row).toHaveCount(0);
  await expect.poll(async () => (await listItems(harness, threadId)).find((item) => item.completionOf === 'tu-bg')?.status).toBe('completed');
  const sibling = (await listItems(harness, threadId)).find((item) => item.completionOf === 'tu-bg');
  expect(sibling?.payloadKind).toBe('command_output');
  // The completion sibling's card, headed by the command (the launch row
  // keeps its description and no output).
  const card = page.getByTestId('command-output-row').filter({ has: page.getByTestId('command-output-command').filter({ hasText: COMMAND }) });
  await expect(card).toHaveCount(1);
  await card.getByTestId('command-output-toggle').click();
  const output = card.locator('.ansi-body');
  await expect(output).toContainText("make: Entering directory '/home/ao/repo'");
  await expect(output).toContainText('Build succeeded');
  await expect(output).toContainText('(0 errors, 1 warning)');
  // The timeline settles at its bottom with the opened card before the capture.
  const timeline = page.getByTestId('message-timeline-scroll');
  await expect.poll(() => timeline.evaluate((el: HTMLElement) => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeLessThan(1);
  await shoot(page, testInfo, 'ended');
  expect(harness.countEvents('provider:background_task_output', isTail)).toBe(framesBeforeCollapse);
});

test('a task that ends while its tail is open shows its last line, then its card holds the output', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: scenario('background-task-live-tail-ends-open', [
      { waitSignal: { name: 'grow-1' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_1, append: true } },
      { waitSignal: { name: 'finish' } },
      { writeFile: { path: OUTPUT_FILE, content: CHUNK_4, append: true } },
      ...completion,
    ]),
  });
  const { threadId, mockId, row } = await openThreadAndStart(harness, page, 'Tail ends open');
  await row.getByTestId('command-output-toggle').click();
  const tail = row.getByTestId('background-task-live-tail');
  await waitForGate(harness, 'grow-1');
  await advance(harness, mockId, 'grow-1');
  await expect(tail).toContainText('compiled internal/tasktail');

  // The end's last read carries the final line before the ended frame.
  const ended = harness.waitForEvent<{ ended?: boolean; append?: string }>(
    'provider:background_task_output',
    (ev: any) => ev.launchId === 'tu-bg' && ev.ended === true,
  );
  const lastRead = harness.waitForEvent<{ append?: string }>(
    'provider:background_task_output',
    (ev: any) => ev.launchId === 'tu-bg' && typeof ev.append === 'string' && ev.append.includes('Build succeeded'),
  );
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await lastRead;
  await ended;
  await expect(row).toHaveCount(0);
  await expect.poll(async () => (await listItems(harness, threadId)).find((item) => item.completionOf === 'tu-bg')?.status).toBe('completed');
  const sibling = (await listItems(harness, threadId)).find((item) => item.completionOf === 'tu-bg');
  expect(sibling?.payloadKind).toBe('command_output');
  const card = page.getByTestId('command-output-row').filter({ has: page.getByTestId('command-output-command').filter({ hasText: COMMAND }) });
  await expect(card).toHaveCount(1);
  await card.getByTestId('command-output-toggle').click();
  await expect(card.locator('.ansi-body')).toContainText('Build succeeded');
});

const WATCH_COMMAND = 'until [ -f /tmp/gate.done ]; do sleep 5; done';
const WATCH_FILE = 'tasks/task-watch.output';

// A Monitor's launch ack names no file. Its first notification does, and
// from then on the tray row tails the file like a backgrounded Bash.
test("a Monitor's tray row tails its output once a notification names the file", async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('background-task-live-tail-monitor', [
      { writeFile: { path: WATCH_FILE, content: '' } },
      emit([
        ...textLines('msg-lead', 'Watching the gate.'),
        toolUseLine('msg-watch', 'tu-watch', 'Monitor', { command: WATCH_COMMAND, description: 'gate done', timeout_ms: 1800000 }),
        taskStartedLine('task-watch', 'tu-watch', 'gate done', { taskType: 'local_bash' }),
        toolResultLine(
          'tu-watch',
          'Monitor started (task task-watch, timeout 1800000ms). You will be notified on each event. Keep working — do not poll or sleep.',
          { toolUseResult: { taskId: 'task-watch', timeoutMs: 1800000 } },
        ),
        backgroundTasksChangedLine([{ task_id: 'task-watch', task_type: 'local_bash', description: 'gate done' }]),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'event-1' } },
      { writeFile: { path: WATCH_FILE, content: 'matched: gate opened\n', append: true } },
      emit([
        taskNotificationLine('task-watch', 'tu-watch', 'matched: gate opened', { outputFile: `\${CWD}/${WATCH_FILE}`, uuid: 'watch-1' }),
      ]),
      { waitSignal: { name: 'event-2' } },
      { writeFile: { path: WATCH_FILE, content: 'matched: gate closed\n', append: true } },
      { waitSignal: { name: 'finish' } },
      emit([
        taskUpdatedLine('task-watch', { status: 'completed', end_time: 1787419845000 }),
        taskNotificationLine('task-watch', 'tu-watch', 'Monitor timed out after 1800000ms', { uuid: 'watch-2' }),
        backgroundTasksChangedLine([]),
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'Monitor tail-app', 'Monitor tail');
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText('Monitor tail', { exact: true }).click();
  const mockId = await startMock(harness, threadId);
  const completed = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'watch the gate', null);
  await completed;
  await page.getByTestId('activity-rail-background-toggle').click();
  const row = page.getByTestId('activity-rail-background-body').locator('[data-testid="background-task-tray-row"][data-row-id="tu-watch"]');
  await expect(row.getByTestId('background-task-tray-row-status')).toHaveAttribute('data-status', 'running');
  // No file is known yet: the row is a command row with a dead chevron.
  await expect(row.getByTestId('command-output-command')).toHaveText('gate done');
  const toggle = row.getByTestId('command-output-toggle');
  await expect(toggle).toHaveAttribute('aria-disabled', 'true');

  // The notification names the file: the tray delta marks the row
  // tailable without writing the launch row, and the tail opens on it.
  await waitForGate(harness, 'event-1');
  await advance(harness, mockId, 'event-1');
  await expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
  const launch = (await listItems(harness, threadId)).find((item) => item.id === 'tu-watch');
  expect(launch?.meta ?? '').not.toContain('output_file');
  expect(launch?.meta ?? '').not.toContain('tail_available');
  await toggle.click();
  const scroll = row.getByTestId('background-task-live-tail-scroll');
  await expect(scroll).toContainText('matched: gate opened');

  // Later output follows without another notification.
  await waitForGate(harness, 'event-2');
  await advance(harness, mockId, 'event-2');
  await expect(scroll).toContainText('matched: gate closed');

  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await expect(row).toHaveCount(0);
});
