// A Claude prompt whose replay echo the CLI withholds still reaches the
// timeline, its turn settles, and the turns after it keep their own rows.
//
// Claude 2.1.284 echoes a turn-opening prompt only when its replay filter
// accepts the text, and the filter rejects any prompt that contains a tag
// such as `<task-notification>` (claude-wire.md §Outbound user message). The
// mock reproduces that filter. Before the fix the send was confirmed by the
// echo alone, so such a prompt left its pending send stranded: an agent
// message never rendered, the thread read as running forever, and every
// later turn reopened the stranded turn's index and overwrote its rows.
// The prompt's transcript record now confirms it.
//
// Coverage: a typed prompt quoting the tag followed by two more turns; a
// thread-tools wake quoting it delivered to an idle thread; and a quoting
// prompt whose turn the user interrupts before sending the next message.
import type { Page, TestInfo } from '@playwright/test';
import { expect, test, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  claudeTurnsScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';
import {
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
} from './thread-tools-helpers.js';

const QUOTED = 'Why does the log print `<task-notification>` before every report?';

// The thread is idle in every place a user would look: no working rail, no
// Stop button, no message parked in the send-queue preview.
async function expectIdle(page: Page): Promise<void> {
  await expect(page.getByTestId('activity-rail-working')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Interrupt current turn', exact: true })).toHaveCount(0);
  await expect(page.getByTestId('send-queue-preview-row')).toHaveCount(0);
}

// Conversation rows in timeline order, as the page renders them.
async function renderedConversation(page: Page): Promise<string[]> {
  return await page.evaluate(() =>
    Array.from(
      document.querySelectorAll('[data-testid="user-message-bubble"], [data-testid="assistant-message-body"]'),
    ).map((el) => (el.textContent ?? '').trim()),
  );
}

// Conversation rows in stored order, user and assistant text only.
async function storedConversation(harness: HarnessApp, threadId: string): Promise<string[]> {
  const items = await listItems(harness, threadId);
  return items
    .filter((item) => item.kind === 'user_text' || item.kind === 'assistant_text')
    .map((item) => item.summary.trim());
}

async function send(page: Page, text: string): Promise<void> {
  const input = page.getByLabel('Message Input');
  await input.fill(text);
  await page.getByRole('button', { name: 'Send message', exact: true }).click();
}

async function capture(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  await page.screenshot({ path: testInfo.outputPath(name) });
}

test('a typed prompt the CLI does not echo lands, settles, and the next turns keep their rows', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('unechoed-direct', [
      [emit([...textLines('msg-1', 'It is the wrapper the CLI puts around background reports.'), RESULT_LINE])],
      [emit([...textLines('msg-2', 'Answer to g.'), RESULT_LINE])],
      [emit([...textLines('msg-3', 'Answer to the follow-up.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'unechoed-direct', 'Unechoed direct');
  await harness.open(page);
  await page.getByText('Unechoed direct').click();
  await startMock(harness, threadId);

  await send(page, QUOTED);
  await awaitTurnCompleted(harness, threadId);
  await expect(page.getByText('It is the wrapper the CLI puts around background reports.')).toBeVisible();
  await expectIdle(page);

  await send(page, 'g');
  await awaitTurnCompleted(harness, threadId);
  await expect(page.getByText('Answer to g.')).toBeVisible();
  await send(page, 'And the follow-up?');
  await awaitTurnCompleted(harness, threadId);
  await expect(page.getByText('Answer to the follow-up.')).toBeVisible();
  await expectIdle(page);
  await capture(page, testInfo, 'unechoed-direct.png');

  const want = [
    'set the stage',
    'Ready.',
    QUOTED,
    'It is the wrapper the CLI puts around background reports.',
    'g',
    'Answer to g.',
    'And the follow-up?',
    'Answer to the follow-up.',
  ];
  expect(await storedConversation(harness, threadId)).toEqual(want);
  await expect.poll(() => renderedConversation(page)).toEqual(want);
});

test('a wake that quotes the tag reaches an idle thread and settles it', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'unechoed-wake',
        repo: {},
        threads: [
          {
            title: 'Unechoed wake',
            provider: 'claude',
            turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];
  const note = 'check that each `<task-notification>` block carries its task id';

  // The wake is the app's own message, queued through the same path a late
  // thread_reply takes, and it reaches the thread once its turn has ended,
  // so it opens a turn of its own: the incident's shape.
  await setScenario(
    harness,
    seed.projects[0].path,
    threadToolsScenario({
      name: 'unechoed-wake-script',
      provider: 'claude',
      turns: [
        {
          steps: [{ call: { tool: 'thread_remind', args: { after_seconds: 1, note } } }],
          text: 'Reminder set.',
        },
        { text: 'Every block carries its task id.' },
      ],
      afterTurns: 'silent',
    }),
  );

  await harness.open(page);
  await page.getByText('Unechoed wake').click();
  await harness.rpc('StartSession', threadId);
  await harness.rpc('SendMessage', threadId, 'remind me to check the task ids', null);
  const remind = await awaitToolAnswer(harness, { tool: 'thread_remind', timeoutMs: 60_000 });
  expect(remind.isError, remind.text).toBe(false);
  await awaitTurnCompleted(harness, threadId);
  await awaitTurnCompleted(harness, threadId);

  await expect(page.getByTestId('user-message-bubble').filter({ hasText: note })).toBeVisible();
  await expect(page.getByText('Every block carries its task id.')).toBeVisible();
  await expectIdle(page);
  await capture(page, testInfo, 'unechoed-wake.png');

  const stored = await storedConversation(harness, threadId);
  expect(stored).toHaveLength(6);
  expect(stored.slice(2, 4)).toEqual(['remind me to check the task ids', 'Reminder set.']);
  expect(stored[4]).toContain(note);
  expect(stored[5]).toBe('Every block carries its task id.');
});

test('interrupting a prompt the CLI does not echo keeps it above its output and the next message below', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('unechoed-interrupt', [
      [
        emit(textLines('msg-1', 'Looking through the log first.')),
        { waitSignal: { name: 'hold' } },
        emit([...textLines('msg-1b', 'Never reached.'), RESULT_LINE]),
      ],
      [emit([...textLines('msg-2', 'Answer to g.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'unechoed-interrupt', 'Unechoed interrupt');
  await harness.open(page);
  await page.getByText('Unechoed interrupt').click();
  await startMock(harness, threadId);

  await send(page, QUOTED);
  await waitForGate(harness, 'hold');
  await expect(page.getByText('Looking through the log first.')).toBeVisible();
  await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
  await awaitTurnCompleted(harness, threadId);
  await expectIdle(page);

  await send(page, 'g');
  await awaitTurnCompleted(harness, threadId);
  await expect(page.getByText('Answer to g.')).toBeVisible();
  await expectIdle(page);
  await capture(page, testInfo, 'unechoed-interrupt.png');

  const want = ['set the stage', 'Ready.', QUOTED, 'Looking through the log first.', 'g', 'Answer to g.'];
  expect(await storedConversation(harness, threadId)).toEqual(want);
  await expect.poll(() => renderedConversation(page)).toEqual(want);
});
