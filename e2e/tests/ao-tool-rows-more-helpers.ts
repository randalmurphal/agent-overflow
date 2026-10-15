// The AO tool rows ao-tool-rows-helpers.ts does not drive: one lead thread
// calls every other thread tool (ask, item, update, group, remind, watch,
// cancel), the workflow run and memory tools, the browser tools on a local
// page, and the remote tools on a paired computer. The target answers the
// lead's send with thread_reply, so its own timeline holds a reply row.
// Shared by ao-tool-rows-more.spec.ts (desktop) and
// compact-ao-tool-rows-more.spec.ts (compact).

import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import {
  FOOTER_TOKEN_PATTERN,
  TOOL_ROW_ID_PATTERN,
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import {
  START_RUN_PATTERN,
  WORKFLOW_TOOLS_SERVER,
  answerPhasesDone,
  doneFlowYaml,
  otherProvider,
} from './workflows-agent-tools-helpers.js';
import { expectRowTextWhole, shootRow } from './ao-tool-rows-helpers.js';

export const MORE_LEAD_TITLE = 'Lead thread';
export const MORE_TARGET_TITLE = 'Parser work';
export const MORE_SPAWN_TITLE = 'Crash triage';

const BROWSER = 'ao-browser-tools';
const REMOTE = 'ao-remote-tools';

/** Every row the lead's turn makes, in order: its tool and server. */
export const MORE_ROWS = [
  'thread_spawn', 'thread_send', 'thread_ask', 'thread_show', 'thread_item', 'thread_update', 'thread_group',
  'thread_remind', 'thread_watch', 'thread_cancel', 'thread_status',
  'workflow_list', 'workflow_schema', 'run_start', 'run_list', 'run_amend', 'run_guide', 'run_watch', 'memory_add', 'memory_list',
  'browser_open_file', 'browser_snapshot', 'browser_evaluate', 'browser_pages', 'browser_screenshot',
  'remote_computers', 'remote_run',
] as const;

/**
 * Pairs `remote` with `app` as Build box, which agents may use, and returns
 * its computer id. Both computers are named so no capture shows the host's
 * own name.
 */
async function pairBuildBox(app: HarnessApp, remote: HarnessApp): Promise<string> {
  await app.rpc('SetDeviceName', 'Agent computer');
  await remote.rpc('SetDeviceName', 'Other computer');
  const invite = await remote.rpc<{ url: string; linkId: string }>('MintDevicePairing', 'desktop', 'full');
  const attachment = await app.rpc<{ id: string }>('AddBackend', invite.url);
  await remote.rpc('ConfirmDevicePairing', invite.linkId);
  await app.rpc('RenameBackend', attachment.id, 'Build box');
  await expect
    .poll(async () => {
      try {
        await app.rpc('SetAgentComputerEnabled', attachment.id, true);
        return 'enabled';
      } catch (error) {
        return String(error);
      }
    }, { timeout: 30_000 })
    .toBe('enabled');
  return attachment.id;
}

export interface MoreAoToolRowsRun {
  leadThreadId: string;
  targetThreadId: string;
}

/** Seeds both computers, scripts the lead's turn and drives it until its last call answers. */
export async function driveMoreAoToolRows(app: HarnessApp, remote: HarnessApp, provider: ProviderName, tag: string): Promise<MoreAoToolRowsRun> {
  await app.rpc('UpdateSettings', { notifyQuietWhen: 'never' });
  const computer = await pairBuildBox(app, remote);
  const remoteSeed = await remote.rpc<SeedResult>('HarnessSeed', { projects: [{ name: `ao-more-build-${tag}`, repo: {} }] });
  const remoteProject = remoteSeed.projects[0].projectId;

  const bulk = Array.from({ length: 120 }, (_, i) => `line ${i + 1}: parsing token ${i + 1}`);
  bulk[60] = 'NEEDLE panic: the parser overran the escape';
  const seed = await app.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `ao-more-lead-${tag}`,
        repo: { commits: [{ message: 'page', files: { 'status.html': '<title>Release dashboard</title><h1>All green</h1><p>Build 812 passed.</p>' } }] },
        threads: [{ title: MORE_LEAD_TITLE, provider }],
        workflows: {
          definitions: [
            { name: 'digest', yaml: doneFlowYaml('digest', otherProvider(provider)), prompts: { 'digest.md': 'Do the work and report the outcome.' } },
          ],
        },
      },
      {
        name: `ao-more-target-${tag}`,
        repo: {},
        threads: [
          {
            title: MORE_TARGET_TITLE,
            provider,
            turns: [
              {
                userText: 'investigate the parser crash',
                items: [
                  { kind: 'assistant_text', summary: 'The crash is in scanIdent when the input ends mid-escape.' },
                  { kind: 'tool_call', toolName: 'Bash', summary: 'go test ./internal/parse', payload: { kind: 'tool_call_result', data: bulk.join('\n') } },
                ],
              },
            ],
          },
        ],
      },
    ],
  });
  const lead = seed.projects[0];
  const target = seed.projects[1];
  const leadThreadId = lead.threadIds[0];
  const targetThreadId = target.threadIds[0];

  await answerPhasesDone(app, otherProvider(provider));
  // Every message an agent sends here carries a request footer: the target
  // answers it with thread_reply. The ask's hidden copy runs here too.
  await setScenario(app, target.path, threadToolsScenario({
    name: `ao-more-target-${tag}`,
    provider,
    turns: [{
      steps: [
        { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
        { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: 'Rebased onto main; the parser tests pass.' } } },
      ],
      text: 'Replied.',
    }],
  }));
  await setScenario(app, lead.path, threadToolsScenario({
    name: `ao-more-lead-${tag}`,
    provider,
    echoUserMessage: true,
    turns: [
      // A standing agent may not spawn, so the spawn runs before the lead
      // is promoted, in a turn of its own.
      {
        steps: [
          { call: { tool: 'thread_spawn', args: { prompt: 'Triage the crash report.', title: MORE_SPAWN_TITLE, project_id: target.projectId, group: 'Bugs', wait_seconds: 30 } } },
        ],
        text: 'Spawned.',
      },
      {
        steps: [
          { call: { tool: 'thread_send', args: { thread_id: targetThreadId, message: 'Rebase onto main.', notify: true } } },
          { call: { tool: 'thread_ask', args: { thread_id: targetThreadId, question: 'What changed in the parser?', wait_seconds: 30 } } },
          { call: { tool: 'thread_show', args: { thread_id: targetThreadId, window: 'tail', turns: 5, include: ['tool_calls'] } } },
          { capture: { var: 'ITEM', from: '${MCP_RESULT}', pattern: TOOL_ROW_ID_PATTERN } },
          { call: { tool: 'thread_item', args: { thread_id: targetThreadId, item_id: '${ITEM}', query: 'NEEDLE' } } },
          { call: { tool: 'thread_update', args: { thread_ids: [targetThreadId], pin: 'front' } } },
          { call: { tool: 'thread_group', args: { group: 'Bugs', project_id: target.projectId, rename: 'Bug sweep' } } },
          { call: { tool: 'thread_remind', args: { after_seconds: 3600, note: 'Check the deploy.' } } },
          { call: { tool: 'thread_watch', args: { label: 'ci', command: 'echo green', every_seconds: 3600 } } },
          { call: { tool: 'thread_cancel', args: { label: 'ci' } } },
          { call: { tool: 'thread_status', args: {} } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'workflow_list', args: {} } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'workflow_schema', args: {} } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_start', args: { workflow_id: 'digest', goal: 'Weekly summary', seeds: { goal: 'Weekly summary' } } } },
          { capture: { var: 'RUN', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_list', args: {} } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_amend', args: { run_id: '${RUN}', seeds: { goal: 'Monthly summary' } } } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_guide', args: { run_id: '${RUN}', text: 'Keep it short.' } } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_watch', args: { run_id: '${RUN}', wait_seconds: 5 } } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'memory_add', args: { kind: 'learning', text: 'The digest needs the schema first.', run_id: '${RUN}' } } },
          { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'memory_list', args: { run_id: '${RUN}' } } },
          { call: { server: BROWSER, tool: 'browser_open_file', args: { path: join(lead.path, 'status.html') } } },
          { call: { server: BROWSER, tool: 'browser_snapshot', args: {} } },
          { call: { server: BROWSER, tool: 'browser_evaluate', args: { expression: 'document.title' } } },
          { call: { server: BROWSER, tool: 'browser_pages', args: {} } },
          { call: { server: BROWSER, tool: 'browser_screenshot', args: {} } },
          { call: { server: REMOTE, tool: 'remote_computers', args: {} } },
          {
            call: {
              server: REMOTE,
              tool: 'remote_run',
              args: {
                computer_id: computer,
                project_id: remoteProject,
                request_id: randomUUID(),
                label: 'nightly build',
                argv: ['sh', '-c', 'echo build 812 finished: green'],
                wait_seconds: 30,
              },
            },
          },
        ],
        text: 'Done.',
      },
      { text: 'Read the answer.' },
    ],
  }));

  await app.rpc('StartSession', leadThreadId);
  const spawnTurn = awaitTurnCompleted(app, leadThreadId);
  await app.rpc('SendMessage', leadThreadId, 'spawn the crash triage', null);
  // The spawned thread answers inside the call's wait, so no wake will
  // take the next turn.
  const spawn = await awaitToolAnswer<{ outcome: string }>(app, { tool: 'thread_spawn', cwd: lead.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  expect(spawn.value?.outcome, spawn.text).toBe('settled');
  await spawnTurn;
  // A watch runs a script from a standing agent's home, so the lead is one.
  await app.rpc('PromoteStandingAgent', leadThreadId);
  await app.rpc('SendMessage', leadThreadId, 'run every tool once', null);
  const last = await app.awaitMcpResult({ server: REMOTE, tool: 'remote_run', cwd: lead.path, timeoutMs: 120_000 });
  expect(last.report.isError, last.report.result).toBeFalsy();
  return { leadThreadId, targetThreadId };
}

/** Opens a thread by title with every activity run expanded. */
export async function openThread(app: HarnessApp, page: Page, title: string): Promise<void> {
  await app.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row-title').filter({ hasText: title }).first().click();
  const runs = page.getByTestId('activity-run');
  await expect(runs.first()).toBeAttached({ timeout: 30_000 });
  for (let index = 0; index < (await runs.count()); index++) {
    const run = runs.nth(index);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  }
}

/** The tool row for one AO tool, by the tool name its label carries. */
export function aoRow(page: Page, tool: string): Locator {
  return page.getByTestId('tool-call-card').filter({ has: page.locator(`[data-testid="tool-call-card-label"][title="${tool}"]`) });
}

/**
 * Brings a row on screen, screenshots it collapsed, opens it, checks its
 * body reads as decoded text, not JSON, and screenshots it open. Rows far
 * from the reader are windowed, so it scrolls up until the row mounts:
 * callers read rows from the newest up.
 */
export async function readRow(page: Page, tool: string, shot: (name: string) => string): Promise<Locator> {
  const row = aoRow(page, tool).first();
  const scroller = page.getByTestId('message-timeline-scroll');
  await expect(async () => {
    if ((await row.count()) === 0) {
      // Runs that mount as the reader scrolls open too.
      const runs = page.locator('[data-testid="activity-run"][data-collapsed="true"]');
      for (let index = (await runs.count()) - 1; index >= 0; index--) await runs.nth(index).getByTestId('activity-run-header').click();
      await scroller.hover();
      await page.mouse.wheel(0, -600);
    }
    await expect(row).toHaveCount(1, { timeout: 500 });
  }).toPass({ timeout: 30_000 });
  await shootRow(row, shot(`${tool}-collapsed`));
  // Where the row sits on screen, net of its activity run's own clip: for a
  // reader above the bottom, the timeline must not move a row they open (its
  // reading anchor, its windowing). A run's clip that follows its own end is
  // the run's, and a reader at the bottom follows the growth.
  const placed = () => row.evaluate((el) => {
    const clip = el.closest<HTMLElement>('[data-testid="activity-run-clip"]');
    return el.getBoundingClientRect().top + (clip?.scrollTop ?? 0);
  });
  const timeline = () => page.getByTestId('message-timeline-scroll').evaluate((el) => ({
    top: Math.round(el.scrollTop), aboveBottom: el.scrollHeight - el.clientHeight - el.scrollTop > 1,
  }));
  const placedBefore = await placed();
  const timelineBefore = await timeline();
  await row.getByTestId('tool-call-card-toggle').click();
  await expect(row.getByTestId('tool-call-card-toggle')).toHaveAttribute('aria-expanded', 'true');
  const body = row.getByTestId('tool-call-card-body');
  await expect(body).toBeVisible();
  if (timelineBefore.aboveBottom) {
    expect(Math.abs((await placed()) - placedBefore), `${tool} row moved when it opened; timeline scrollTop ${timelineBefore.top} -> ${(await timeline()).top}`).toBeLessThanOrEqual(1);
  }
  await expect(body.getByText('Loading…')).toHaveCount(0);
  await expect(row.getByTestId('tool-call-card-ao-undecoded')).toHaveCount(0);
  await expect(body).not.toHaveText(/"[A-Za-z_]+":/);
  await expectRowTextWhole(row);
  await shootRow(row, shot(`${tool}-expanded`));
  return row;
}

/**
 * Reads every row of the lead's turn from the newest up, then the target's
 * reply row, with what each decoded body must say.
 */
export async function readMoreRows(app: HarnessApp, page: Page, shot: (name: string) => string, check: (row: Locator) => Promise<void> = async () => {}): Promise<void> {
  await openThread(app, page, MORE_LEAD_TITLE);
  const says: Partial<Record<(typeof MORE_ROWS)[number], (row: Locator) => Promise<void>>> = {
    remote_run: async (row) => {
      await expect(row.getByTestId('tool-call-card-remote-outcome')).toContainText('succeeded');
      await expect(row.getByTestId('tool-call-card-body')).toContainText('build 812 finished: green');
    },
    remote_computers: (row) => expect(row.getByTestId('tool-call-card-ao-line').filter({ hasText: 'Build box' })).toHaveCount(1),
    // The harness browser engine opens pages but renders no content, so
    // the reads that need content refuse; their rows show the refusal.
    browser_evaluate: (row) => expect(row.getByText('renders no page content').first()).toBeVisible(),
    browser_snapshot: (row) => expect(row.getByText('renders no page content').first()).toBeVisible(),
    browser_screenshot: (row) => expect(row.getByText('renders no page content').first()).toBeVisible(),
    browser_pages: (row) => expect(row.getByTestId('tool-call-card-ao-line')).toHaveCount(1),
    memory_list: (row) => expect(row.getByTestId('tool-call-card-ao-line').filter({ hasText: 'The digest needs the schema first.' })).toHaveCount(1),
    memory_add: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText('learning added'),
    workflow_schema: async (row) => {
      await expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText(/Agent Overflow Workflow · \d+ fields · \d+ definitions$/);
      await expect(row.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="required"]')).toContainText('phases');
      await expect(row.getByTestId('tool-call-card-ao-line-title').filter({ hasText: /^phase · object$/ })).toHaveCount(1);
    },
    run_list: (row) => expect(row.getByTestId('tool-call-card-ao-line-link').first()).toHaveAttribute('data-link-kind', 'run'),
    thread_status: (row) => expect(row.getByTestId('tool-call-card-ao-line-title').filter({ hasText: /^remind · accepted/ })).toHaveCount(1),
    thread_item: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText('1 match'),
    thread_ask: async (row) => {
      await expect(row.getByTestId('tool-call-card-preview')).toHaveText(`${MORE_TARGET_TITLE} · What changed in the parser?`);
      await expect(row.getByTestId('tool-call-card-ao-link')).toHaveText(MORE_TARGET_TITLE);
    },
    thread_spawn: async (row) => {
      await expect(row.getByTestId('tool-call-card-preview')).toHaveText(MORE_SPAWN_TITLE);
      await expect(row.getByTestId('tool-call-card-ao-link')).toHaveText(MORE_SPAWN_TITLE);
      await expect(row.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="group"]')).toHaveText('Bugs');
    },
    thread_group: (row) => expect(row.getByTestId('tool-call-card-preview')).toHaveText('rename Bugs → Bug sweep'),
    thread_watch: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toBeVisible(),
    thread_cancel: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toBeVisible(),
  };
  for (const tool of [...MORE_ROWS].reverse()) {
    const row = await readRow(page, tool, shot);
    await says[tool]?.(row);
    await check(row);
  }
  await openThread(app, page, MORE_TARGET_TITLE);
  const reply = await readRow(page, 'thread_reply', shot);
  await expect(reply.getByTestId('tool-call-card-ao-outcome')).toContainText('accepted');
  await expect(reply.getByTestId('tool-call-card-ao-link')).toHaveText(MORE_LEAD_TITLE);
  await check(reply);
}
