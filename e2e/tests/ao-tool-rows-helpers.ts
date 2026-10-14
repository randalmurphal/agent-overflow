// One lead thread that calls each kind of AO tool through a mock provider
// making real MCP calls, shared by ao-tool-rows.spec.ts (desktop) and
// compact-ao-tool-rows.spec.ts (compact): a spawn whose prompt is long
// enough for the wire projection to clip, a send with notify, a status
// read of that request, a search, a show, a loop, a run start and a run
// status, and a show of a thread that does not exist.

import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import {
  RESULT_TOKEN_PATTERN,
  awaitToolAnswer,
  plainScenario,
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

export const LEAD_TITLE = 'Lead thread';
export const TARGET_TITLE = 'Target thread';
export const SPAWN_TITLE = 'Release notes';

/** Over the 2 KiB meta budget on its own, so the row ships its opening only. */
export const SPAWN_PROMPT = `Write the release notes for the next version.\n\n${'Each section lists what changed and why.\n'.repeat(80)}`;

export interface AoToolRowsRun {
  leadThreadId: string;
  targetThreadId: string;
  spawnedThreadId: string;
  runId: string;
}

/**
 * Seeds the lead's project (with one workflow) and a target project with an
 * existing thread, scripts the lead's turn and drives it to completion.
 * The spawned thread and the target answer at once, so both requests settle
 * and their wakes land in the lead.
 */
export async function driveAoToolRows(harness: HarnessApp, provider: ProviderName, tag: string): Promise<AoToolRowsRun> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `ao-rows-lead-${tag}`,
        repo: {},
        threads: [{ title: LEAD_TITLE, provider }],
        workflows: {
          definitions: [
            {
              name: 'digest',
              yaml: doneFlowYaml('digest', otherProvider(provider)),
              prompts: { 'digest.md': 'Do the work and report the outcome.' },
            },
          ],
        },
      },
      { name: `ao-rows-target-${tag}`, repo: {}, threads: [{ title: TARGET_TITLE, provider }] },
    ],
  });
  const lead = seed.projects[0];
  const target = seed.projects[1];
  const leadThreadId = lead.threadIds[0];
  const targetThreadId = target.threadIds[0];

  await answerPhasesDone(harness, otherProvider(provider));
  await setScenario(
    harness,
    target.path,
    plainScenario({ name: `ao-rows-target-${tag}`, provider, texts: ['Rebased and green.'] }),
  );
  await setScenario(
    harness,
    lead.path,
    threadToolsScenario({
      name: `ao-rows-lead-${tag}`,
      provider,
      echoUserMessage: true,
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: { prompt: SPAWN_PROMPT, title: SPAWN_TITLE, project_id: target.projectId, notify: true },
              },
            },
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: targetThreadId, message: 'Rebase onto main\nthen run the tests.', notify: true },
              },
            },
            { capture: { var: 'SEND', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            { call: { tool: 'thread_status', args: { tokens: ['${SEND}'] } } },
            { call: { tool: 'thread_search', args: { query: 'Target' } } },
            { call: { tool: 'thread_show', args: { thread_id: targetThreadId, window: 'last_answer' } } },
            { call: { tool: 'thread_loop', args: { label: 'board', note: 'Check the board.', every_seconds: 600 } } },
            { call: { tool: 'thread_show', args: { thread_id: 'nosuch1' } } },
            {
              call: {
                server: WORKFLOW_TOOLS_SERVER,
                tool: 'run_start',
                args: { workflow_id: 'digest', goal: 'Weekly summary', seeds: { goal: 'Weekly summary' } },
              },
            },
            { capture: { var: 'RUN', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
            { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_status', args: { run_id: '${RUN}' } } },
          ],
          text: 'Started the work.',
        },
        { text: 'Read the answer.' },
      ],
    }),
  );

  await harness.rpc('StartSession', leadThreadId);
  await harness.rpc('SendMessage', leadThreadId, 'get the release moving', null);
  const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: lead.path });
  expect(spawn.isError, spawn.text).toBe(false);
  // run_status is the turn's last call; the refusal before it is asserted
  // where it shows, on its row.
  const status = await harness.awaitMcpResult({ server: WORKFLOW_TOOLS_SERVER, tool: 'run_status', cwd: lead.path, timeoutMs: 60_000 });
  expect(status.report.isError, status.report.result).toBeFalsy();
  const runId = /"itemId":"([^"]+)"/.exec(status.report.result ?? '')?.[1] ?? '';
  expect(runId).not.toBe('');
  return { leadThreadId, targetThreadId, spawnedThreadId: spawn.value!.thread_id, runId };
}

/** Opens the lead thread with every activity run expanded. */
export async function openLead(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row-title').filter({ hasText: LEAD_TITLE }).first().click();
  // The turn's calls fold into activity runs; every run opens so each row
  // is on the page.
  const runs = page.getByTestId('activity-run');
  await expect(runs.first()).toBeAttached({ timeout: 30_000 });
  for (let index = 0; index < (await runs.count()); index++) {
    const run = runs.nth(index);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  }
  await expect(page.getByTestId('tool-call-card-label').filter({ hasText: /^spawn$/ }).first()).toBeAttached();
}

/** The one tool row with this gutter verb whose header reads `preview`. */
export function toolRow(page: Page, label: string, preview: string | RegExp): Locator {
  return page
    .getByTestId('tool-call-card')
    .filter({ has: page.getByTestId('tool-call-card-label').filter({ hasText: new RegExp(`^${label}$`) }) })
    .filter({ has: page.getByTestId('tool-call-card-preview').filter({ hasText: preview }) });
}

/** Expands a row and waits for its decoded result. */
export async function expandRow(row: Locator): Promise<void> {
  await row.getByTestId('tool-call-card-toggle').click();
  await expect(row.getByTestId('tool-call-card-toggle')).toHaveAttribute('aria-expanded', 'true');
}

/**
 * Screenshots a row centred in the timeline once nothing is moving it. A
 * row merely scrolled into view can sit under the composer, which floats
 * over the timeline's bottom edge, and a timeline following a live tail
 * pulls a script-scrolled row away again: a wheel turn escapes the follow
 * as a reader does, then the row is centred and must hold still on screen.
 */
export async function shootRow(row: Locator, path: string): Promise<void> {
  const page = row.page();
  await page.getByTestId('message-timeline-scroll').hover();
  await page.mouse.wheel(0, -40);
  await row.evaluate((element) => element.scrollIntoView({ block: 'center' }));
  let last = '';
  await expect.poll(async () => {
    const box = await row.boundingBox();
    const viewport = page.viewportSize()!;
    const onScreen = box !== null && box.y >= 0 && box.y + box.height <= viewport.height;
    const at = onScreen ? `${Math.round(box.x)},${Math.round(box.y)},${Math.round(box.height)}` : '';
    const still = at !== '' && at === last;
    last = at;
    return still;
  }, { intervals: [150], timeout: 10_000 }).toBe(true);
  await row.screenshot({ path, animations: 'disabled' });
}
