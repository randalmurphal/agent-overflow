// Empty terminal polls preserve process state without history rows. Late runtime
// events cannot split a Codex answer; live activity expansion survives batching.
import { test, expect } from './fixtures.js';
import { advance, seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';

const ANSWER = 'One complete answer continues through the late activity.';
const PREFIX = 'One complete answer ';
const rpc = (method: string, params: object) => JSON.stringify({ jsonrpc: '2.0', method, params });
const event = (method: string, fields: object, turnId = '${TURN_ID}') => rpc(method, { threadId: '${THREAD_ID}', turnId, ...fields });
const emit = (lines: string[]) => ({ emit: { lines } });
const gate = (name: string) => ({ waitSignal: { name } });
const start = () => rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } });
const finish = () => rpc('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } });
const poll = (turnId = '${TURN_ID}') => event('item/commandExecution/terminalInteraction', { itemId: 'old-command', processId: '42', stdin: '' }, turnId);
const command = (id: string, status: string, text: string, processId = '42') => event(`item/${status === 'inProgress' ? 'started' : 'completed'}`, {
  item: { id, type: 'commandExecution', source: 'unifiedExecStartup', processId, status, command: text,
    ...(status === 'completed' ? { exitCode: 0, aggregatedOutput: 'command finished\n' } : {}) },
});
interface Row { id: string; kind: string; summary: string; status: string; itemIndex: number; turnIndex: number }

for (const interrupted of [false, true]) {
  test(`${interrupted ? 'interrupted' : 'normal'} polls leave the working chip and answer intact`, async ({ harness, page }) => {
    await harness.rpc('UpdateSettings', { spinnerVerbsEnabled: true, spinnerCustomVerbs: ['Vibing'], spinnerBuiltinVerbsDisabled: true });
    const answerSteps = [
      emit([event('item/agentMessage/delta', { itemId: 'answer', delta: PREFIX })]), gate('answer-started'),
      emit([poll(interrupted ? 'turn-1' : '${TURN_ID}'), poll(interrupted ? 'turn-1' : '${TURN_ID}'),
        command('late-command', 'inProgress', 'echo late', '43'), command('late-command', 'completed', 'echo late', '43')]),
      gate('late-events'),
      emit([event('item/agentMessage/delta', { itemId: 'answer', delta: ANSWER.slice(PREFIX.length) }),
        event('item/completed', { item: { id: 'answer', type: 'agentMessage', text: ANSWER, phase: 'final_answer' } }), finish()]),
      gate('finished'),
    ];
    await harness.rpc('HarnessSetScenario', { scenario: {
      version: 1, name: `codex-polls-${interrupted}`, provider: 'codex', afterTurns: 'silent',
      turns: interrupted ? [
        { steps: [emit([start(), command('old-command', 'inProgress', 'sleep 60')]), gate('polling')] },
        { steps: [emit([start()]), ...answerSteps] },
      ] : [{ steps: [emit([start(), command('old-command', 'inProgress', 'sleep 60')]), gate('polling'), ...answerSteps] }],
    } });
    const title = `Terminal polls ${interrupted}`;
    const threadId = await seedAgentThread(harness, `polls-${interrupted}`, title, 'codex');
    await harness.open(page);
    await page.getByText(title, { exact: true }).click();
    const mock = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'run command', null);
    await waitForGate(harness, 'polling');
    const chip = page.getByTestId('activity-rail-working');
    await expect(chip.locator('[data-activity-rail-verb]')).toHaveText('Vibing');
    if (interrupted) {
      await page.getByRole('button', { name: 'Interrupt current turn', exact: true }).click();
      await expect(chip).toHaveCount(0);
      await harness.rpc('SendMessage', threadId, 'answer now', null);
    } else await advance(harness, mock, 'polling');
    await waitForGate(harness, 'answer-started');
    await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveText(PREFIX.trim());
    await expect(chip.locator('[data-activity-rail-verb]')).toHaveText('Vibing');
    const chipBefore = await chip.evaluate(el => ({ verb: el.querySelector('[data-activity-rail-verb]')?.textContent,
      sprite: el.querySelector('.working-sprite')?.getAttribute('data-sprite-id'), leds: !!el.querySelector('[data-testid="activity-rail-working-leds"]') }));
    await advance(harness, mock, 'answer-started');
    await waitForGate(harness, 'late-events');
    const rows = await harness.rpc<Row[]>('ListItems', threadId, true);
    expect(rows.filter(row => row.kind === 'terminal_interaction')).toEqual([]);
    expect(rows.filter(row => row.kind === 'assistant_text' && row.summary !== 'Ready.')).toHaveLength(1);
    expect(rows.find(row => row.kind === 'assistant_text' && row.summary !== 'Ready.')?.status).toBe('streaming');
    await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveCount(1);
    await expect(page.locator('[data-item-id="late-command"]')).toHaveCount(0);
    expect(await chip.evaluate(el => ({ verb: el.querySelector('[data-activity-rail-verb]')?.textContent,
      sprite: el.querySelector('.working-sprite')?.getAttribute('data-sprite-id'), leds: !!el.querySelector('[data-testid="activity-rail-working-leds"]') }))).toEqual(chipBefore);
    await expect(page.getByRole('button', { name: 'Background 1', exact: true })).toBeVisible();
    await advance(harness, mock, 'late-events');
    await waitForGate(harness, 'finished');
    await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveText(ANSWER);
    await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveCount(1);
    await expect(chip).toHaveCount(0);
    await expect(page.locator('[data-item-id="late-command"]')).toBeVisible();
    const final = await harness.rpc<Row[]>('ListItems', threadId, true);
    expect(final.filter(row => row.kind === 'terminal_interaction')).toEqual([]);
    expect(final.filter(row => row.kind === 'assistant_text' && row.summary !== 'Ready.').map(row => row.summary)).toEqual([ANSWER]);
    const answer = final.find(row => row.kind === 'assistant_text' && row.summary !== 'Ready.')!;
    expect(final.find(row => row.id === 'late-command')!.itemIndex).toBeGreaterThan(answer.itemIndex);
    await page.reload();
    await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveText(ANSWER);
    expect((await harness.rpc<Row[]>('ListItems', threadId, true)).map(row => [row.id, row.itemIndex, row.summary]))
      .toEqual(final.map(row => [row.id, row.itemIndex, row.summary]));
  });
}

test('a completed singleton and following answer arriving together render expanded', async ({ harness, page }) => {
  await harness.rpc('HarnessSetScenario', { scenario: {
    version: 1, name: 'codex-singleton-batch', provider: 'codex', afterTurns: 'silent', turns: [{ steps: [
      emit([start()]), gate('burst'),
      emit([command('quick', 'inProgress', 'echo quick'), command('quick', 'completed', 'echo quick'),
        event('item/completed', { item: { id: 'answer', type: 'agentMessage', text: 'Finished the quick command.' } }), finish()]),
      gate('finished'),
    ] }],
  } });
  const threadId = await seedAgentThread(harness, 'singleton', 'Singleton activity', 'codex');
  await harness.open(page);
  await page.getByText('Singleton activity', { exact: true }).click();
  const mock = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'quick command', null);
  await waitForGate(harness, 'burst');
  await advance(harness, mock, 'burst');
  await waitForGate(harness, 'finished');
  await expect(page.getByTestId('assistant-message-body').filter({ hasNotText: 'Ready.' })).toHaveText('Finished the quick command.');
  const run = page.getByTestId('activity-run');
  await expect(run).toHaveCount(1);
  await expect(run).toHaveAttribute('data-collapsed', 'false');
  await run.getByTestId('activity-run-header').click();
  await expect(run).toHaveAttribute('data-collapsed', 'true');
});
