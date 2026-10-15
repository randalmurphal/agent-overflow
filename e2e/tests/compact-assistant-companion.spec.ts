// The assistant's agent-thread companion pane under the compact layout
// (docs/specs/standing-agents.md, "Agents segment"): a tap on an Agents
// tray row reveals the worker's companion on the phone with its header and
// Open as Pane in view, and its own Stop interrupts the worker.
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { agentRow, mockInterrupt, openAgents } from './agents-tray-helpers.js';
import { awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

const LEAD = 'Release lead';
const WORKER = 'Changelog worker';

function companion(page: Page) {
  return page.locator('section[data-pane-kind="agent-thread"]');
}

test('compact: a tray row reveals the worker in a companion that fits the phone', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'compact-assistant-companion', repo: {}, threads: [{ title: LEAD, assistant: true }] },
      { name: 'compact-assistant-companion-worker', repo: {}, threads: [] },
    ],
  });
  const [leadProject, workerProject] = seed.projects;
  const lead = leadProject.threadIds[0];
  await setScenario(harness, leadProject.path, threadToolsScenario({
    name: 'compact-assistant-companion-lead', provider: 'claude', echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Write the changelog.', title: WORKER, project_id: workerProject.projectId, notify: true } } }], text: 'Started the changelog.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerProject.path, threadToolsScenario({
    name: 'compact-assistant-companion-worker', provider: 'claude', echoUserMessage: true,
    turns: [{ steps: [{ gate: 'worker-hold' }], text: 'Written.' }, { text: 'Noted.' }],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'Get the changelog written.', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadProject.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  await awaitTurnCompleted(harness, lead);

  await harness.open(page);
  await openAgents(page, LEAD);
  await agentRow(page, spawn.value!.token).getByTestId('agent-tray-row-open').tap();
  await expect(companion(page).locator(`[data-ui-surface="chat"][data-thread-id="${spawn.value!.thread_id}"]`)).toBeVisible();
  const title = companion(page).getByTestId('chat-header-title');
  await expect(title).toHaveText(WORKER);
  await expect(title).toBeInViewport();
  const openAsPane = companion(page).getByTestId('agent-thread-open-as-pane');
  await expect(openAsPane).toBeInViewport({ ratio: 1 });
  const interrupt = companion(page).getByTestId('composer-interrupt');
  await expect(interrupt).toBeInViewport({ ratio: 1 });
  await captureAgentMessage(page, 'compact-assistant-companion.png', companion(page).getByTestId('chat-header-title-row'), interrupt);

  const stopped = mockInterrupt(harness, workerProject.path);
  await interrupt.tap();
  await stopped;
  await expect(companion(page).getByTestId('composer-send')).toBeVisible();
});
