// A worker's question and approval routed to the standing agent that
// spawned it (docs/specs/standing-agents.md, "Wake supervisor"), end to end
// on both mock providers.
//
// Coverage: a worker's question and approval delivered to the standing agent
// that spawned it, answered with thread_reply, and settled by the user
// answering in the worker; and a worker's approval or question handed back
// to the user, and notified, when its agent's session died before taking it
// in.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import { launchHarness, type HarnessMockEventData } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type ScriptedTurn,
  type TurnStep,
} from './thread-tools-helpers.js';
import {
  DATA_NOTICE,
  wakeInput,
  wakeBubble,
  shoot,
  type NotificationSend,
} from './standing-agent-wake-helpers.js';

/**
 * Captures a sidebar row's status dot under the pointer. The dot's label is
 * its title and accessible name, as it is for Working; headless Chromium
 * paints no OS tooltip, so the spec draws the title beside the dot in a box
 * that says it is drawn by the spec. The row's accessibility tree goes to the
 * log.
 */
async function shootHoveredStatus(page: Page, name: string, title: string, label: string): Promise<void> {
  const row = page.getByTestId('thread-row').filter({ hasText: title });
  const dot = row.getByTestId('thread-row-status-dot');
  await expect(dot).toHaveAttribute('title', label);
  await expect(dot).toHaveAttribute('aria-label', label);
  const aria = await row.ariaSnapshot();
  expect(aria).toContain(`status "${label}"`);
  console.log(`${name}: aria snapshot of the "${title}" row\n${aria}`);
  await row.scrollIntoViewIfNeeded();
  await dot.hover();
  const box = (await dot.boundingBox())!;
  await page.evaluate(
    ({ x, y, text }) => {
      const caption = document.createElement('div');
      caption.id = 'spec-drawn-title';
      caption.textContent = text;
      Object.assign(caption.style, {
        position: 'fixed',
        left: `${x}px`,
        top: `${y}px`,
        zIndex: '2147483647',
        background: '#fef3c7',
        color: '#1c1917',
        font: '12px sans-serif',
        padding: '3px 6px',
        border: '1px dashed #b45309',
        borderRadius: '3px',
        pointerEvents: 'none',
        whiteSpace: 'nowrap',
      });
      document.body.append(caption);
    },
    { x: box.x + 8, y: box.y + 14, text: `Drawn by the spec, the dot's title: ${label}` },
  );
  try {
    await shoot(page, name, row);
  } finally {
    await page.evaluate(() => document.getElementById('spec-drawn-title')?.remove());
  }
}

// A worker's question goes to its agent (ruling 19). A thread a standing
// agent spawned asks a question and then an approval; each lands in the
// agent's timeline as a badged wake, the worker reads as waiting on the
// agent rather than on the user, no approval notification is raised for it,
// and the agent's thread_reply answers the prompt in the worker. A third
// prompt the user answers in the worker first refuses the agent's reply.
// An ordinary thread's approval still notifies, so the zero count above
// means something.
const PROMPT_TOKEN = 'thread_reply, token (\\S+), once';

function askStep(header: string, question: string, options: string[], onAllow: TurnStep[]): TurnStep {
  return {
    approval: {
      toolName: 'AskUserQuestion',
      input: {
        questions: [
          {
            id: header.toLowerCase(),
            header,
            question,
            options: options.map((label) => ({ label, description: `${label} environment` })),
            multiSelect: false,
          },
        ],
      },
      onAllow,
    },
  };
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a worker's question and approval go to the standing agent that spawned it`, async ({ page }) => {
    test.setTimeout(180_000);
    const harness = await launchHarness();
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          { name: `asks-agent-${provider}`, repo: {}, threads: [{ title: 'Release agent', provider }] },
          { name: `asks-worker-${provider}`, repo: {}, threads: [] },
          { name: `asks-plain-${provider}`, repo: {}, threads: [{ title: 'Plain asker', provider }] },
        ],
      });
      const [agentP, workerP, plainP] = seed.projects;
      const agent = agentP.threadIds[0];
      const plain = plainP.threadIds[0];
      const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
      // It runs in its home and spawns its worker in the worker's project,
      // so it serves that project too.
      await harness.rpc('SetAssistantProjects', agent, false, [agentP.projectId, workerP.projectId]);

      const answer = (gate: string, args: Record<string, unknown>, text: string): ScriptedTurn => ({
        steps: [
          { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: PROMPT_TOKEN } },
          { gate },
          { call: { tool: 'thread_reply', args: { token: '${TOKEN}', ...args } } },
        ],
        text,
      });
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `asks-agent-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_spawn',
                    // A sidebar row, so the spec reads the worker's status dot.
                    args: { prompt: 'Ship release 4.2.', title: 'Release worker', project_id: workerP.projectId, placement: 'standalone' },
                  },
                },
              ],
              text: 'Started the release worker.',
            },
            answer('answer-question', { text: 'staging, then smoke tests' }, 'Told it staging.'),
            answer('answer-approval', { text: 'A staging deploy is expected.', decision: 'allow' }, 'Allowed the deploy.'),
            answer('answer-late', { text: 'tomorrow' }, 'The user had answered it.'),
          ],
          afterTurns: 'silent',
        }),
      );
      await setScenario(
        harness,
        workerP.path,
        threadToolsScenario({
          name: `asks-worker-${provider}`,
          provider,
          turns: [
            {
              steps: [
                // Each prompt waits for the agent's turn to end, or Codex
                // would steer it into that turn rather than start one.
                { gate: 'then-ask-target' },
                askStep('Target', 'Which environment should release 4.2 go to?', ['staging', 'production'], [
                  { gate: 'then-approve' },
                  {
                    approval: {
                      toolName: 'Bash',
                      input: { command: 'make deploy ENV=staging' },
                      onAllow: [
                        { gate: 'then-ask' },
                        askStep('Window', 'When should the production rollout run?', ['tonight', 'tomorrow'], []),
                      ],
                    },
                  },
                ]),
              ],
              text: 'Release 4.2 is on staging.',
            },
          ],
          afterTurns: 'silent',
        }),
      );
      await setScenario(
        harness,
        plainP.path,
        threadToolsScenario({
          name: `asks-plain-${provider}`,
          provider,
          turns: [{ steps: [{ approval: { toolName: 'Bash', input: { command: 'make clean' } } }], text: 'Cleaned.' }],
          afterTurns: 'silent',
        }),
      );

      const approvalNeeded = (threadId: string) => (data: NotificationSend) =>
        !data.retract && data.kind === 'approval-needed' && data.target.threadId === threadId;

      const agentTurns = () =>
        harness.countEvents<{ threadId?: string }>('provider:turn_completed', (data) => data.threadId === agent);
      // Let the worker raise its next prompt once the agent's turn that
      // answered the last one has ended.
      const nextPromptAfterTurn = async (turnsBefore: number, gate: string) => {
        await expect.poll(agentTurns, { timeout: 30_000 }).toBeGreaterThan(turnsBefore);
        const parked = await awaitGate(harness, gate, workerP.path);
        await advanceGate(harness, parked.mockId, gate);
      };

      await harness.open(page);
      await harness.rpc('StartSession', agent);
      const spawnTurnsBefore = agentTurns();
      await harness.rpc('SendMessage', agent, 'ship release 4.2', null);
      const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home });
      expect(spawn.isError).toBe(false);
      const worker = spawn.value!.thread_id;
      // The worker is the agent's, with no sidebar row; shown there, as
      // Show in Sidebar on its Agents row does, its row carries its state.
      await harness.rpc('SetThreadPlacement', [worker], 'standalone');
      await nextPromptAfterTurn(spawnTurnsBefore, 'then-ask-target');
      const workerDot = page
        .getByTestId('thread-row')
        .filter({ hasText: 'Release worker' })
        .getByTestId('thread-row-status-dot');

      // ---- The question lands in the agent as a wake; the worker waits on it.
      const asked = await wakeInput(harness, home, 'Which environment should release 4.2 go to?');
      const askedText = asked.report.input ?? '';
      expect(askedText).toContain('"Release worker"');
      expect(askedText).toContain(worker);
      expect(askedText).toContain(DATA_NOTICE);
      expect(askedText).toContain('option: staging (staging environment)');
      expect(askedText).toMatch(/thread_reply, token \S+, once: your answer in text\./);
      const questionGate = await awaitGate(harness, 'answer-question', home);
      await expect(workerDot).toHaveAttribute('aria-label', 'Waiting on Release agent');
      await expect(workerDot).toHaveAttribute('data-status', 'running');
      // A page that loads now learns it from the live-activity snapshot.
      await page.reload();
      await expect(workerDot).toHaveAttribute('aria-label', 'Waiting on Release agent');
      await shoot(
        page,
        `${provider}-worker-question.png`,
        await wakeBubble(page, 'Release agent', 'Question · Release worker', 'Which environment should release 4.2 go to?'),
      );
      await shootHoveredStatus(page, `${provider}-worker-waiting-on-hover.png`, 'Release worker', 'Waiting on Release agent');

      // ---- The agent answers; the answer reaches the worker's prompt.
      const questionDecided = harness.waitForEvent<HarnessMockEventData>(
        'harness:mock',
        (ev) => ev.report.kind === 'approval_decided' && ev.cwd === workerP.path,
      );
      const questionReply = awaitToolAnswer<{ accepted: boolean; prompt: boolean; source_thread_id: string }>(harness, {
        tool: 'thread_reply',
        cwd: home,
      });
      let agentTurnsBefore = agentTurns();
      await advanceGate(harness, questionGate.mockId, 'answer-question');
      const replied = await questionReply;
      expect(replied.isError).toBe(false);
      expect(replied.value).toMatchObject({ accepted: true, prompt: true, source_thread_id: worker });
      // The provider received the agent's text as the answer to its question.
      const questionAnswered = (await questionDecided).report;
      expect(questionAnswered.detail).toBe('allow');
      expect(questionAnswered.result).toContain('staging, then smoke tests');
      await nextPromptAfterTurn(agentTurnsBefore, 'then-approve');

      // ---- Then its approval: routed too, and no approval notification.
      const approval = await wakeInput(harness, home, 'It asks for approval to use');
      expect(approval.report.input).toContain('make deploy ENV=staging');
      expect(approval.report.input).toMatch(/decision allow or deny, and your reason in text\./);
      const approvalGate = await awaitGate(harness, 'answer-approval', home);
      await expect(workerDot).toHaveAttribute('aria-label', 'Waiting on Release agent');
      await shoot(
        page,
        `${provider}-worker-approval.png`,
        await wakeBubble(page, 'Release agent', 'Approval · Release worker', 'make deploy ENV=staging'),
      );
      const approvalDecided = harness.waitForEvent<HarnessMockEventData>(
        'harness:mock',
        (ev) => ev.report.kind === 'approval_decided' && ev.cwd === workerP.path,
      );
      const approvalReply = awaitToolAnswer<{ accepted: boolean; prompt: boolean }>(harness, {
        tool: 'thread_reply',
        cwd: home,
      });
      agentTurnsBefore = agentTurns();
      await advanceGate(harness, approvalGate.mockId, 'answer-approval');
      expect((await approvalReply).value).toMatchObject({ accepted: true, prompt: true });
      expect((await approvalDecided).report.detail).toBe('allow');
      await nextPromptAfterTurn(agentTurnsBefore, 'then-ask');

      // ---- A third prompt the user answers in the worker first.
      await wakeInput(harness, home, 'When should the production rollout run?');
      const lateGate = await awaitGate(harness, 'answer-late', home);
      const pending = await harness.rpc<{
        userInputs: { requestId: string; questions: { id: string }[]; routedTo?: { threadId: string; title: string } }[];
      }>('ListPendingInteractiveRequests', worker);
      expect(pending.userInputs).toHaveLength(1);
      expect(pending.userInputs[0].routedTo).toEqual({ threadId: agent, title: 'Release agent' });
      const { requestId, questions } = pending.userInputs[0];
      await harness.rpc('RespondToUserInput', worker, {
        requestId,
        decision: 'accept',
        answers: { [questions[0].id]: ['tonight'] },
      });
      const workerDone = awaitTurnCompleted(harness, worker);
      const lateReply = awaitToolAnswer(harness, { tool: 'thread_reply', cwd: home });
      await advanceGate(harness, lateGate.mockId, 'answer-late');
      const refused = await lateReply;
      expect(refused.isError).toBe(true);
      expect(refused.text).toContain('That prompt is no longer open');
      await workerDone;

      // The worker carried on to the end of its turn.
      await page.getByTestId('thread-row').getByText('Release worker', { exact: true }).click();
      await expect(page.getByText('Release 4.2 is on staging.').first()).toBeVisible();

      // ---- No approval notification ever named the worker; an ordinary
      // thread's approval still raises one.
      const plainNotified = harness.waitForEvent<NotificationSend>('notification:send', approvalNeeded(plain), 60_000);
      await harness.rpc('StartSession', plain);
      await harness.rpc('SendMessage', plain, 'clean up', null);
      await plainNotified;
      expect(harness.countEvents<NotificationSend>('notification:send', approvalNeeded(worker))).toBe(0);
    } finally {
      await harness.close();
    }
  });
}

// A worker's prompt its agent never took in goes back to the user: the
// agent's session dies with the wake unconfirmed, so the worker's approval
// or question is announced again as the user's, notifies, stays out of the
// agent's composer, and withdraws its notification once the user answers.
for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  for (const prompt of ['approval', 'question'] as const) {
  test(`${provider}: a worker's ${prompt} its agent never took in goes back to the user`, async ({ page }) => {
    test.setTimeout(120_000);
    const harness = await launchHarness();
    try {
      await harness.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
      const dataDir = harness.bootstrap.dataDir;
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          { name: `handback-agent-${provider}`, repo: {}, threads: [{ title: 'Ops agent', provider }] },
          { name: `handback-worker-${provider}`, repo: {}, threads: [] },
        ],
      });
      const [agentP, workerP] = seed.projects;
      const agent = agentP.threadIds[0];
      const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
      // It runs in its home and spawns its worker in the worker's project,
      // so it serves that project too.
      await harness.rpc('SetAssistantProjects', agent, false, [agentP.projectId, workerP.projectId]);
      // Codex: the wake starts turn 2, which parks without echoing it.
      await setScenario(
        harness,
        home,
        threadToolsScenario({
          name: `handback-agent-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            {
              steps: [
                {
                  call: {
                    tool: 'thread_spawn',
                    args: { prompt: 'Rotate the logs.', title: 'Ops worker', project_id: workerP.projectId },
                  },
                },
              ],
              text: 'Started the ops worker.',
            },
            { steps: [{ gate: 'hold' }], text: 'unreached', echo: false },
          ],
          afterTurns: 'silent',
        }),
      );
      await setScenario(
        harness,
        workerP.path,
        threadToolsScenario({
          name: `handback-worker-${provider}`,
          provider,
          turns: [
            {
              steps: [
                { gate: 'ask' },
                prompt === 'approval'
                  ? { approval: { toolName: 'Bash', input: { command: 'logrotate -f /etc/logrotate.conf' } } }
                  : askStep('Window', 'When should the logs rotate?', ['now', 'tonight'], []),
              ],
              text: 'Rotated.',
            },
          ],
          afterTurns: 'silent',
        }),
      );

      await harness.open(page);
      await harness.rpc('StartSession', agent);
      await harness.rpc('SendMessage', agent, 'rotate the logs', null);
      const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home });
      const worker = spawn.value!.thread_id;
      // Shown in the sidebar, as Show in Sidebar does, so its row carries its state.
      await harness.rpc('SetThreadPlacement', [worker], 'standalone');
      await awaitTurnCompleted(harness, agent);
      const askGate = await awaitGate(harness, 'ask', workerP.path);

      if (provider === 'claude') {
        // Claude's mock echoes whatever reaches a running session, so the
        // wake goes to a session still starting: it reads the wake, then
        // holds its first frame, and the echo never comes.
        await harness.rpc('StopSession', agent);
        await setScenario(harness, home, {
          ...threadToolsScenario({ name: `handback-slow-${provider}`, provider, turns: [{ text: 'unreached' }] }),
          startupDelayMs: 30_000,
        });
      }
      const handedBack = harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => !data.retract && data.kind === 'approval-needed' && data.target.threadId === worker,
        60_000,
      );
      await advanceGate(harness, askGate.mockId, 'ask');
      const wake = await wakeInput(
        harness,
        home,
        prompt === 'approval' ? 'It asks for approval to use' : 'When should the logs rotate?',
      );
      if (prompt === 'approval') expect(wake.report.input).toContain('logrotate -f /etc/logrotate.conf');
      const mockId = provider === 'claude' ? wake.mockId : (await awaitGate(harness, 'hold', home)).mockId;
      type Pending = { requestId: string; routedTo?: { threadId: string } | null; questions?: { id: string }[] };
      const pendingOf = async () => {
        const all = await harness.rpc<{ approvals: Pending[]; userInputs: Pending[] }>('ListPendingInteractiveRequests', worker);
        return prompt === 'approval' ? all.approvals : all.userInputs;
      };
      const routed = await pendingOf();
      expect(routed[0].routedTo?.threadId).toBe(agent);
      expect(harness.countEvents<NotificationSend>('notification:send', (data) => data.kind === 'approval-needed' && data.target.threadId === worker)).toBe(0);

      await harness.rpc('HarnessMockCommand', mockId, { type: 'exit', code: 1 });
      const notice = await handedBack;
      const back = await pendingOf();
      expect(back).toHaveLength(1);
      expect(back[0].routedTo ?? null).toBeNull();
      expect(notice.id).toBe(`${prompt}:${worker}:${back[0].requestId}`);
      const workerDot = page
        .getByTestId('thread-row')
        .filter({ hasText: 'Ops worker' })
        .getByTestId('thread-row-status-dot');
      await expect(workerDot).toHaveAttribute('aria-label', prompt === 'approval' ? 'Pending Approval' : 'Awaiting Input');
      const conn = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
      try {
        const draft = conn.prepare(`SELECT content FROM thread_drafts WHERE thread_id = ?`).get(agent) as
          | { content: string }
          | undefined;
        expect(draft?.content ?? '').toBe('');
      } finally {
        conn.close();
      }

      // The user answers in the worker, and the notification is withdrawn.
      const withdrawn = harness.waitForEvent<NotificationSend>(
        'notification:send',
        (data) => Boolean(data.retract) && data.id === notice.id,
        60_000,
      );
      if (prompt === 'approval') {
        await harness.rpc('RespondToApproval', worker, { requestId: back[0].requestId, decision: 'allow' });
      } else {
        await harness.rpc('RespondToUserInput', worker, {
          requestId: back[0].requestId,
          decision: 'accept',
          answers: { [back[0].questions![0].id]: ['tonight'] },
        });
      }
      await withdrawn;
      await awaitTurnCompleted(harness, worker);
    } finally {
      await harness.close();
    }
  });
  }
}
