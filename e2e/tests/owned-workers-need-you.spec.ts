// A row's needs-you badge (docs/specs/standing-agents.md, "Sidebar"): an
// assistant's worker with a sidebar row asks for an approval in the
// assistant's run, the assistant's session dies before taking it in, and
// the prompt goes back to the user (an owned worker's would wait for the
// assistant). The assistant's
// row then counts its open worker and badges the one waiting on the user;
// the badge goes once the user answers and the count once the worker ends.
import { test, expect, type SeedResult } from './fixtures.js';
import {
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
} from './thread-tools-helpers.js';
import { wakeInput } from './standing-agent-wake-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';
import { assistantRow, assistantsSection } from './sidebar-sections-helpers.js';

test('a worker waiting on the user badges its assistant row until answered', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'need-you-agent', repo: {}, threads: [{ title: 'Ops agent', provider: 'claude' }] },
      { name: 'need-you-worker', repo: {}, threads: [] },
    ],
  });
  const [agentP, workerP] = seed.projects;
  const agent = agentP.threadIds[0];
  const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
  await harness.rpc('SetAssistantProjects', agent, false, [agentP.projectId, workerP.projectId]);
  await setScenario(harness, home, threadToolsScenario({
    name: 'need-you-agent', provider: 'claude', echoUserMessage: true,
    turns: [{ steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Rotate the logs.', title: 'Ops worker', project_id: workerP.projectId } } }], text: 'Started the ops worker.' }],
    afterTurns: 'silent',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: 'need-you-worker', provider: 'claude',
    turns: [{
      steps: [
        { gate: 'ask' },
        { approval: { toolName: 'Bash', input: { command: 'logrotate -f /etc/logrotate.conf' }, onAllow: [{ gate: 'after' }], onDeny: [] } },
      ],
      text: 'Rotated.',
    }],
    afterTurns: 'silent',
  }));

  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  await awaitSidebarSettled(page);
  await harness.startSession(agent);
  await harness.send(agent, 'rotate the logs');
  const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home });
  const worker = spawn.value!.thread_id;
  // Shown in the sidebar, as Show in Sidebar does.
  await harness.rpc('SetThreadPlacement', [worker], 'standalone');
  await awaitTurnCompleted(harness, agent);
  const askGate = await awaitGate(harness, 'ask', workerP.path);
  const row = assistantRow(page, agent);
  await expect(row.getByTestId('thread-row-workers')).toHaveText('1');
  await expect(row.getByTestId('thread-row-workers-need-you')).toHaveCount(0);

  // The agent's next session reads the wake and holds its first frame, so
  // the wake is never confirmed; its exit hands the approval back.
  await harness.stopSession(agent);
  await setScenario(harness, home, {
    ...threadToolsScenario({ name: 'need-you-slow', provider: 'claude', turns: [{ text: 'unreached' }] }),
    startupGate: 'held-start',
  });
  await advanceGate(harness, askGate.mockId, 'ask');
  const wake = await wakeInput(harness, home, 'It asks for approval to use');
  type Pending = { requestId: string; routedTo?: { threadId: string } | null };
  const approvals = async () => (await harness.rpc<{ approvals: Pending[] }>('ListPendingInteractiveRequests', worker)).approvals;
  expect((await approvals())[0].routedTo?.threadId).toBe(agent);
  // Routed to the agent, it does not wait on the user.
  await expect(row.getByTestId('thread-row-workers-need-you')).toHaveCount(0);
  await harness.rpc('HarnessMockCommand', wake.mockId, { type: 'exit', code: 1 });
  await expect.poll(async () => (await approvals())[0]?.routedTo ?? null).toBeNull();

  const badge = row.getByTestId('thread-row-workers-need-you');
  await expect(badge).toHaveText('1');
  await expect(badge).toHaveAttribute('title', '1 worker waiting on you');
  await expect(row.getByTestId('thread-row-workers')).toHaveText('1');
  await page.mouse.move(800, 400);
  await capture(page, 'need-you-assistant-row.png', assistantsSection(page));

  // The user answers: the badge goes, the open worker stays counted.
  await harness.rpc('RespondToApproval', worker, { requestId: (await approvals())[0].requestId, decision: 'allow' });
  const after = await awaitGate(harness, 'after', workerP.path);
  await expect(badge).toHaveCount(0);
  await expect(row.getByTestId('thread-row-workers')).toHaveText('1');

  // The worker ends its turn: nothing is open, and the count goes.
  const done = awaitTurnCompleted(harness, worker);
  await advanceGate(harness, after.mockId, 'after');
  await done;
  await expect(row.getByTestId('thread-row-workers')).toHaveCount(0);
});
