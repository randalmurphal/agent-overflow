// Shared setup for the ao-workflow-tools specs (workflows-agent-tools.spec.ts
// and compact-workflows-agent-tools.spec.ts): a project with a workflow and
// an interactive thread, scenarios whose turns make REAL MCP calls against
// the app's workflow tools (the mock's `mcpCall` / `mcpList` steps), the
// answer each call got, and the migrator run whose rest wakes the thread.

import type { Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
  awaitTurnCompleted,
  type ProviderName,
  type ScriptedTurn,
  type ToolAnswer,
} from './thread-tools-helpers.js';
import { doneEnvelope, doneResult } from './workflows-helpers.js';
import { openRunsFor } from './standing-agent-wake-helpers.js';
import { screenshot, screenshotsOn } from '../src/screenshot.js';

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

/** One step a phase's turn takes before its envelope. */
export type PhaseStep =
  | { list: true }
  | { call: string; args?: Record<string, unknown>; timeoutMs?: number }
  | { capture: string; pattern: string }
  | { gate: string };

function phaseStepJSON(step: PhaseStep): Record<string, unknown> {
  if ('list' in step) return { mcpList: { server: WORKFLOW_TOOLS_SERVER } };
  if ('gate' in step) return { waitSignal: { name: step.gate } };
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

/** The gate a held phase waits on until the spec releases it. */
export const HELD_PHASE_GATE = 'migration';

/** An input a mock received. */
export type MockInput = { scenario: string; report: { kind: string; input?: string } };

/** The scenario the interactive thread's session runs. */
export function threadScenario(provider: ProviderName): string {
  return `workflow-tools-${provider}`;
}

/**
 * How many inputs the interactive thread's session received that name a run.
 * Matched by the session's scenario, not its cwd: a read-only phase runs in
 * the same project root, and its prompt names its run.
 */
export function inputsNaming(harness: HarnessApp, provider: ProviderName, runId: string): number {
  return harness.countEvents<MockInput>(
    'harness:mock',
    (event) =>
      event.scenario === threadScenario(provider) &&
      event.report.kind === 'user_input' &&
      (event.report.input ?? '').includes(runId),
  );
}

/** The message that starts `migrator` (migratorTurns). */
export const MIGRATION_MESSAGE = 'Now start the migration and let me know when it is done.';

/**
 * The interactive thread's turns for `migrator`: the first starts it with a
 * wait that ends first, and the second, the one its rest wakes, reads its
 * outputs.
 */
export function migratorTurns(): ScriptedTurn[] {
  const W = WORKFLOW_TOOLS_SERVER;
  return [
    {
      steps: [
        {
          call: {
            server: W,
            tool: 'run_start',
            args: { workflow_id: 'migrator', goal: 'Migrate the schema', seeds: { goal: 'Migrate the schema' }, wait_seconds: 2 },
            timeoutMs: 30_000,
          },
        },
        { capture: { var: 'RUN2', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
      ],
      text: 'The migration is still running; its result will arrive here.',
    },
    {
      steps: [{ call: { server: W, tool: 'run_output', args: { run_id: '${RUN2}' } } }],
      text: 'The migration finished and its outputs are in.',
    },
  ];
}

/**
 * Holds every phase of `provider`'s other provider on HELD_PHASE_GATE and
 * sends MIGRATION_MESSAGE; returns once the thread's session received it.
 */
export async function sendMigration(
  harness: HarnessApp,
  provider: ProviderName,
  project: { path: string; threadId: string },
): Promise<void> {
  const phaseProvider = otherProvider(provider);
  await setPhaseScenario(harness, phaseProvider, `${phaseProvider}-phases-held`, [{ gate: HELD_PHASE_GATE }], { complete: true });
  await harness.send(project.threadId, MIGRATION_MESSAGE);
  await harness.waitForEvent<MockInput>(
    'harness:mock',
    (event) =>
      event.scenario === threadScenario(provider) &&
      event.report.kind === 'user_input' &&
      (event.report.input ?? '').includes(MIGRATION_MESSAGE),
    60_000,
  );
}

export interface Migration {
  /** run_start's wait ended first. */
  backgrounded: StartAnswer;
  /** What the turn the rest woke read with run_output. */
  output: Record<string, unknown>;
}

/**
 * After sendMigration: run_start's wait ends first, the held phase is let
 * go, the run's rest wakes the thread, and the wake's turn reads its outputs.
 */
export async function finishMigration(
  harness: HarnessApp,
  provider: ProviderName,
  project: { path: string; threadId: string },
): Promise<Migration> {
  const backgrounded = await awaitWorkflowAnswer<StartAnswer>(harness, 'run_start', { cwd: project.path, timeoutMs: 60_000 });
  expect(backgrounded.isError, backgrounded.text).toBe(false);
  await awaitTurnCompleted(harness, project.threadId);

  const gate = await awaitGate(harness, HELD_PHASE_GATE);
  await advanceGate(harness, gate.mockId, HELD_PHASE_GATE);
  const runId = backgrounded.value!.start.itemId;
  await harness.waitForEvent<MockInput>(
    'harness:mock',
    (event) =>
      event.scenario === threadScenario(provider) &&
      event.report.kind === 'user_input' &&
      (event.report.input ?? '').includes(runId),
    60_000,
  );
  const output = await awaitWorkflowAnswer(harness, 'run_output', { cwd: project.path });
  expect(output.isError, output.text).toBe(false);
  await awaitTurnCompleted(harness, project.threadId);
  return { backgrounded: backgrounded.value!, output: output.value! };
}

/**
 * Opens the thread in the page and captures its end: the backgrounded
 * run_start row, the wake that reported the run's rest, and the turn that
 * read it, every activity run expanded and the last row clear of the
 * composer. With screenshots on, the harness's temporary root is written
 * as `<harness root>`.
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
  await openRunsFor(page, page.getByText(`Run "${runId}"`).last());
  await expect(page.getByTestId('tool-call-card').filter({ hasText: 'migrator' }).first()).toBeAttached();
  if (screenshotsOn) {
    await page.evaluate(() => {
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        const text = node.textContent ?? '';
        if (text.includes('/ao-harness-')) node.textContent = text.replace(/\/\S*?\/ao-harness-[^/\s]+/g, '<harness root>');
      }
    });
  }
  const scroller = page.getByTestId('message-timeline-scroll').first();
  await scroller.evaluate((element) => element.scrollTo({ top: element.scrollHeight }));
  await page.evaluate(
    () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
  );
  // The end of the thread reads whole: the wake and the turn after it are
  // on screen, and the last row stops above the composer.
  await expect(page.getByText(`Run "${runId}"`).last()).toBeInViewport();
  const last = (await page.getByTestId('message-timeline-node').last().boundingBox())!;
  const composer = (await page.getByTestId('composer-root').first().boundingBox())!;
  expect(last.y + last.height).toBeLessThanOrEqual(composer.y + 1);
  // The sidebar's system footer shows the host machine's own CPU and memory
  // once its first sample lands, which a longer run reaches and a shorter one
  // does not. It is not part of the evidence, so no capture shows it.
  await screenshot(page, {
    path: test.info().outputPath(name),
    animations: 'disabled',
    style: '[data-testid="sidebar-system-stats"] { display: none !important; }',
  });
}
