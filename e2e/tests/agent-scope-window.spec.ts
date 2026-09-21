// Subagent history beyond the old wholesale cap: only a run window is sent,
// additional members load on demand, and a cold pane retains the same history.
import { test, expect } from './fixtures.js';
import { RESULT_LINE, claudeScenario, emit, seedAgentThread, startMock,
  taskStartedLine, taskUpdatedLine, textLines, toolUseLine, toolResultLine } from './agent-visibility-helpers.js';

const COUNT = 2100;

test('a large subagent transcript pages activity members independently of its main thread', async ({ harness, page }) => {
  test.setTimeout(90_000);
  const activity = Array.from({ length: COUNT }, (_, index) => [
    toolUseLine(`call-${index}`, `bash-${index}`, 'Bash', { command: `echo scope-command-${index}` }, 'scope-root'),
    toolResultLine(`bash-${index}`, 'done', { parentToolUseId: 'scope-root' }),
  ]).flat();
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('large-scope', [emit([
    toolUseLine('scope-launch', 'scope-root', 'Agent', { description: 'Large scoped history', subagent_type: 'Explore' }),
    taskStartedLine('scope-task', 'scope-root', 'Large scoped history'),
    ...textLines('scope-intro', 'Beginning scoped history', 'scope-root'),
    ...activity,
    ...textLines('scope-answer', 'Scoped history complete', 'scope-root'),
    taskUpdatedLine('scope-task', { status: 'completed', end_time: 1787415964725 }),
    toolResultLine('scope-root', 'Scoped history complete'), RESULT_LINE,
  ])]) });
  const threadId = await seedAgentThread(harness, 'large-scope', 'Large scope');
  await harness.open(page);
  await page.getByText('Large scope', { exact: true }).click();
  await startMock(harness, threadId);
  const settled = harness.waitForEvent('provider:turn_completed', undefined, 75_000);
  await harness.rpc('SendMessage', threadId, 'Inspect all history', null);
  await settled;

  const main = page.getByTestId('message-timeline-scroll').first();
  const card = main.getByTestId('subagent-group').first();
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByText('Scoped history complete', { exact: true })).toBeVisible();
  const run = pane.getByTestId('activity-run');
  await expect(run.getByTestId('activity-run-header-counts')).toContainText(`${COUNT} Bash`);
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const rows = pane.getByTestId('command-output-row');
  await expect(rows).toHaveCount(30);
  await expect(rows.last()).toContainText(`scope-command-${COUNT - 1}`);
  const earlier = pane.getByTestId('activity-run-earlier');
  await expect(earlier).toContainText(`${COUNT - 30} earlier`);
  await earlier.click();
  await expect(rows).toHaveCount(55);
  await expect(earlier).toContainText(`${COUNT - 55} earlier`);
  await expect(main.getByTestId('command-output-row')).toHaveCount(0);

  const result = await harness.rpc<{ items: Array<{ parentId: string }>; runs: Array<{ memberCount: number }> }>(
    'ListThreadSliceAround', threadId, '', 200, { runWindowRows: 30, selection: { scopeRootId: 'scope-root' } });
  expect(result.items.length).toBeLessThan(40);
  expect(result.items.every(item => item.parentId === 'scope-root')).toBe(true);
  expect(result.runs[0].memberCount).toBe(COUNT);
});


test('an older host keeps main history available and reports unsupported agent history', async ({ harness, page }) => {
  let scopedReads = 0;
  await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
    const server = socket.connectToServer();
    socket.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'rpc' && frame.params?.some((arg: unknown) =>
        arg && typeof arg === 'object' && 'selection' in arg && (arg as { selection?: { scopeRootId?: string } }).selection?.scopeRootId)) scopedReads++;
      server.send(message);
    });
    server.onMessage(message => {
      const frame = JSON.parse(String(message));
      if (frame.type === 'hello') {
        frame.capabilities = frame.capabilities.filter((capability: string) => capability !== 'timeline.scopes.v1');
        socket.send(JSON.stringify(frame));
      } else socket.send(message);
    });
  });
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('old-scope-host', [emit([
    toolUseLine('old-launch', 'old-root', 'Agent', { description: 'Older agent history', subagent_type: 'Explore' }),
    ...textLines('child', 'Child history', 'old-root'),
    toolResultLine('old-root', 'Agent complete'),
    ...textLines('main-answer', 'Main history remains available'), RESULT_LINE,
  ])]) });
  const thread = await seedAgentThread(harness, 'old-scope-host', 'Older host');
  await harness.open(page);
  await page.getByText('Older host', { exact: true }).click();
  await startMock(harness, thread);
  const settled = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', thread, 'Read history', null);
  await settled;
  const main = page.getByTestId('message-timeline-scroll').first();
  await expect(main.getByText('Main history remains available', { exact: true })).toBeVisible();
  const completedRun = main.getByTestId('activity-run').first();
  await expect(completedRun).toHaveAttribute('data-live', 'false');
  await expect(async () => {
    if ((await completedRun.getAttribute('data-collapsed')) === 'true') {
      await completedRun.getByTestId('activity-run-header').click();
    }
    await main.getByTestId('subagent-group-open-pane').first().click({ timeout: 500 });
  }).toPass();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByText('Update the computer hosting this thread to load its agent history.', { exact: true })).toBeVisible();
  await expect(pane.getByText('Main history remains available', { exact: true })).toHaveCount(0);
  expect(scopedReads).toBe(0);
});
