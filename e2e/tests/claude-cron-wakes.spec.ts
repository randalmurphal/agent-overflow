// Claude turns the CLI starts on its own (claude-wire.md §E7 and §E11),
// replayed from the 2.1.284 cron captures in
// docs/references/fixtures/claude/ through the mock provider:
//
//   - A session holding an armed recurring `CronCreate` job outlives the
//     idle-reap threshold, the job's fire still reaches it, and
//     `CronDelete` releases it so the next sweep reaps it. The reaper runs
//     on a shortened clock (HARNESS_TIMING idle-reap / idle-reap-sweep).
//   - Each fire renders as a scheduled turn: the injected prompt as a user
//     row badged "Scheduled", never as a message from another Claude
//     session, including a fire the CLI held through a busy turn.
//   - A background completion and a Monitor event that wake an idle
//     session render a badged row naming the task and its result at the
//     start of the woken turn.

import { readFileSync } from 'node:fs';
import path from 'node:path';

import type { Page, TestInfo } from '@playwright/test';
import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeTurnsScenario,
  emit,
  emitBurst,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const FIXTURES = path.resolve(import.meta.dirname, '../../docs/references/fixtures/claude');
const CRON_CAPTURE = 'cron_create_fire_20261010.ndjson';
const LOOP_CAPTURE = 'cron_loop_midturn_20261010.ndjson';
const CRON_PROMPT = 'Reply with exactly the text CRON-FIRED and call no tools.';
const LOOP_PROMPT = 'Reply with exactly the text LOOP-PING and call no tools.';
// The CronCreate turn's own reply, in both captures.
const SCHEDULED_REPLY = /^Scheduled job /;

/**
 * Lines `from`..`to` (1-based, inclusive) of a capture. The mock owns the
 * session id, so the capture's synthetic one becomes the mock's own.
 */
function capture(file: string, from: number, to: number): string[] {
  const lines = readFileSync(path.join(FIXTURES, file), 'utf8').split('\n');
  return lines
    .slice(from - 1, to)
    .filter((line) => line.trim() !== '')
    .map((line) => line.replace(/"session_id":"00000000-0000-4000-8000-00000000c20[12]"/g, '"session_id":"${SESSION_ID}"'));
}

async function withHarness(
  page: Page,
  timing: string,
  run: (harness: HarnessApp) => Promise<void>,
): Promise<void> {
  const harness = await launchHarness({ env: { AO_HARNESS_TIMING: timing } });
  try {
    await run(harness);
  } finally {
    await page.close();
    await harness.close();
  }
}

async function openThread(harness: HarnessApp, page: Page, title: string): Promise<void> {
  await harness.open(page);
  await page.getByTestId('thread-row-title').getByText(title, { exact: true }).click();
  await expect(page.getByLabel('Message Input')).toBeEnabled();
}

/** The timeline row of the user message carrying `text`. */
function userRow(page: Page, text: string) {
  return page.locator('[data-item-id][data-item-kind="user_text"]').filter({ hasText: text });
}

/** The assistant reply with a paragraph that is exactly `text`, or matches it. */
function replyRow(page: Page, text: string | RegExp) {
  const paragraph = typeof text === 'string' ? page.getByText(text, { exact: true }) : page.getByText(text);
  return page.locator('[data-item-id][data-item-kind="assistant_text"]').filter({ has: paragraph });
}

async function shot(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  await page.screenshot({ path: testInfo.outputPath(name) });
}

async function expectAbove(upper: ReturnType<Page['locator']>, lower: ReturnType<Page['locator']>): Promise<void> {
  const [a, b] = [await upper.boundingBox(), await lower.boundingBox()];
  expect(a, 'upper row is laid out').not.toBeNull();
  expect(b, 'lower row is laid out').not.toBeNull();
  expect(a!.y).toBeLessThan(b!.y);
}

test('an armed recurring cron job keeps its session alive until CronDelete releases it', async ({ page }) => {
  test.setTimeout(120_000);
  // The session is a reap candidate after 3s idle; the sweep runs twice a
  // second. Every assertion below is about the cron record, not the cadence.
  await withHarness(page, `${HARNESS_TIMING},idle-reap=3s,idle-reap-sweep=500ms`, async (harness) => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('cron-reap', [
        [
          // CronCreate "* * * * *" and the turn's result.
          emitBurst(capture(CRON_CAPTURE, 7, 57)),
          { waitSignal: { name: 'fire' } },
          // One idle fire: bracket, mirror row, reply, result.
          emit(capture(CRON_CAPTURE, 59, 72)),
        ],
        // CronList, a one-shot CronCreate, a CronDelete of an unknown id,
        // both real CronDeletes and an empty CronList.
        [emitBurst(capture(CRON_CAPTURE, 106, 212))],
      ]),
    });
    const threadId = await seedAgentThread(harness, 'cron-reap', 'Cron reap');
    await openThread(harness, page, 'Cron reap');
    const mockId = await startMock(harness, threadId);
    const exited = () =>
      harness.countEvents<{ mockId: string; report: { kind: string } }>(
        'harness:mock',
        (ev) => ev.mockId === mockId && ev.report.kind === 'exiting',
      );

    await harness.rpc('SendMessage', threadId, 'Schedule the cron job.', null);
    await expect(replyRow(page, SCHEDULED_REPLY)).toBeVisible();
    await waitForGate(harness, 'fire');

    // Several sweeps past the idle threshold with the job armed.
    await page.waitForTimeout(10_000);
    expect(exited(), 'the session holding an armed recurring job was reaped').toBe(0);

    // The fire reaches the live session.
    await advance(harness, mockId, 'fire');
    await expect(replyRow(page, 'CRON-FIRED')).toBeVisible();
    await page.waitForTimeout(5_000);
    expect(exited(), 'the session was reaped after its job fired').toBe(0);

    // CronDelete releases the job; the next sweeps reap the idle session.
    await harness.rpc('SendMessage', threadId, 'List and delete the jobs.', null);
    await expect(replyRow(page, 'DONE')).toBeVisible();
    await expect.poll(exited, { timeout: 10_000 }).toBe(1);
  });
});

test('a cron fire renders as a scheduled turn with its prompt', async ({ page }, testInfo) => {
  await withHarness(page, HARNESS_TIMING, async (harness) => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('cron-fire', [
        [
          emitBurst(capture(CRON_CAPTURE, 7, 57)),
          { waitSignal: { name: 'fires' } },
          // Three idle fires, each its own bracket.
          emit(capture(CRON_CAPTURE, 59, 100)),
        ],
      ]),
    });
    const threadId = await seedAgentThread(harness, 'cron-fire', 'Cron fire');
    await openThread(harness, page, 'Cron fire');
    const mockId = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'Schedule the cron job.', null);
    await expect(replyRow(page, SCHEDULED_REPLY)).toBeVisible();
    await waitForGate(harness, 'fires');
    await advance(harness, mockId, 'fires');
    await expect(replyRow(page, 'CRON-FIRED')).toHaveCount(3);
    await shot(page, testInfo, 'cron-fire-turn.png');

    const fires = userRow(page, CRON_PROMPT);
    await expect(fires).toHaveCount(3);
    await expect(fires.getByTestId('user-message-scheduled-origin')).toHaveCount(3);
    await expect(fires.first().getByTestId('user-message-scheduled-origin')).toHaveText('Scheduled');
    await expect(page.getByTestId('user-message-peer-origin')).toHaveCount(0);
    await expectAbove(fires.first(), replyRow(page, 'CRON-FIRED').first());
  });
});

test('a cron fire held through a busy turn renders after that turn as a scheduled turn', async ({ page }, testInfo) => {
  await withHarness(page, HARNESS_TIMING, async (harness) => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('cron-held', [
        // /loop 1m: CronCreate "*/1 * * * *".
        [emitBurst(capture(LOOP_CAPTURE, 7, 96))],
        // The 80s foreground Bash turn, then the fire it held.
        [emitBurst(capture(LOOP_CAPTURE, 103, 137)), emit(capture(LOOP_CAPTURE, 139, 152))],
      ]),
    });
    const threadId = await seedAgentThread(harness, 'cron-held', 'Cron held');
    await openThread(harness, page, 'Cron held');
    await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'Loop every minute.', null);
    await expect(replyRow(page, SCHEDULED_REPLY)).toBeVisible();
    // `/loop` runs its prompt once right away, so the reply above already
    // ends in LOOP-PING; the fire's reply is the second one.
    const pings = replyRow(page, 'LOOP-PING');
    await expect(pings).toHaveCount(1);
    await harness.rpc('SendMessage', threadId, 'Sleep for 80 seconds.', null);
    await expect(pings).toHaveCount(2);
    await pings.last().scrollIntoViewIfNeeded();
    await shot(page, testInfo, 'cron-held-fire.png');

    const fire = userRow(page, LOOP_PROMPT);
    await expect(fire.getByTestId('user-message-scheduled-origin')).toHaveCount(1);
    await expect(page.getByTestId('user-message-peer-origin')).toHaveCount(0);
    await expectAbove(replyRow(page, 'SLEPT'), fire);
    await expectAbove(fire, pings.last());
  });
});

// The idle wakes below follow the shapes AO's provider-event logs recorded
// on 2.1.280 and 2.1.284 (claude-wire.md §E7): a terminal completion sends
// task_updated and task_notification before the wake, a Monitor event sends
// neither, and both start the woken turn with system/init and a
// main-transcript mirror row (`turnOrigin:"task_notification"`) carrying the
// `<task-notification>` block. Ids and text are synthetic.
const BUILD_TASK = 'bwake0001';
const BUILD_TOOL = 'toolu_wake_build';
const BUILD_DESC = 'Run the release build';
const WATCH_TASK = 'bwatch002';
const WATCH_TOOL = 'toolu_wake_watch';
const WATCH_DESC = 'deploy log errors';
const ARMED_REPLY = 'The build runs in the background and the deploy log is watched.';
const WATCH_REPLY = 'Deploy step 3 is failing to connect.';

function j(value: unknown): string {
  return JSON.stringify(value);
}

function initLine(): string {
  return j({ type: 'system', subtype: 'init', session_id: '${SESSION_ID}', model: 'claude-mock-1', cwd: '${CWD}', tools: ['Bash', 'Monitor'] });
}

function wakeMirror(uuid: string, body: string): string {
  return j({
    type: 'transcript_mirror',
    filePath: '/home/user/.claude/projects/-home-user-project/${SESSION_ID}.jsonl',
    entries: [
      { type: 'queue-operation', operation: 'enqueue', timestamp: '2026-10-10T20:00:00.000Z' },
      { type: 'queue-operation', operation: 'dequeue', timestamp: '2026-10-10T20:00:00.001Z' },
      {
        parentUuid: null,
        isSidechain: false,
        type: 'user',
        message: { role: 'user', content: body },
        uuid,
        timestamp: '2026-10-10T20:00:00.002Z',
        origin: { kind: 'task-notification', producer: 'session-task' },
        promptSource: 'system',
        turnOrigin: 'task_notification',
        queueSkipAttachments: true,
        userType: 'external',
      },
    ],
  });
}

test('a background completion and a Monitor event that wake an idle session say what woke it', async ({ page }, testInfo) => {
  const buildSummary = `Background command "${BUILD_DESC}" completed (exit code 0)`;
  const watchEvent = 'ERROR deploy step 3 failed: connection refused';
  await withHarness(page, HARNESS_TIMING, async (harness) => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('idle-wake', [
        [
          emit([
            toolUseLine('msg-launch-build', BUILD_TOOL, 'Bash', { command: 'make release', description: BUILD_DESC, run_in_background: true }),
            taskStartedLine(BUILD_TASK, BUILD_TOOL, BUILD_DESC, { taskType: 'local_bash' }),
            toolResultLine(BUILD_TOOL, `Command running in background with ID: ${BUILD_TASK}.`, {
              toolUseResult: { stdout: '', stderr: '', interrupted: false, isImage: false, noOutputExpected: false, backgroundTaskId: BUILD_TASK },
            }),
            toolUseLine('msg-launch-watch', WATCH_TOOL, 'Monitor', { command: 'tail -F deploy.log | grep ERROR', description: WATCH_DESC, persistent: true }),
            taskStartedLine(WATCH_TASK, WATCH_TOOL, WATCH_DESC, { taskType: 'local_bash' }),
            toolResultLine(WATCH_TOOL, `Monitor started (task ${WATCH_TASK}, persistent).`, {
              toolUseResult: { taskId: WATCH_TASK, timeoutMs: 0, persistent: true },
            }),
            ...textLines('msg-armed', ARMED_REPLY),
            RESULT_LINE,
          ]),
          { waitSignal: { name: 'build-done' } },
          emit([
            taskUpdatedLine(BUILD_TASK, { status: 'completed', end_time: 1791143870401 }),
            taskNotificationLine(BUILD_TASK, BUILD_TOOL, buildSummary),
            initLine(),
            j({ type: 'system', subtype: 'status', status: 'requesting' }),
            wakeMirror('wake-build-0001', [
              '<task-notification>',
              `<task-id>${BUILD_TASK}</task-id>`,
              `<tool-use-id>${BUILD_TOOL}</tool-use-id>`,
              '<output-file>/tmp/tasks/bwake0001.output</output-file>',
              '<status>completed</status>',
              `<summary>${buildSummary}</summary>`,
              '</task-notification>',
            ].join('\n')),
            ...textLines('msg-after-build', 'The release build finished cleanly.'),
            RESULT_LINE,
          ]),
          { waitSignal: { name: 'watch-event' } },
          emit([
            initLine(),
            j({ type: 'system', subtype: 'status', status: 'requesting' }),
            wakeMirror('wake-watch-0002', [
              '<task-notification>',
              `<task-id>${WATCH_TASK}</task-id>`,
              `<summary>Monitor event: "${WATCH_DESC}"</summary>`,
              `<event>${watchEvent}</event>`,
              '</task-notification>',
            ].join('\n')),
            ...textLines('msg-after-watch', WATCH_REPLY),
            RESULT_LINE,
          ]),
        ],
      ]),
    });
    const threadId = await seedAgentThread(harness, 'idle-wake', 'Idle wake');
    await openThread(harness, page, 'Idle wake');
    const mockId = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'Build the release and watch the deploy.', null);
    await expect(replyRow(page, ARMED_REPLY)).toBeVisible();

    await waitForGate(harness, 'build-done');
    await advance(harness, mockId, 'build-done');
    await expect(replyRow(page, 'The release build finished cleanly.')).toBeVisible();
    await waitForGate(harness, 'watch-event');
    await advance(harness, mockId, 'watch-event');
    await expect(replyRow(page, WATCH_REPLY)).toBeVisible();
    await shot(page, testInfo, 'e7-idle-wake.png');

    const wakes = page.locator('[data-testid="notification-row"][data-notification-kind="background_wake"]');
    await expect(wakes).toHaveCount(2);
    const buildWake = wakes.filter({ hasText: BUILD_DESC });
    await expect(buildWake).toContainText(buildSummary);
    await expectAbove(replyRow(page, ARMED_REPLY), buildWake);
    await expectAbove(buildWake, replyRow(page, 'The release build finished cleanly.'));
    const watchWake = wakes.filter({ hasText: WATCH_DESC });
    await expect(watchWake).toContainText(watchEvent);
    await expectAbove(replyRow(page, 'The release build finished cleanly.'), watchWake);
    await expectAbove(watchWake, replyRow(page, WATCH_REPLY));
  });
});
