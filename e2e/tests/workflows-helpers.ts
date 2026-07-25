import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

export interface WorkflowItem {
  id: string;
  state: string;
  reason?: string;
}

export interface WorkflowPhase {
  phaseId: string;
  attempt: number;
  threadId?: string;
  outputEnvelope?: unknown;
  status: string;
}

export interface WorkflowDetail {
  item: WorkflowItem;
  checkPhaseIds: string[];
  phases: WorkflowPhase[];
}

export interface WorkflowStateEvent {
  itemId: string;
  projectId: string;
  from: string;
  to: string;
  reason?: string;
}

const prompt = 'Complete this workflow phase and return the required envelope.';

export interface WorkflowSeedDefinition {
  name: string;
  yaml: string;
  prompts?: Record<string, string>;
}

export interface WorkflowSeedItem {
  workflow: string;
  goal: string;
  seeds?: Record<string, unknown>;
  stepMode?: boolean;
  count?: number;
  target?: 'running' | 'needs-human' | 'done';
}

export async function seedWorkflowProject(
  harness: HarnessApp,
  projectName: string,
  definitions: WorkflowSeedDefinition[],
  items: WorkflowSeedItem[] = [],
  profile = '',
): Promise<SeedResult['projects'][number]> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: projectName,
        repo: {},
        workflows: {
          definitions: definitions.map((definition) => ({
            ...definition,
            prompts: definition.prompts ?? {
              [`${definition.name}.md`]: prompt,
            },
          })),
          profile,
          items,
        },
      },
    ],
  });
  const project = seed.projects[0];
  if (!project) {
    throw new Error(`HarnessSeed returned no project for ${projectName}`);
  }
  return project;
}

export async function seedWorkflow(
  harness: HarnessApp,
  projectName: string,
  workflowName: string,
  yaml: string,
  profile = '',
): Promise<{ projectId: string; path: string }> {
  return await seedWorkflowProject(
    harness,
    projectName,
    [{ name: workflowName, yaml }],
    [],
    profile,
  );
}

export async function setClaudeScenario(
  harness: HarnessApp,
  name: string,
  turns: Array<{ label?: string; steps: unknown[] }>,
): Promise<void> {
  await harness.rpc('HarnessSetScenario', {
    scenario: { version: 1, name, provider: 'claude', turns },
  });
}

export async function start(
  harness: HarnessApp,
  projectId: string,
  workflowId: string,
  stepMode = false,
): Promise<WorkflowItem> {
  return await startWorkflow(
    harness,
    projectId,
    workflowId,
    `Run ${workflowId}`,
    undefined,
    stepMode,
  );
}

export async function startWorkflow(
  harness: HarnessApp,
  projectId: string,
  workflowId: string,
  goal: string,
  seeds: Record<string, unknown> = { goal },
  stepMode = false,
): Promise<WorkflowItem> {
  return await harness.rpc<WorkflowItem>(
    'WorkflowStartRun',
    projectId,
    workflowId,
    'project',
    goal,
    seeds,
    null,
    '',
    stepMode,
  );
}

export async function waitForWorkflowState(
  harness: HarnessApp,
  itemId: string,
  state: string,
  reason?: string,
): Promise<WorkflowStateEvent> {
  return await harness.waitForEvent<WorkflowStateEvent>(
    'workflow:item-state',
    (event) =>
      event.itemId === itemId &&
      event.to === state &&
      (reason === undefined || event.reason === reason),
  );
}

// setGlobalPause drives the one engine-level kill switch. While paused a
// started run is admitted and persisted `running` with its first phase held,
// so a spec can stage scenarios before any provider session exists.
export async function setGlobalPause(harness: HarnessApp, paused: boolean): Promise<void> {
  await harness.rpc('WorkflowSetGlobalPause', paused);
}

export async function waitForEnginePause(
  harness: HarnessApp,
  paused: boolean,
): Promise<void> {
  await harness.waitForEvent<{ paused: boolean }>(
    'workflow:engine-state',
    (event) => event.paused === paused,
  );
}

export function doneResult(outputs: Record<string, unknown>): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    structured_output: {
      status: 'done',
      outputs,
      question: null,
      reason: null,
    },
  });
}

export function questionResult(question: string): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    structured_output: {
      status: 'question',
      outputs: null,
      question,
      reason: null,
    },
  });
}

export function singlePhaseWorkflow(
  id: string,
  gate: string,
  access: 'read-only' | 'write' = 'read-only',
): string {
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: ${id}.md
    access: ${access}
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
${gate}
cleanup: manual
`;
}

export function retryableFailureWorkflow(id: string): string {
  return `id: ${id}
name: ${id}
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
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
        - when:
            eq:
              ref: run.complete
              value: false
          to: failed
        - to: done
`;
}
