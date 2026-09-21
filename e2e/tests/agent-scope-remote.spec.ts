// Scoped member reads and retained history recovery across a paired backend.
import type { WebSocketRoute } from '@playwright/test';
import { test, expect } from './fixtures.js';
import { launchHarness } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock,
  taskStartedLine, taskUpdatedLine, textLines, toolUseLine, toolResultLine, waitForGate } from './agent-visibility-helpers.js';

test('a remote agent pane retains loaded run history through a disconnected completion', async ({ page }) => {
  test.setTimeout(120_000);
  const harness = await launchHarness();
  try {
    const remote = await launchHarness();
    try {
      const tools = (start: number, count: number) => Array.from({ length: count }, (_, offset) => {
        const i = start + offset;
        return [toolUseLine(`call-${i}`, `tool-${i}`, 'Bash', { command: `echo remote-scope-${i}` }, 'remote-root'),
          toolResultLine(`tool-${i}`, `remote result ${i}`, { parentToolUseId: 'remote-root' })];
      }).flat();
      await remote.rpc('HarnessSetScenario', { scenario: claudeScenario('remote-scope', [
        emit([toolUseLine('launch', 'remote-root', 'Agent', { description: 'Remote scoped history', subagent_type: 'Explore' }),
          taskStartedLine('remote-task', 'remote-root', 'Remote scoped history'),
          ...textLines('intro', 'Remote work started', 'remote-root'), ...tools(0, 90)]),
        { waitSignal: { name: 'finish' } },
        emit([...tools(90, 30), ...textLines('answer', 'Remote scoped work completed', 'remote-root'),
          taskUpdatedLine('remote-task', { status: 'completed', end_time: 1787415964725 }),
          toolResultLine('remote-root', 'Remote scoped work completed'), RESULT_LINE]),
      ]) });
      const thread = await seedAgentThread(remote, 'remote-scope', 'Remote scope reader');
      const pairing = await headlessPairing(remote);
      try {
        const attached = await harness.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
        await pairing.confirm(attached.verificationNumber);
      } finally { pairing.close(); }

      let online = true;
      let connection: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
      await page.routeWebSocket(/\/ws(?:\?|$)/, socket => {
        if (!online) { void socket.close({ code: 1012 }); return; }
        const server = socket.connectToServer();
        connection = { page: socket, server };
        socket.onMessage(message => server.send(message));
        server.onMessage(message => socket.send(message));
      });
      await harness.open(page);
      await page.getByText('Remote scope reader', { exact: true }).click();
      const mock = await startMock(remote, thread);
      await remote.rpc('SendMessage', thread, 'Inspect remote history', null);
      await waitForGate(remote, 'finish');
      const main = page.getByTestId('message-timeline-scroll').first();
      await main.getByTestId('subagent-group-open-pane').first().click();
      const pane = page.getByTestId('companion-pane-agent-body');
      const run = pane.getByTestId('activity-run');
      await expect(run.getByTestId('activity-run-header-counts')).toContainText('90 Bash');
      if ((await run.getAttribute('data-collapsed')) === 'false') await run.getByTestId('activity-run-header').click();
      await run.getByTestId('activity-run-header').click();
      await pane.getByTestId('activity-run-earlier').click();
      await expect(pane.getByTestId('command-output-row')).toHaveCount(55);
      await expect(pane.getByTestId('activity-run-earlier')).toContainText('35 earlier');
      const clip = pane.getByTestId('activity-run-clip');
      const priorTop = await clip.evaluate(element => element.scrollTop);
      await clip.hover();
      await page.mouse.wheel(0, -200);
      await expect.poll(() => clip.evaluate(element => element.scrollTop)).toBeLessThan(priorTop);

      online = false;
      await connection!.page.close({ code: 1012 });
      await connection!.server.close();
      const completed = remote.waitForEvent('provider:turn_completed');
      await advance(remote, mock, 'finish');
      await completed;
      online = true;
      await expect(run.getByTestId('activity-run-header-counts')).toContainText('120 Bash');
      await expect(pane.getByTestId('activity-run-earlier')).toContainText('35 earlier');
      await expect(pane.getByTestId('command-output-row').first()).toContainText('remote-scope-35');
      await expect(main.getByTestId('command-output-row').filter({ hasText: 'remote-scope-35' })).toHaveCount(0);
    } finally {
      await page.close();
      await remote.close();
    }
  } finally {
    await harness.close();
  }
});
