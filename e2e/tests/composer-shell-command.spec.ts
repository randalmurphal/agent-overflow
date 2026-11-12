// Composer shell commands: `!command` in the composer, and Run on a shell
// code block. The app runs the command in the thread's workspace, shows it
// above the composer while it runs, and sends the output to the agent as a
// user message whose row is a command block.
//
// Covered here because only the real backend and a real page can show them:
// the command runs in the seeded workspace, the provider receives the output
// in its own `!` record, the live card hands over to the timeline row, a
// reload during a run finds it again, Stop sends nothing, a command run
// mid-turn waits in the send queue, a command is the first message of a new
// thread, Run appears on shell fences only, inside a surface that may run
// them, and a prompt on the command's terminal is answered from the card
// with an answer that reaches the command and nothing else.
import type { Page, TestInfo } from '@playwright/test';
import type { HarnessApp, HarnessMockEventData } from '../src/harness-app.js';
import { expect, test, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeTurnsScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  textLines,
  waitForGate,
} from './agent-visibility-helpers.js';

function nextUserInput(harness: HarnessApp, containing: string): Promise<HarnessMockEventData> {
  return harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && (ev.report.input ?? '').includes(containing),
  );
}

async function openThread(harness: HarnessApp, page: Page, title: string): Promise<void> {
  await harness.open(page);
  await page.getByText(title, { exact: true }).click();
  await expect(page.getByLabel('Message Input')).toBeVisible();
}

/** A viewport capture per theme, attached to the report for the design pass. */
async function shoot(page: Page, testInfo: TestInfo, state: string): Promise<void> {
  for (const theme of ['dark', 'light'] as const) {
    await page.emulateMedia({ colorScheme: theme });
    await expect(page.locator('html')).toHaveClass(new RegExp(`\\b${theme}\\b`));
    await page.evaluate(() => Promise.all(
      document.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => undefined)),
    ));
    const path = testInfo.outputPath(`shell-${state}-${theme}.png`);
    await page.screenshot({ path });
    await testInfo.attach(`shell-${state}-${theme}`, { path, contentType: 'image/png' });
  }
  await page.emulateMedia({ colorScheme: 'dark' });
}

test('a !command runs in the workspace, shows its output while it runs, and is sent to the agent', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-idle', [
      [emit([...textLines('msg-reply', 'Three steps ran and the README is the only file.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-idle', 'Shell command idle');
  await openThread(harness, page, 'Shell command idle');
  await startMock(harness, threadId);

  const input = page.getByLabel('Message Input');
  const hint = page.getByTestId('composer-shell-hint');
  await expect(hint).toHaveCount(0);

  // Shell mode is the first character, and only the first character.
  await input.fill('look at !this');
  await expect(hint).toHaveCount(0);
  await input.fill('!');
  await expect(hint).toContainText('the output is sent to the agent');
  await expect(page.getByRole('button', { name: 'Run command', exact: true })).toBeDisabled();

  const command = 'for i in 1 2 3; do echo "step $i"; sleep 0.6; done; ls';
  await input.fill(`!${command}`);
  await expect(page.getByRole('button', { name: 'Run command', exact: true })).toHaveText('Run');
  await shoot(page, testInfo, 'composer');

  const delivered = nextUserInput(harness, '<bash-input>');
  await input.press('Enter');

  // While it runs: the card above the composer, the text gone from the box,
  // and nothing in the timeline yet.
  const card = page.getByTestId('shell-run-card');
  await expect(card).toHaveAttribute('data-state', 'running');
  await expect(card.getByTestId('shell-run-command')).toHaveText(command);
  await expect(card.getByTestId('shell-run-output')).toContainText('step 1');
  await expect(input).toHaveValue('');
  await expect(page.getByTestId('shell-command-block')).toHaveCount(0);
  await shoot(page, testInfo, 'running');

  // The provider reads Claude's own record of a command the person ran,
  // with the output of a command that ran in the seeded workspace.
  expect((await delivered).report.input).toBe(
    `<bash-input>${command}</bash-input>\n<bash-stdout>step 1\nstep 2\nstep 3\nREADME.md</bash-stdout><bash-stderr></bash-stderr>`,
  );

  // After it exits: the card is gone and the message is one command block.
  const block = page.getByTestId('shell-command-block');
  await expect(block).toHaveCount(1);
  await expect(card).toHaveCount(0);
  await expect(block.getByTestId('shell-command-line')).toHaveText(command);
  await expect(block.getByTestId('shell-command-outcome')).toContainText('ok');
  await expect(block.getByTestId('shell-command-output')).toContainText('README.md');
  // The typed line is the stored message; the block stands for it on screen.
  await expect(page.getByTestId('user-message-bubble').last()).not.toContainText(`!${command}`);
  const rows = (await listItems(harness, threadId)).filter((item) => item.kind === 'user_text');
  expect(rows[rows.length - 1].summary).toBe(`!${command}`);

  await expect(page.getByText('Three steps ran and the README is the only file.')).toBeVisible();
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId).catch(() => undefined);
  await shoot(page, testInfo, 'sent');
});

test('a failing command shows its exit code and opens its whole output on demand', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-failed', [
      [emit([...textLines('msg-reply', 'It exited 3 after printing 400 lines.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-failed', 'Shell command failed');
  await openThread(harness, page, 'Shell command failed');
  await startMock(harness, threadId);

  const delivered = nextUserInput(harness, '<bash-input>');
  const input = page.getByLabel('Message Input');
  await input.fill('!seq 1 400; echo oops >&2; exit 3');
  await input.press('Enter');

  // A failure is Claude's stderr record: the output, then how it ended.
  const wire = (await delivered).report.input ?? '';
  expect(wire.startsWith('<bash-input>seq 1 400; echo oops >&2; exit 3</bash-input>\n<bash-stdout></bash-stdout><bash-stderr>1\n2\n')).toBe(true);
  expect(wire.endsWith('400\noops\nExit code 3</bash-stderr>')).toBe(true);

  const block = page.getByTestId('shell-command-block');
  await expect(block).toHaveAttribute('data-exit-code', '3');
  await expect(block.getByTestId('shell-command-outcome')).toContainText('exit 3');
  const output = block.getByTestId('shell-command-output');
  await expect(output).toContainText('12');
  await expect(output).not.toContainText('400');
  await expect(page.getByText('It exited 3 after printing 400 lines.')).toBeVisible();
  await shoot(page, testInfo, 'failed');

  await block.getByTestId('shell-command-toggle').click();
  await expect(output).toContainText('400\noops');
  await shoot(page, testInfo, 'expanded');
  await block.getByTestId('shell-command-toggle').click();
  await expect(output).not.toContainText('400');
});

test('Stop ends the command and sends nothing', async ({ harness, page }) => {
  test.setTimeout(60_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-stop', [
      [emit([...textLines('msg-reply', 'unexpected'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-stop', 'Shell command stop');
  await openThread(harness, page, 'Shell command stop');
  const before = (await listItems(harness, threadId)).length;

  const input = page.getByLabel('Message Input');
  await input.fill('!echo started; sleep 60');
  const finished = harness.waitForEvent(
    'shell:command',
    (ev: any) => ev.threadId === threadId && ev.phase === 'finished',
  );
  await input.press('Enter');

  const card = page.getByTestId('shell-run-card');
  await expect(card.getByTestId('shell-run-output')).toContainText('started');
  // A second command waits for the first: the composer says why.
  await input.fill('!echo second');
  await expect(page.getByTestId('composer-shell-hint')).toContainText('A command is already running');
  await expect(page.getByRole('button', { name: 'Run command', exact: true })).toBeDisabled();

  await card.getByTestId('shell-run-stop').click();
  expect(((await finished) as { state: string }).state).toBe('stopped');
  await expect(card).toHaveCount(0);
  await expect(page.getByTestId('shell-command-block')).toHaveCount(0);
  expect((await listItems(harness, threadId)).length).toBe(before);
  // The text typed meanwhile is still the person's, and can run now.
  await expect(input).toHaveValue('!echo second');
  await expect(page.getByRole('button', { name: 'Run command', exact: true })).toBeEnabled();
});

test('a reload during a run finds the command again and still shows its result', async ({
  harness,
  page,
}) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-reload', [
      [emit([...textLines('msg-reply', 'Saw the late line.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-reload', 'Shell command reload');
  await openThread(harness, page, 'Shell command reload');
  await startMock(harness, threadId);

  const input = page.getByLabel('Message Input');
  await input.fill('!echo early; sleep 4; echo late');
  await input.press('Enter');
  await expect(page.getByTestId('shell-run-output')).toContainText('early');

  // The run belongs to the backend. A page that was not there for its start
  // learns of it from the thread's live state and reads its output so far.
  await page.reload();
  const card = page.getByTestId('shell-run-card');
  await expect(card).toHaveAttribute('data-state', 'running');
  await expect(card.getByTestId('shell-run-output')).toContainText('early');

  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-output')).toContainText('late', { timeout: 30_000 });
  await expect(card).toHaveCount(0);
  await expect(page.getByText('Saw the late line.')).toBeVisible();
});

test('a command run mid-turn waits in the send queue and lands as its own row', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-queued', [
      [
        emit(textLines('msg-working', 'Working on it.')),
        { waitSignal: { name: 'finish' } },
        emit([...textLines('msg-final', 'First answer.'), RESULT_LINE]),
      ],
      [emit([...textLines('msg-second', 'Read the queued output.'), RESULT_LINE])],
    ], { holdQueuedInput: true }),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-queued', 'Shell command queued');
  await openThread(harness, page, 'Shell command queued');
  const mockId = await startMock(harness, threadId);

  const input = page.getByLabel('Message Input');
  await input.fill('Start the first turn.');
  await input.press('Enter');
  await waitForGate(harness, 'finish');

  const flushed = harness.waitForEvent('provider:queue_flushed', (ev: any) => ev.threadId === threadId);
  await input.fill('!echo queued-output');
  await input.press('Enter');
  await flushed;

  // The pending row names the command the person typed.
  const pending = page.getByTestId('send-queue-preview-row');
  await expect(pending).toContainText('!echo queued-output');
  await expect(page.getByTestId('shell-run-card')).toHaveCount(0);
  await shoot(page, testInfo, 'queued');

  const delivered = nextUserInput(harness, '<bash-input>');
  await advance(harness, mockId, 'finish');
  expect((await delivered).report.input).toBe(
    '<bash-input>echo queued-output</bash-input>\n<bash-stdout>queued-output</bash-stdout><bash-stderr></bash-stderr>',
  );
  await expect(page.getByText('Read the queued output.')).toBeVisible();
  await expect(pending).toHaveCount(0);
  await expect(page.getByTestId('shell-command-block').getByTestId('shell-command-output')).toContainText('queued-output');
});

test('a command is the first message of a new thread', async ({ harness, page }) => {
  test.setTimeout(90_000);
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'shell-command-draft', repo: { commits: [{ message: 'init', files: { 'README.md': '# fixture\n' } }] } }],
  });
  await harness.open(page);
  await page.getByTestId('project-item-new-thread').first().click();

  // Running the command empties the composer of a thread that has no
  // message yet. The thread has to outlive that until the output arrives.
  const input = page.getByLabel('Message Input');
  const delivered = nextUserInput(harness, '<bash-input>');
  await input.fill('!echo before; sleep 2; ls');
  await input.press('Enter');
  const card = page.getByTestId('shell-run-card');
  await expect(card.getByTestId('shell-run-output')).toContainText('before');
  await expect(input).toHaveValue('');

  expect((await delivered).report.input).toBe(
    '<bash-input>echo before; sleep 2; ls</bash-input>\n<bash-stdout>before\nREADME.md</bash-stdout><bash-stderr></bash-stderr>',
  );
  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-output')).toContainText('README.md');
  await expect(card).toHaveCount(0);
  // The default scenario answers the thread's first message.
  await expect(page.getByTestId('assistant-message-body')).toContainText(
    'This reply is streaming from the mock provider',
  );
});

test('Run on a shell code block runs the block and sends its output', async ({ harness, page }, testInfo) => {
  test.setTimeout(90_000);
  const reply = [
    'Run this to see the tree:',
    '',
    '```bash',
    'echo from-the-block',
    'ls',
    '```',
    '',
    'and this is not a shell block:',
    '',
    '```js',
    'console.log("no run button")',
    '```',
  ].join('\n');
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-block', [
      [emit([...textLines('msg-reply', reply), RESULT_LINE])],
      [emit([...textLines('msg-second', 'The block printed its line.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-block', 'Shell command block');
  await openThread(harness, page, 'Shell command block');
  await startMock(harness, threadId);

  const input = page.getByLabel('Message Input');
  await input.fill('What should I run?');
  await input.press('Enter');
  await harness.waitForEvent('provider:turn_completed', (ev: any) => ev.threadId === threadId);

  const run = page.getByRole('button', { name: 'Run and send the output to the agent' });
  await expect(run).toHaveCount(1);
  const fence = page.locator('.streamdown-code-host[data-code-lang="bash"]');
  await fence.hover();
  await expect(run).toBeVisible();
  await shoot(page, testInfo, 'code-block');

  // The control is shown by the surface's mark, which a session without
  // both grants does not get.
  const surface = page.locator('[data-ui-surface="chat"]');
  await surface.evaluate((el) => el.removeAttribute('data-shell-runnable'));
  await expect(run).toBeHidden();
  await surface.evaluate((el) => el.setAttribute('data-shell-runnable', ''));
  await fence.hover();
  await expect(run).toBeVisible();

  const delivered = nextUserInput(harness, '<bash-input>');
  await run.click();
  expect((await delivered).report.input).toBe(
    '<bash-input>echo from-the-block\nls</bash-input>\n<bash-stdout>from-the-block\nREADME.md</bash-stdout><bash-stderr></bash-stderr>',
  );
  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-line')).toHaveText('echo from-the-block\nls');
  await expect(block.getByTestId('shell-command-output')).toContainText('from-the-block');
  await expect(page.getByText('The block printed its line.')).toBeVisible();
  // The composer was not involved: nothing was typed into it.
  await expect(input).toHaveValue('');
});

test('a prompt on the command\'s terminal is answered from the card and the answer reaches only the command', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('shell-command-prompt', [
      [emit([...textLines('msg-reply', 'It read an answer of 21 characters.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'shell-command-prompt', 'Shell command prompt');
  await openThread(harness, page, 'Shell command prompt');
  await startMock(harness, threadId);

  // What sudo does: echo off, a prompt on the terminal, a line read from
  // it, and a line break once it is read.
  const command = [
    'stty -echo < /dev/tty',
    "printf 'Password: ' > /dev/tty",
    'IFS= read -r pw < /dev/tty',
    'stty echo < /dev/tty',
    'echo > /dev/tty',
    'sleep 1',
    'echo "length=${#pw}"',
  ].join('; ');
  const secret = 'correct horse battery';
  const delivered = nextUserInput(harness, '<bash-input>');
  const input = page.getByLabel('Message Input');
  await input.fill(`!${command}`);
  await input.press('Enter');

  // The card shows the whole command and the prompt, and the masked field
  // has taken focus from the composer. The prompt is not output.
  const card = page.getByTestId('shell-run-card');
  const answer = card.getByTestId('shell-run-answer');
  await expect(card.getByTestId('shell-run-prompt')).toContainText('The command is asking:');
  await expect(card.getByTestId('shell-run-prompt')).toContainText('Password:');
  await expect(answer).toHaveAttribute('type', 'password');
  await expect(answer).toBeFocused();
  await expect(card.getByTestId('shell-run-output')).toHaveCount(0);
  await shoot(page, testInfo, 'prompt');

  await page.keyboard.type(secret);
  await expect(input).toHaveValue('');
  await page.keyboard.press('Enter');

  // Answered: the terminal ended its line, so the field is gone while the
  // command runs on, and focus is back in the composer.
  await expect(answer).toHaveCount(0);
  await expect(card).toHaveAttribute('data-state', 'running');
  await expect(input).toBeFocused();

  // The command read all of it. The agent reads the output and no prompt.
  expect((await delivered).report.input).toBe(
    `<bash-input>${command}</bash-input>\n<bash-stdout>length=21</bash-stdout><bash-stderr></bash-stderr>`,
  );
  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-output')).toContainText('length=21');
  await expect(card).toHaveCount(0);
  await expect(page.getByText('It read an answer of 21 characters.')).toBeVisible();

  // The answer is in no row, nowhere on the page and in no saved draft.
  expect(JSON.stringify(await listItems(harness, threadId))).not.toContain(secret);
  expect(await page.content()).not.toContain(secret);
  const saved = await page.evaluate(() => JSON.stringify([{ ...localStorage }, { ...sessionStorage }]));
  expect(saved).not.toContain(secret);
});
