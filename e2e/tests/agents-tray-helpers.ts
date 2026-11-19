// Shared setup for the activity rail's Agents segment (agents-tray.spec.ts
// and compact-agents-tray.spec.ts): a lead thread that spawns a child and
// sends to a peer, a child that spawns a grandchild and starts a background
// shell, a grandchild held on an approval, the row locators, and reads of
// the request ledger, and the Past list's locators (agents-tray-past.spec.ts
// too). seedLeadWithWorker is the companion pane specs' one
// assistant with one held worker. seedPastWithAsk, seedAssistantWithHelper
// and failAgentReads set up the Past, helper and failed-read cases the
// desktop and compact specs share.
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import {
  assistantHome,
  awaitToolAnswer,
  awaitTurnCompleted,
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
import { methodIdByName } from './offhost-helpers.js';

export const LEAD = 'Release lead';
export const CHILD = 'Child worker';
export const GRAND = 'Grand worker';
export const PEER = 'Peer desk';

// The lead's first answer is longer than the pane, so a wake row after it
// starts out of view.
const LEAD_REPORT = ['Started the release work.', ...Array.from({ length: 60 }, (_, i) => `Release step ${i + 1} is queued.`)].join('\n\n');

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
        ], text: LEAD_REPORT },
        { text: 'Noted.' },
      ],
    }),
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

/** What seedLeadWithWorker made. */
export interface LeadWithWorker {
  /** The assistant's current thread, which runs in its home. */
  lead: string;
  home: string;
  /** The worker's project checkout, where its mock runs. */
  workerPath: string;
  /** The spawn's request token: the worker's Agents row. */
  token: string;
  worker: string;
}

/**
 * An assistant titled `lead`, serving its own project (`<tag>-lead`) and
 * the worker's (`<tag>-worker`), that has spawned one worker titled
 * `worker` there and finished its turn. The worker's first turn holds on
 * the `worker-hold` gate, so its Agents row stays to click; any later turn
 * of either answers 'Noted.'.
 */
export async function seedLeadWithWorker(
  harness: HarnessApp,
  opts: { tag: string; provider: ProviderName; lead: string; worker: string },
): Promise<LeadWithWorker> {
  const { tag, provider } = opts;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: opts.lead, provider, assistant: true }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadProject, workerProject] = seed.projects;
  const lead = leadProject.threadIds[0];
  const home = await assistantHome(harness, lead);
  await harness.rpc('SetAssistantProjects', lead, false, [leadProject.projectId, workerProject.projectId]);
  await setScenario(harness, home, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Write the changelog.', title: opts.worker, project_id: workerProject.projectId, provider, notify: true } } }], text: 'Started the changelog.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerProject.path, threadToolsScenario({
    name: `${tag}-worker`, provider, echoUserMessage: true,
    turns: [{ steps: [{ gate: 'worker-hold' }], text: 'Written.' }, { text: 'Noted.' }],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'Get the changelog written.', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  await awaitTurnCompleted(harness, lead);
  return { lead, home, workerPath: workerProject.path, token: spawn.value!.token, worker: spawn.value!.thread_id };
}

/** The Agents body of the visible pane. */
export function agentsBody(page: Page): Locator {
  return page.getByTestId('activity-rail-agents-body').filter({ visible: true });
}

/** A row, open or past. */
export function agentRow(page: Page, token: string): Locator {
  return agentsBody(page).locator(`[data-testid="agent-tray-row"][data-token="${token}"]`);
}

/** A row in the open list, not under Past. */
export function openAgentRow(page: Page, token: string): Locator {
  return agentsBody(page).locator(`[data-testid="agent-tray-row"][data-token="${token}"]:not([data-past])`);
}

/** The Past row's toggle and the list it opens. */
export function pastToggle(page: Page): Locator {
  return agentsBody(page).getByTestId('activity-rail-agents-past-toggle');
}

export function pastList(page: Page): Locator {
  return agentsBody(page).getByTestId('activity-rail-agents-past');
}

export function pastRow(page: Page, token: string): Locator {
  return pastList(page).locator(`[data-testid="agent-tray-row"][data-token="${token}"]`);
}

/** Expands Past and waits for its first page. */
export async function openPast(page: Page): Promise<Locator> {
  const toggle = pastToggle(page);
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(pastList(page).getByTestId('agent-tray-row').first()).toBeVisible({ timeout: 30_000 });
  return pastList(page);
}

/** The agent thread companion pane. */
export function companionPane(page: Page): Locator {
  return page.locator('section[data-pane-kind="agent-thread"]');
}

/**
 * An ask's copy offers only what it can do: its message has no edit or
 * fork, and its composer no pickers and no attach; Stop stays.
 */
export async function expectAskCopyReadOnly(copyPane: Locator): Promise<void> {
  await expect(copyPane.getByTestId('user-message-bubble').first()).toBeVisible({ timeout: 30_000 });
  await expect(copyPane.getByLabel('Fork from this message')).toHaveCount(0);
  await expect(copyPane.getByLabel('Edit message and resend from here')).toHaveCount(0);
  for (const id of ['composer-model-menu-trigger', 'composer-effort-trigger', 'composer-access-toggle', 'composer-mcp-trigger', 'composer-attach']) {
    await expect(copyPane.getByTestId(id), id).toHaveCount(0);
  }
  // No branch or worktree picker, the desktop strip's or the compact header's:
  // either acts on the workspace the copy shares.
  await expect(copyPane.locator(['branch-picker-trigger', 'env-picker-trigger', 'chat-header-branch', 'chat-header-worktree']
    .map((id) => `[data-testid="${id}"]`).join(', '))).toHaveCount(0);
  await expect(copyPane.getByTestId('composer-interrupt')).toBeVisible();
}

export interface LedgerRequest {
  token: string;
  kind: string;
  targetThreadId: string;
  state: string;
}

/** The requests a thread made, oldest first, as the ledger holds them. */
export function ledgerRequests(harness: HarnessApp, caller: string): LedgerRequest[] {
  const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return db.prepare(`SELECT token, kind, COALESCE(target_thread_id, '') AS targetThreadId, state
      FROM thread_requests WHERE caller_thread_id = ? ORDER BY created_at, rowid`).all(caller) as unknown as LedgerRequest[];
  } finally {
    db.close();
  }
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

/** The send ids held in a thread's durable queue, in queue order. */
export function heldSendIds(harness: HarnessApp, threadId: string): string[] {
  const db = new DatabaseSync(join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return (db.prepare('SELECT send_id FROM flush_queue_items WHERE thread_id = ? ORDER BY enqueued_at, rowid').all(threadId) as { send_id: string }[])
      .map((row) => row.send_id);
  } finally {
    db.close();
  }
}

/** The next interrupt the mock in `cwd` reports. */
export function mockInterrupt(harness: HarnessApp, cwd: string, timeoutMs = 60_000) {
  return harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'turn_interrupted' && ev.cwd === cwd,
    timeoutMs,
  );
}

export const ASKED = 'Asked desk';
export const ASK_ANSWER = 'The changelog covers every merged change.';
export const HELPER = 'Notes helper';

/** How many rows a thread's Past holds, as the backend counts them. */
export function pastTotal(harness: HarnessApp, thread: string): Promise<number> {
  return harness.rpc<{ total: number }>('ListThreadPastAgentRows', thread, { settledAt: 0, token: '' }, 1).then((page) => page.total);
}

/** What seedPastWithAsk made: a lead whose Past holds a finished worker and an answered ask. */
export interface PastWithAsk {
  lead: string;
  workerToken: string;
  askToken: string;
  /** The ask's copy, while it exists. */
  copyThreadId: string;
  /** The asked thread's workspace, whose scenario the copy's turn reads. */
  askedPath: string;
}

/**
 * A lead titled LEAD that spawned a worker, which finished, and then asked
 * a thread titled ASKED, whose copy answered ASK_ANSWER: two Past rows,
 * the ask settled last. Resolves once both are in Past. With `holdAsk` the
 * copy's turn waits on the `ask-hold` gate in `askedPath` instead, and the
 * call resolves with the worker alone in Past and the ask running.
 */
export async function seedPastWithAsk(harness: HarnessApp, provider: ProviderName, tag: string, holdAsk = false): Promise<PastWithAsk> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
      { name: `${tag}-asked`, repo: {}, threads: [{ title: ASKED, provider }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadP, askedP, workerP] = seed.projects;
  const lead = leadP.threadIds[0];
  const asked = askedP.threadIds[0];
  // The asked thread runs a turn first, so its copy has a session to be
  // cut from; the copy's session reads the scenario set after it.
  await setScenario(harness, askedP.path, threadToolsScenario({ name: `${tag}-asked`, provider, turns: [{ text: 'Read the changelog.' }] }));
  await harness.rpc('StartSession', asked);
  await harness.rpc('SendMessage', asked, 'read the changelog', null);
  await awaitTurnCompleted(harness, asked);
  await setScenario(harness, askedP.path, threadToolsScenario({
    name: `${tag}-asked-copy`, provider, turns: [holdAsk ? { steps: [{ gate: 'ask-hold' }], text: ASK_ANSWER } : { text: ASK_ANSWER }],
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({ name: `${tag}-worker`, provider, turns: [{ text: 'Done.' }], afterTurns: 'repeatLast' }));
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { steps: [
        { call: { tool: 'thread_spawn', args: { prompt: 'Write the changelog.', title: 'Changelog worker', project_id: workerP.projectId, provider, notify: true } } },
        { call: { tool: 'thread_ask', args: { thread_id: asked, question: 'Is the changelog complete?', wait_seconds: 0, notify: true } } },
      ], text: 'Started it and asked.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'write the changelog and ask', null);
  await awaitTurnCompleted(harness, lead);
  const [spawn, ask] = ledgerRequests(harness, lead);
  expect([spawn?.kind, ask?.kind]).toEqual(['spawn', 'ask']);
  await expect.poll(() => pastTotal(harness, lead), { timeout: 90_000 }).toBe(holdAsk ? 1 : 2);
  return { lead, workerToken: spawn.token, askToken: ask.token, copyThreadId: ask.targetThreadId, askedPath: askedP.path };
}

/** What seedAssistantWithHelper made. */
export interface AssistantWithHelper {
  lead: string;
  home: string;
  token: string;
  helper: string;
}

/**
 * An assistant titled LEAD that spawned a helper titled HELPER (a spawn
 * naming no project: outside every project, always the assistant's
 * worker). The helper's turn holds on the `helper-hold` gate in the
 * assistant's home until the spec releases it.
 */
export async function seedAssistantWithHelper(harness: HarnessApp, provider: ProviderName, tag: string): Promise<AssistantWithHelper> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider, assistant: true }] }],
  });
  const lead = seed.projects[0].threadIds[0];
  const home = await assistantHome(harness, lead);
  // The helper runs in the assistant's home too. A session reads its
  // scenario when it starts, so the assistant's session starts on its own
  // before the helper's scenario takes the home.
  await setScenario(harness, home, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { text: 'Ready.' },
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Keep the release notes.', title: HELPER, provider, notify: true } } }], text: 'Started the notes.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'get ready', null);
  await awaitTurnCompleted(harness, lead);
  await setScenario(harness, home, threadToolsScenario({
    name: `${tag}-helper`, provider, echoUserMessage: true,
    turns: [{ steps: [{ gate: 'helper-hold' }], text: 'Notes kept.' }],
    afterTurns: 'silent',
  }));
  await harness.rpc('SendMessage', lead, 'keep the release notes', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: home, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  return { lead, home, token: spawn.value!.token, helper: spawn.value!.thread_id };
}

/**
 * Answers the page's reads of the Agents segment (its open list and its
 * Past) with a method error while `failing` is set, as a backend that
 * cannot read its ledger answers them. Install before the page opens.
 */
export async function failAgentReads(page: Page): Promise<{ failing: boolean }> {
  const control = { failing: true };
  const methods = new Set([methodIdByName('ListThreadAgentRows'), methodIdByName('ListThreadPastAgentRows')]);
  await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number };
      if (control.failing && frame.type === 'rpc' && frame.id && frame.methodId !== undefined && methods.has(frame.methodId)) {
        socket.send(JSON.stringify({ type: 'rpc', id: frame.id, error: { code: 'method_error', message: 'the ledger could not be read' } }));
        return;
      }
      server.send(message);
    });
    server.onMessage((message) => socket.send(message));
  });
  return control;
}
