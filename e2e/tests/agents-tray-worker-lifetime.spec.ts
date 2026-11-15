// Documents current behavior, not the intended contract: the Agents segment
// follows a request's lifetime, not its worker's
// (docs/specs/standing-agents.md, "Agents segment").
//
// Coverage: a worker that answers its spawn with thread_reply and keeps
// working in the same turn reads as replied in its spawner's Agents list, with
// no Stop and no live line, is not counted on the toggle or the sidebar row,
// and leaves the open list for Past after the linger while its turn is still
// running.
import { test, expect, type SeedResult } from './fixtures.js';
import {
  FOOTER_TOKEN_PATTERN,
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { LEAD, agentRow, agentsBody, companionPane, openAgents, openPast, pastRow } from './agents-tray-helpers.js';

const WORKER = 'Deploy watcher';
const REPLY = 'The deploy started; I am watching it.';

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker that replies and keeps working reads as settled while it runs`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const tag = `agents-lifetime-${provider}`;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
        { name: `${tag}-worker`, repo: {}, threads: [] },
      ],
    });
    const [leadP, workerP] = seed.projects;
    const lead = leadP.threadIds[0];
    await setScenario(harness, leadP.path, threadToolsScenario({
      name: `${tag}-lead`, provider, echoUserMessage: true,
      turns: [
        { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Rerun the deploy, tell me when it starts, then watch it.', title: WORKER, project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await setScenario(harness, workerP.path, threadToolsScenario({
      name: `${tag}-worker`, provider, echoUserMessage: true,
      turns: [
        { steps: [
          { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
          { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: REPLY } } },
          // Still in the same turn: watching the deploy.
          { gate: 'worker-hold' },
        ], text: 'Watched the deploy to the end.' },
        { text: 'Noted.' },
      ],
      afterTurns: 'repeatLast',
    }));
    await harness.rpc('StartSession', lead);
    await harness.rpc('SendMessage', lead, 'rerun the deploy', null);
    const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
    expect(spawn.isError, spawn.text).toBe(false);
    const { token, thread_id: worker } = spawn.value!;
    const reply = await awaitToolAnswer(harness, { tool: 'thread_reply', cwd: workerP.path, timeoutMs: 60_000 });
    expect(reply.isError, reply.text).toBe(false);
    const hold = await awaitGate(harness, 'worker-hold', workerP.path);
    const workerRunning = async () => Boolean((await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn);
    expect(await workerRunning(), 'the worker is still in its turn').toBe(true);

    await harness.open(page);
    await openAgents(page, LEAD);
    const row = agentRow(page, token);
    // The worker runs, and its row reads as settled: replied, no Stop.
    await expect(row.getByTestId('agent-tray-row-state')).toHaveText('replied', { timeout: 30_000 });
    await expect(row).toHaveAttribute('data-settled', 'true');
    await expect(row.getByTestId('agent-tray-row-stop')).toHaveCount(0);
    await expect(row.getByTestId('agent-tray-row-activity')).toHaveCount(0);
    await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveCount(0);
    const leadRow = page.getByTestId('thread-row').filter({ has: page.getByTestId('thread-row-title').getByText(LEAD, { exact: true }) });
    await expect(leadRow.getByTestId('thread-row-workers')).toHaveCount(0);
    expect(await harness.rpc<{ open: number }>('GetThreadWorkerSummary', lead)).toMatchObject({ open: 0, needYou: 0 });
    expect(await workerRunning()).toBe(true);
    await capture(page, `lifetime-${provider}-replied-while-running.png`, agentsBody(page));

    // After the linger it is only in Past, while the worker still runs.
    await expect(agentsBody(page).locator('[data-testid="agent-tray-row"]:not([data-past])')).toHaveCount(0, { timeout: 30_000 });
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('1');
    expect(await workerRunning(), 'the worker is still in its turn after its row left').toBe(true);
    await capture(page, `lifetime-${provider}-past-while-running.png`, agentsBody(page));

    // Opened beside, the worker is visibly still working.
    await openPast(page);
    await pastRow(page, token).getByTestId('agent-tray-row-open-pane').click();
    await expect(companionPane(page)).toBeVisible({ timeout: 30_000 });
    expect(await workerRunning()).toBe(true);
    await capture(page, `lifetime-${provider}-past-with-worker-open.png`, agentsBody(page), companionPane(page));

    await advanceGate(harness, hold.mockId, 'worker-hold');
  });
}
