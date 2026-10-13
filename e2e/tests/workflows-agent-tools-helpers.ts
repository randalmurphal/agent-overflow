// Shared setup for the ao-workflow-tools specs (workflows-agent-tools.spec.ts
// and compact-workflows-agent-tools.spec.ts): a project with a workflow and
// an interactive thread, scenarios whose turns make REAL MCP calls against
// the app's workflow tools (the mock's `mcpCall` / `mcpList` steps), and the
// answer each call got.

import type { Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type ScriptedTurn,
  type ToolAnswer,
} from './thread-tools-helpers.js';
import { doneEnvelope, doneResult } from './workflows-helpers.js';

export const WORKFLOW_TOOLS_SERVER = 'ao-workflow-tools';

/** Every tool, in the server's table order. */
export const ALL_WORKFLOW_TOOLS = [
  'workflow_list',
  'workflow_validate',
  'workflow_schema',
  'workflow_new',
  'run_start',
  'run_status',
  'run_watch',
  'run_output',
  'run_inspect',
  'run_narrative',
  'run_list',
  'run_control',
  'run_amend',
  'run_guide',
  'run_resolve',
  'memory_add',
  'memory_list',
];

export const MODELS: Record<ProviderName, string> = {
  claude: 'claude-opus-4-7',
  codex: 'gpt-5.5',
};

export function otherProvider(provider: ProviderName): ProviderName {
  return provider === 'claude' ? 'codex' : 'claude';
}

/** The run id out of a run_start answer. */
export const START_RUN_PATTERN = '"itemId":"([^"]+)"';

/**
 * A workflow whose one phase reports done on `provider`, with a workflow
 * output so run_output has something to read. The phase runs on the
 * provider the interactive thread does not use: harness scenario rules are
 * keyed by provider and cwd, and a read-only phase runs in the project root,
 * the interactive thread's cwd.
 */
export function doneFlowYaml(id: string, provider: ProviderName): string {
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
outputs:
  verdict:
    from: run.complete
phases:
  - id: run
    driver: agent
    provider: ${provider}
    model: ${MODELS[provider]}
    prompt: ${id}.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`;
}

export interface WorkflowSeedDefinition {
  name: string;
  yaml: string;
}

/** Seeds one project with its workflows and, optionally, one interactive thread. */
export async function seedToolsProject(
  harness: HarnessApp,
  name: string,
  definitions: WorkflowSeedDefinition[],
  thread?: { title: string; provider: ProviderName },
): Promise<{ projectId: string; path: string; threadId: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name,
        repo: {},
        ...(thread ? { threads: [{ title: thread.title, provider: thread.provider }] } : {}),
        workflows: {
          definitions: definitions.map((definition) => ({
            ...definition,
            prompts: { [`${definition.name}.md`]: 'Do the work and report the outcome.' },
          })),
        },
      },
    ],
  });
  const project = seed.projects[0];
  if (!project) throw new Error(`HarnessSeed returned no project for ${name}`);
  return { projectId: project.projectId, path: project.path, threadId: project.threadIds?.[0] ?? '' };
}

/** Every mock of `provider` that has no cwd rule ends its turns done. */
export async function answerPhasesDone(harness: HarnessApp, provider: ProviderName): Promise<void> {
  await setPhaseScenario(harness, provider, `${provider}-phases-done`, [], { complete: true });
}

/** One MCP step a phase's turn takes before its envelope. */
export type PhaseStep =
  | { list: true }
  | { call: string; args?: Record<string, unknown>; timeoutMs?: number }
  | { capture: string; pattern: string };

function phaseStepJSON(step: PhaseStep): Record<string, unknown> {
  if ('list' in step) return { mcpList: { server: WORKFLOW_TOOLS_SERVER } };
  if ('call' in step) {
    return {
      mcpCall: {
        server: WORKFLOW_TOOLS_SERVER,
        tool: step.call,
        args: step.args ?? {},
        ...(step.timeoutMs ? { timeoutMs: step.timeoutMs } : {}),
      },
    };
  }
  return { capture: { var: step.capture, from: '${MCP_RESULT}', pattern: step.pattern } };
}

/**
 * A provider-wide rule for workflow phase sessions: each turn runs `steps`
 * against ao-workflow-tools, then answers done with `outputs`. Phase
 * sessions are the only mocks of that provider in the spec without a cwd
 * rule, so the rule reaches exactly them.
 */
export async function setPhaseScenario(
  harness: HarnessApp,
  provider: ProviderName,
  name: string,
  steps: PhaseStep[],
  outputs: Record<string, unknown>,
): Promise<void> {
  const calls = steps.map(phaseStepJSON);
  if (provider === 'claude') {
    await harness.rpc('HarnessSetScenario', {
      scenario: {
        version: 1,
        name,
        provider: 'claude',
        turns: [{ steps: [...calls, { emit: { lines: [doneResult(outputs)] } }] }],
        afterTurns: 'repeatLast',
      },
    });
    return;
  }
  const envelope = JSON.stringify(JSON.stringify(doneEnvelope(outputs)));
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name,
      provider: 'codex',
      codex: { threadId: 'mock-codex-thread', responses: {} },
      turns: [
        {
          steps: [
            {
              emit: {
                lines: [
                  '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}',
                ],
              },
            },
            ...calls,
            {
              emit: {
                lines: [
                  `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"\${THREAD_ID}","turnId":"\${TURN_ID}","item":{"type":"agentMessage","id":"msg-\${TURN}","status":"completed","text":${envelope}}}}`,
                  '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}',
                ],
              },
            },
          ],
        },
      ],
      afterTurns: 'repeatLast',
    },
  });
}

/** One workflow tool's answer, decoded; a refusal keeps its prose. */
export async function awaitWorkflowAnswer<T = Record<string, unknown>>(
  harness: HarnessApp,
  tool: string,
  filter: { cwd?: string; timeoutMs?: number } = {},
): Promise<ToolAnswer<T>> {
  const event = await harness.awaitMcpResult({
    server: WORKFLOW_TOOLS_SERVER,
    tool,
    cwd: filter.cwd,
    timeoutMs: filter.timeoutMs ?? 60_000,
  });
  const text = event.report.result ?? '';
  const answer: ToolAnswer<T> = { isError: Boolean(event.report.isError), text };
  if (!answer.isError) answer.value = JSON.parse(text) as T;
  return answer;
}

export interface StartAnswer {
  start: {
    itemId: string;
    workflowId: string;
    workflowScope: string;
    state: string;
    skipped?: boolean;
    boundThreadId?: string;
    bindingWarning?: string;
  };
  outcome?: string;
  run?: { itemId: string; state: string; resting: boolean };
  notify?: { armed: boolean; detail: string };
}

/**
 * Drives one interactive thread through a run: it lists the tools, starts
 * `workflowId` and waits for it to rest, then reads its outputs. Returns
 * the answers in call order, after the turn and the wake that follows the
 * run's rest have both completed.
 */
export async function startAndWatchFromThread(
  harness: HarnessApp,
  provider: ProviderName,
  project: { path: string; threadId: string },
  workflowId: string,
  later: ScriptedTurn[] = [{ text: 'The run is done; nothing else to do.' }],
): Promise<{ tools: string[]; instructions: string; start: StartAnswer; output: Record<string, unknown> }> {
  const W = WORKFLOW_TOOLS_SERVER;
  await setScenario(
    harness,
    project.path,
    threadToolsScenario({
      name: `workflow-tools-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        {
          steps: [
            { list: { server: W } },
            {
              call: {
                server: W,
                tool: 'run_start',
                args: { workflow_id: workflowId, goal: 'Ship the importer', seeds: { goal: 'Ship the importer' }, wait_seconds: 120 },
                timeoutMs: 150_000,
              },
            },
            { capture: { var: 'RUN', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
            { call: { server: W, tool: 'run_output', args: { run_id: '${RUN}' } } },
          ],
          text: 'Started the run and watched it finish.',
        },
        ...later,
      ],
      afterTurns: 'repeatLast',
    }),
  );
  await harness.rpc('StartSession', project.threadId);
  await harness.rpc('SendMessage', project.threadId, 'Run the importer workflow and tell me how it went.', null);

  const listing = await harness.awaitMcpTools({ server: W, cwd: project.path, timeoutMs: 60_000 });
  expect(listing.isError, listing.error).toBe(false);
  const started = await awaitWorkflowAnswer<StartAnswer>(harness, 'run_start', { cwd: project.path, timeoutMs: 180_000 });
  expect(started.isError, started.text).toBe(false);
  const output = await awaitWorkflowAnswer(harness, 'run_output', { cwd: project.path });
  expect(output.isError, output.text).toBe(false);
  await awaitTurnCompleted(harness, project.threadId);
  return { tools: listing.tools, instructions: listing.instructions, start: started.value!, output: output.value! };
}

/**
 * Opens the thread in the page and captures it once the run_start row and
 * the wake that reported the run's rest are both on screen. The harness's
 * temporary root is written as `<harness root>` in the capture.
 */
export async function captureRunFromThread(
  harness: HarnessApp,
  page: Page,
  title: string,
  runId: string,
  name: string,
): Promise<void> {
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  // The turn's two tool calls fold into one activity run; open it so the
  // run_start row is in the capture.
  const run = page.getByTestId('activity-run').filter({ hasText: 'Workflow start' }).first();
  await expect(run).toBeVisible({ timeout: 30_000 });
  if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  const startRow = run.getByTestId('tool-call-card').filter({ hasText: 'importer' }).first();
  await expect(startRow).toBeVisible();
  const wake = page.getByText(`Run "${runId}"`).last();
  await wake.scrollIntoViewIfNeeded();
  await expect(wake).toBeVisible({ timeout: 30_000 });
  await run.evaluate((element) => element.scrollIntoView({ block: 'start' }));
  await expect(startRow).toBeInViewport();
  await page.evaluate(() => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent ?? '';
      if (text.includes('/ao-harness-')) node.textContent = text.replace(/\/\S*?\/ao-harness-[^/\s]+/g, '<harness root>');
    }
  });
  await page.evaluate(
    () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
  );
  await page.screenshot({ path: test.info().outputPath(name), animations: 'disabled' });
}
