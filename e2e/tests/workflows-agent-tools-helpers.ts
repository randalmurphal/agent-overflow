// Shared setup for the ao-workflow-tools specs (workflows-agent-tools.spec.ts
// and compact-workflows-agent-tools.spec.ts): a project with a workflow and
// an interactive thread, scenarios whose turns make REAL MCP calls against
// the app's workflow tools (the mock's `mcpCall` / `mcpList` steps), and the
// answer each call got.

import type { Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
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
type MockInput = { scenario: string; report: { kind: string; input?: string } };

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

export interface ThreadRuns {
  tools: string[];
  instructions: string;
  /** importer: run_start waited and returned the rest. */
  rested: StartAnswer;
  restedOutput: Record<string, unknown>;
  /** migrator: run_start's wait ended first, and the rest came as a wake. */
  backgrounded: StartAnswer;
  backgroundedOutput: Record<string, unknown>;
}

/**
 * Drives one interactive thread through two runs, the two ways a rest
 * reaches it. Turn 1 lists the tools, starts `importer` and waits for its
 * rest, then reads its outputs: the call is the delivery, so no wake follows.
 * Turn 2 starts `migrator`, whose phase holds on HELD_PHASE_GATE, with a short
 * wait that ends first. Releasing the gate lets the run rest, the rest wakes
 * the thread exactly once, and turn 3 (the wake's) reads its outputs.
 */
export async function startAndWatchFromThread(
  harness: HarnessApp,
  provider: ProviderName,
  project: { path: string; threadId: string },
): Promise<ThreadRuns> {
  const W = WORKFLOW_TOOLS_SERVER;
  const phaseProvider = otherProvider(provider);
  await answerPhasesDone(harness, phaseProvider);
  await setScenario(
    harness,
    project.path,
    threadToolsScenario({
      name: threadScenario(provider),
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
                args: { workflow_id: 'importer', goal: 'Ship the importer', seeds: { goal: 'Ship the importer' }, wait_seconds: 120 },
                timeoutMs: 150_000,
              },
            },
            { capture: { var: 'RUN', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
            { call: { server: W, tool: 'run_output', args: { run_id: '${RUN}' } } },
          ],
          text: 'The importer ran to completion while I waited.',
        },
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
      ],
      afterTurns: 'repeatLast',
    }),
  );
  await harness.rpc('StartSession', project.threadId);
  await harness.rpc('SendMessage', project.threadId, 'Run the importer workflow and tell me how it went.', null);

  const listing = await harness.awaitMcpTools({ server: W, cwd: project.path, timeoutMs: 60_000 });
  expect(listing.isError, listing.error).toBe(false);
  const rested = await awaitWorkflowAnswer<StartAnswer>(harness, 'run_start', { cwd: project.path, timeoutMs: 180_000 });
  expect(rested.isError, rested.text).toBe(false);
  const restedOutput = await awaitWorkflowAnswer(harness, 'run_output', { cwd: project.path });
  expect(restedOutput.isError, restedOutput.text).toBe(false);
  await awaitTurnCompleted(harness, project.threadId);

  // The migrator's phase holds until the spec releases it, so the wait in
  // turn 2 ends first.
  await setPhaseScenario(harness, phaseProvider, `${phaseProvider}-phases-held`, [{ gate: HELD_PHASE_GATE }], { complete: true });
  const second = 'Now start the migration and let me know when it is done.';
  // A wake for the importer's rest would have started a turn of its own
  // after turn 1, and the send would be refused as the thread is working.
  const refused = await harness.rpc('SendMessage', project.threadId, second, null).then(
    () => undefined,
    (error: Error) => error.message,
  );
  expect(refused, 'the thread was busy after turn 1: the importer rest the wait returned also woke it').toBeUndefined();
  // The barrier for the absence below: a wake for the importer's rest would
  // have been queued during turn 1, ahead of this message.
  await harness.waitForEvent<MockInput>(
    'harness:mock',
    (event) =>
      event.scenario === threadScenario(provider) &&
      event.report.kind === 'user_input' &&
      (event.report.input ?? '').includes(second),
    60_000,
  );
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
  const backgroundedOutput = await awaitWorkflowAnswer(harness, 'run_output', { cwd: project.path });
  expect(backgroundedOutput.isError, backgroundedOutput.text).toBe(false);
  await awaitTurnCompleted(harness, project.threadId);

  return {
    tools: listing.tools,
    instructions: listing.instructions,
    rested: rested.value!,
    restedOutput: restedOutput.value!,
    backgrounded: backgrounded.value!,
    backgroundedOutput: backgroundedOutput.value!,
  };
}

/**
 * Opens the thread in the page and captures its end: the backgrounded
 * run_start row, the wake that reported the run's rest, and the turn that
 * read it, every activity run expanded and the last row clear of the
 * composer. The harness's temporary root is written as `<harness root>`.
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
  await expect(page.getByText(`Run "${runId}"`).last()).toBeAttached({ timeout: 30_000 });
  const runs = page.getByTestId('activity-run');
  for (let index = 0; index < (await runs.count()); index++) {
    const run = runs.nth(index);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  }
  await expect(page.getByTestId('tool-call-card').filter({ hasText: 'migrator' }).first()).toBeAttached();
  await page.evaluate(() => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent ?? '';
      if (text.includes('/ao-harness-')) node.textContent = text.replace(/\/\S*?\/ao-harness-[^/\s]+/g, '<harness root>');
    }
  });
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
  await page.screenshot({ path: test.info().outputPath(name), animations: 'disabled' });
}
