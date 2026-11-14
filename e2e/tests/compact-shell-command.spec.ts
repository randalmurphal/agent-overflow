// Composer shell commands on the phone layout: Return is a newline there, so
// the Run button is the only way to run; the running card and the sent
// command block fit the screen at every supported width, and Stop and the
// output toggle are reachable by touch. A prompt on the command's terminal
// is answered by touch and its row fits the screen.
import { expect, test } from './fixtures.js';
import {
  RESULT_LINE,
  claudeTurnsScenario,
  emit,
  seedAgentThread,
  startMock,
  textLines,
} from './agent-visibility-helpers.js';

test('a phone runs a command with the Run button and the card and block fit the screen', async ({
  harness,
  page,
}, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('compact-shell-command', [
      [emit([...textLines('msg-reply', 'Read the long line.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-shell-command', 'Phone shell');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);

  const long = 'a-deliberately-long-unbroken-token-that-is-wider-than-any-phone-screen-'.repeat(3);
  const command = `echo ${long}; seq 1 40; sleep 1.5; echo done`;
  const input = page.getByLabel('Message Input');
  await input.fill(`!${command}`);
  await expect(page.getByTestId('composer-shell-hint')).toBeVisible();

  // Return is a newline on the phone: it must not run the command.
  await input.press('Enter');
  await expect(page.getByTestId('shell-run-card')).toHaveCount(0);
  await input.fill(`!${command}`);

  const run = page.getByRole('button', { name: 'Run command', exact: true });
  await expect(run).toHaveText('Run');
  await run.tap();

  const card = page.getByTestId('shell-run-card');
  await expect(card.getByTestId('shell-run-output')).toContainText('40');
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 850 });
    const box = (await card.boundingBox())!;
    expect(box.x, `card left at ${width}`).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width, `card right at ${width}`).toBeLessThanOrEqual(width);
    const stop = (await card.getByTestId('shell-run-stop').boundingBox())!;
    expect(stop.x + stop.width, `stop at ${width}`).toBeLessThanOrEqual(width);
  }
  await page.setViewportSize({ width: 412, height: 850 });
  const path = testInfo.outputPath('shell-compact-running.png');
  await page.screenshot({ path });
  await testInfo.attach('shell-compact-running', { path, contentType: 'image/png' });

  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-outcome')).toContainText('ok', { timeout: 30_000 });
  await expect(card).toHaveCount(0);
  await expect(page.getByText('Read the long line.')).toBeVisible();
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 850 });
    const box = (await block.boundingBox())!;
    expect(box.x, `block left at ${width}`).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width, `block right at ${width}`).toBeLessThanOrEqual(width);
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth))
      .toBeLessThanOrEqual(1);
  }
  await page.setViewportSize({ width: 412, height: 850 });

  // The output toggle and the copy control are reachable without hover.
  await block.getByTestId('shell-command-toggle').tap();
  await expect(block.getByTestId('shell-command-output')).toContainText('done');
  await expect(block.getByRole('button', { name: 'Copy command' })).toBeVisible();
  const sent = testInfo.outputPath('shell-compact-sent.png');
  await page.screenshot({ path: sent });
  await testInfo.attach('shell-compact-sent', { path: sent, contentType: 'image/png' });
});

test('a phone answers a command\'s prompt by touch and the answer row fits the screen', async ({ harness, page }, testInfo) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('compact-shell-prompt', [
      [emit([...textLines('msg-reply', 'It read the answer.'), RESULT_LINE])],
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-shell-prompt', 'Phone prompt');
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);

  const command = "printf 'Continue with the deploy to the production cluster (yes/no)? ' > /dev/tty; IFS= read -r a < /dev/tty; echo \"answer=$a\"";
  await page.getByLabel('Message Input').fill(`!${command}`);
  await page.getByRole('button', { name: 'Run command', exact: true }).tap();

  const card = page.getByTestId('shell-run-card');
  const answer = card.getByTestId('shell-run-answer');
  await expect(card.getByTestId('shell-run-prompt')).toContainText('(yes/no)?');
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 850 });
    for (const id of ['shell-run-answer', 'shell-run-answer-reveal', 'shell-run-answer-send', 'shell-run-stop']) {
      const box = (await card.getByTestId(id).boundingBox())!;
      expect(box.x, `${id} left at ${width}`).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width, `${id} right at ${width}`).toBeLessThanOrEqual(width);
      expect(box.height, `${id} height at ${width}`).toBeGreaterThanOrEqual(36);
    }
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth))
      .toBeLessThanOrEqual(1);
  }
  await page.setViewportSize({ width: 412, height: 850 });

  await answer.tap();
  await page.keyboard.type('yes');
  // Masked until asked for.
  await expect(answer).toHaveAttribute('type', 'password');
  await card.getByTestId('shell-run-answer-reveal').tap();
  await expect(answer).toHaveAttribute('type', 'text');
  await expect(answer).toHaveValue('yes');
  const path = testInfo.outputPath('shell-compact-prompt.png');
  await page.screenshot({ path });
  await testInfo.attach('shell-compact-prompt', { path, contentType: 'image/png' });
  await card.getByTestId('shell-run-answer-send').tap();

  const block = page.getByTestId('shell-command-block');
  await expect(block.getByTestId('shell-command-output')).toContainText('answer=yes', { timeout: 30_000 });
  await expect(card).toHaveCount(0);
  await expect(page.getByText('It read the answer.')).toBeVisible();
});
