// Shared setup for the activity rail's Agents segment (agents-tray.spec.ts
// and compact-agents-tray.spec.ts): a lead thread that spawns a child and
// sends to a peer, a child that spawns a grandchild and starts a background
// shell, a grandchild held on an approval, the row locators, and reads of
// the request ledger.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import {
  awaitToolAnswer,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type TurnStep,
} from './thread-tools-helpers.js';
import {
  backgroundTasksChangedLine,
  taskStartedLine,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';

export const LEAD = 'Release lead';
export const CHILD = 'Child worker';
export const GRAND = 'Grand worker';
export const PEER = 'Peer desk';

/** The tool name each provider gives the grandchild's command approval. */
export function approvalTool(provider: ProviderName): string {
  return provider === 'claude' ? 'Bash' : 'command';
}

/** The provider frames of one background shell the thread starts. */
export function backgroundShellSteps(provider: ProviderName): TurnStep[] {
  if (provider === 'claude') {
    const shell = { task_id: 'task-shell', task_type: 'local_bash', description: 'tail the build log' };
    return [{
      emitLines: [
        toolUseLine('msg-shell', 'tu-shell', 'Bash', { command: 'tail -f build.log', run_in_background: true }),
        taskStartedLine('task-shell', 'tu-shell', shell.description, { taskType: 'local_bash' }),
        toolResultLine('tu-shell', 'Command running in background with ID: task-shell.', {
          toolUseResult: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'task-shell' },
        }),
        backgroundTasksChangedLine([shell]),
      ],
    }];
  }
  // A unified-exec PTY the model then polls with empty input: the typed
  // signal that makes it the tray's stoppable Codex terminal.
  const frame = (method: string, fields: object) => JSON.stringify({
    jsonrpc: '2.0', method, params: { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...fields },
  });
  return [{
    emitLines: [
      frame('item/started', {
        item: { id: 'shell-1', type: 'commandExecution', source: 'unifiedExecStartup', processId: '77', status: 'inProgress', command: 'tail -f build.log' },
      }),
      frame('item/commandExecution/terminalInteraction', { itemId: 'shell-1', processId: '77', stdin: '' }),
    ],
  }];
}

/** What a Codex session answers to a terminal's stop: the scenario's own. */
export function codexTerminateAnswer(provider: ProviderName): Record<string, unknown> {
  return provider === 'codex'
    ? { codex: { responses: {
      'thread/backgroundTerminals/terminate': '{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"terminated":true}}',
      'thread/backgroundTerminals/clean': '{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}',
    } } }
    : {};
}

export interface AgentsTree {
  lead: string;
  leadPath: string;
  childPath: string;
  grandPath: string;
  peer: string;
  peerPath: string;
}

/**
 * Seeds the lead and its peer and installs every workspace's script. The lead
 * spawns the child and sends to the peer, both with notify; the child spawns
 * the grandchild, starts a background shell and holds; the grandchild asks
 * for an approval and then holds; the peer holds until released.
 */
export async function seedAgentsTree(
  harness: HarnessApp,
  provider: ProviderName,
  tag: string,
  /** The lead also starts a background shell and holds its turn open. */
  opts: { leadHolds?: boolean } = {},
): Promise<AgentsTree> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
      { name: `${tag}-child`, repo: {}, threads: [] },
      { name: `${tag}-grand`, repo: {}, threads: [] },
      { name: `${tag}-peer`, repo: {}, threads: [{ title: PEER, provider }] },
    ],
  });
  const [leadProject, childProject, grandProject, peerProject] = seed.projects;
  const tree: AgentsTree = {
    lead: leadProject.threadIds[0], leadPath: leadProject.path,
    childPath: childProject.path, grandPath: grandProject.path,
    peer: peerProject.threadIds[0], peerPath: peerProject.path,
  };
  await setScenario(harness, tree.leadPath, {
    ...threadToolsScenario({
      name: `${tag}-lead`, provider, echoUserMessage: true,
      turns: [
        { steps: [
          { call: { tool: 'thread_spawn', args: { prompt: 'Build the release.', title: CHILD, project_id: childProject.projectId, provider, notify: true } } },
          { call: { tool: 'thread_send', args: { thread_id: tree.peer, message: 'Check the changelog.', notify: true } } },
          ...(opts.leadHolds ? [...backgroundShellSteps(provider), { gate: 'lead-hold' }] : []),
        ], text: 'Started the release work.' },
        { text: 'Noted.' },
      ],
    }),
    ...codexTerminateAnswer(provider),
  });
  await setScenario(harness, tree.childPath, {
    ...threadToolsScenario({
      name: `${tag}-child`, provider, echoUserMessage: true,
      turns: [
        { steps: [
          { call: { tool: 'thread_spawn', args: { prompt: 'Package the build.', title: GRAND, project_id: grandProject.projectId, provider, notify: true } } },
          ...backgroundShellSteps(provider),
          { gate: 'child-hold' },
        ], text: 'Built the release.' },
        { text: 'Noted.' },
      ],
    }),
    ...codexTerminateAnswer(provider),
  });
  await setScenario(harness, tree.grandPath, threadToolsScenario({
    name: `${tag}-grand`, provider, echoUserMessage: true,
    turns: [
      { steps: [
        { approval: { toolName: 'Bash', input: { command: 'make package' }, toolUseId: 'tu-package', onAllow: [{ gate: 'grand-hold' }], onDeny: [] } },
      ], text: 'Packaged.' },
      { text: 'Noted.' },
    ],
  }));
  await setScenario(harness, tree.peerPath, threadToolsScenario({
    name: `${tag}-peer`, provider, echoUserMessage: true,
    turns: [{ steps: [{ gate: 'peer-hold' }], text: 'The changelog is fine.' }, { text: 'Noted.' }],
  }));
  return tree;
}

export interface StartedTree {
  childToken: string;
  child: string;
  grandToken: string;
  grand: string;
  sendToken: string;
}

/** Starts the lead's turn and waits until every request is made. */
export async function startTree(harness: HarnessApp, tree: AgentsTree): Promise<StartedTree> {
  await harness.rpc('StartSession', tree.lead);
  await harness.rpc('SendMessage', tree.lead, 'run the release', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: tree.leadPath, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const send = await awaitToolAnswer<{ token: string }>(harness, { tool: 'thread_send', cwd: tree.leadPath, timeoutMs: 60_000 });
  expect(send.isError, send.text).toBe(false);
  const nested = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: tree.childPath, timeoutMs: 60_000 });
  expect(nested.isError, nested.text).toBe(false);
  return {
    childToken: spawn.value!.token, child: spawn.value!.thread_id,
    grandToken: nested.value!.token, grand: nested.value!.thread_id,
    sendToken: send.value!.token,
  };
}

/** The Agents body of the visible pane. */
export function agentsBody(page: Page): Locator {
  return page.getByTestId('activity-rail-agents-body').filter({ visible: true });
}

export function agentRow(page: Page, token: string): Locator {
  return agentsBody(page).locator(`[data-testid="agent-tray-row"][data-token="${token}"]`);
}

/** Opens the thread and its Agents segment. */
export async function openAgents(page: Page, title: string): Promise<Locator> {
  await page.getByTestId('thread-row-title').getByText(title, { exact: true }).first().click();
  const toggle = page.getByTestId('activity-rail-agents-toggle').filter({ visible: true });
  await expect(toggle).toBeVisible({ timeout: 30_000 });
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(agentsBody(page)).toBeVisible();
  return toggle;
}

export interface LedgerRow {
  state: string;
  answer: string;
  notify: number;
}

/** A request as the ledger holds it. */
export function ledgerRow(harness: HarnessApp, token: string): LedgerRow | undefined {
  const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return db.prepare('SELECT state, CAST(answer AS TEXT) AS answer, notify FROM thread_requests WHERE token = ?').get(token) as LedgerRow | undefined;
  } finally {
    db.close();
  }
}

/** The next input the mock in `cwd` takes whose text includes every part. */
export function mockInput(harness: HarnessApp, cwd: string, parts: string[], timeoutMs = 60_000) {
  return harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && parts.every((part) => (ev.report.input ?? '').includes(part)),
    timeoutMs,
  );
}

/** The next interrupt the mock in `cwd` reports. */
export function mockInterrupt(harness: HarnessApp, cwd: string, timeoutMs = 60_000) {
  return harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === cwd,
    timeoutMs,
  );
}
