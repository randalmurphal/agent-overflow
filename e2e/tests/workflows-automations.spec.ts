import { test, expect } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  doneResult,
  seedWorkflowProject,
  setClaudeScenario,
  setGlobalPause,
  startWorkflow,
  waitForWorkflowState,
  type WorkflowDetail,
  type WorkflowItem,
} from './workflows-helpers.js';

// Scheduler / automations (spec §11) over the real engine, driven entirely
// through RPCs. Cron *ticks* are not exercised here: the scheduler's granularity
// is a minute, so a real tick would mean a minute of wall clock per assertion —
// the tick arithmetic is unit-tested against a fake clock in
// internal/workflow/scheduler. What only a live app can prove is covered:
// Run now goes through the one start path with the reserved seeds reaching the
// phase's prompt, the overlap policy refuses a second press loudly, and an
// internal-event automation chains off a finished run without chaining into
// itself.

interface AutomationView {
  id: string;
  name: string;
  enabled: boolean;
  triggerKind?: string;
  triggerSummary?: string;
  triggerError?: string;
  nextFireAt?: number;
  notes: string;
  lastFiredAt?: number;
  lastRunItemId?: string;
  skipCount: number;
  lastSkipAt?: number;
  lastSkipReason?: string;
}

interface AutomationRun extends WorkflowItem {
  goal: string;
  source: string;
  sourceRef?: string;
  // Seeds cross the wire as the JSON object they are, not as a string.
  seeds?: Record<string, unknown>;
}

interface ThreadItem {
  id: string;
  kind: string;
  role: string;
  summary: string;
}

// A workflow that declares the reserved job-notes seed as an input and renders
// it into its prompt, which is how a scheduled job carries context across runs.
const jobNotesFlowYaml = `id: notes-flow
name: Notes flow
inputs:
  goal:
    schema:
      type: string
  job-notes:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: notes-flow.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
      job-notes:
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

// The chain fixtures deliberately do not read job-notes: a human-started run
// gets no reserved seeds, so a workflow that interpolates them could not be
// started by hand at all.
const seedFlowYaml = `id: seed-flow
name: Seed flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: seed-flow.md
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

const followUpFlowYaml = `id: follow-up-flow
name: Follow up flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: follow-up-flow.md
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

async function createAutomation(
  harness: HarnessApp,
  projectId: string,
  overrides: Record<string, unknown>,
): Promise<AutomationView> {
  return await harness.rpc<AutomationView>('WorkflowCreateAutomation', {
    projectId,
    workflowScope: 'project',
    enabled: true,
    ...overrides,
  });
}

async function listAutomations(
  harness: HarnessApp,
  projectId: string,
): Promise<AutomationView[]> {
  return await harness.rpc<AutomationView[]>('WorkflowListAutomations', projectId);
}

async function waitForAutomation(
  harness: HarnessApp,
  projectId: string,
  automationId: string,
  match: (row: AutomationView) => boolean,
): Promise<AutomationView> {
  const deadline = Date.now() + 15_000;
  let seen: AutomationView | undefined;
  while (Date.now() < deadline) {
    const rows = await listAutomations(harness, projectId);
    seen = rows.find((row) => row.id === automationId);
    if (seen && match(seen)) return seen;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`automation ${automationId} never matched: ${JSON.stringify(seen)}`);
}

test('Run now starts the automation through the one start path and refuses to overlap', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'automation-run-now', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'workflow-automation-project',
    [
      {
        name: 'notes-flow',
        yaml: jobNotesFlowYaml,
        prompts: { 'notes-flow.md': 'Audit {{goal}}. Continuity notes: {{job-notes}}' },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const automation = await createAutomation(harness, project.projectId, {
    workflowId: 'notes-flow',
    name: 'Nightly audit',
    trigger: { kind: 'cron', expr: '0 3 * * *' },
    seeds: { goal: 'the billing API' },
  });
  expect(automation.triggerKind).toBe('cron');
  expect(automation.triggerSummary).toBe('cron 0 3 * * *');
  expect(automation.triggerError ?? '').toBe('');
  expect(automation.nextFireAt ?? 0).toBeGreaterThan(Date.now());

  // Reserved seeds are refused on the definition, because the scheduler is what
  // supplies them.
  await expect(
    createAutomation(harness, project.projectId, {
      workflowId: 'notes-flow',
      name: 'Reserved',
      trigger: { kind: 'cron', expr: '0 4 * * *' },
      seeds: { 'job-notes': 'mine' },
    }),
  ).rejects.toThrow(/reserved/);
  // So is a cron expression the scheduler could never arm.
  await expect(
    createAutomation(harness, project.projectId, {
      workflowId: 'notes-flow',
      name: 'Broken',
      trigger: { kind: 'cron', expr: 'nightly' },
    }),
  ).rejects.toThrow(/fields/);

  await harness.rpc('WorkflowSetJobNotes', automation.id, 'last run left migration 42 half applied');

  // Global pause holds the started run's first phase, so the spec can press Run
  // now twice with the first run provably still active.
  await setGlobalPause(harness, true);
  const run = await harness.rpc<AutomationRun>('WorkflowRunAutomationNow', automation.id);
  expect(run.source).toBe('automation');
  expect(run.sourceRef).toBe(automation.id);
  expect(run.goal).toBe('Nightly audit (run now)');

  await expect(harness.rpc('WorkflowRunAutomationNow', automation.id)).rejects.toThrow(
    new RegExp(`${run.id}.*still running`),
  );
  // A refused manual fire is an error, not a recorded skip: a human is present.
  const armed = await waitForAutomation(
    harness,
    project.projectId,
    automation.id,
    (row) => row.lastRunItemId === run.id,
  );
  expect(armed.skipCount).toBe(0);
  expect(armed.lastFiredAt ?? 0).toBeGreaterThan(0);

  await setGlobalPause(harness, false);
  await waitForWorkflowState(harness, run.id, 'done');

  // The reserved seeds reached the run, and the phase's prompt rendered the job
  // notes — the point of continuity notes is that the agent reads them.
  const detail = await harness.rpc<WorkflowDetail & { item: AutomationRun }>(
    'WorkflowGetItem',
    run.id,
  );
  const seeds = detail.item.seeds ?? {};
  expect(seeds['goal']).toBe('the billing API');
  expect(seeds['job-notes']).toBe('last run left migration 42 half applied');
  expect((seeds['trigger'] as Record<string, unknown>).kind).toBe('manual');

  const phaseThread = detail.phases[0]?.threadId ?? '';
  expect(phaseThread).toBeTruthy();
  // An itemless thread answers `null`; coalescing keeps a missing prompt a
  // legible assertion failure instead of a TypeError on `.find`.
  const items = (await harness.rpc<ThreadItem[] | null>('ListItems', phaseThread, true)) ?? [];
  const prompt = items.find((item) => item.kind === 'user_text');
  expect(prompt?.summary ?? '').toContain('last run left migration 42 half applied');
});

test('an item-done automation chains off a finished run but never off its own', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'automation-chain', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'workflow-automation-chain-project',
    [
      {
        name: 'seed-flow',
        yaml: seedFlowYaml,
        prompts: { 'seed-flow.md': 'Audit {{goal}}.' },
      },
      {
        name: 'follow-up-flow',
        yaml: followUpFlowYaml,
        prompts: { 'follow-up-flow.md': 'Follow up on {{goal}}.' },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const automation = await createAutomation(harness, project.projectId, {
    workflowId: 'follow-up-flow',
    name: 'Chase completions',
    trigger: { kind: 'event', on: 'item-done' },
    seeds: { goal: 'the run that just finished' },
  });
  expect(automation.triggerSummary).toBe('event item-done');
  // An event trigger has no schedule, so it reports no next fire.
  expect(automation.nextFireAt ?? 0).toBe(0);

  // A human-started run finishing is the event the automation chains off.
  const seed = await startWorkflow(
    harness,
    project.projectId,
    'seed-flow',
    'Audit before the follow-up',
    { goal: 'the billing API' },
  );
  await waitForWorkflowState(harness, seed.id, 'done');

  const chained = await waitForAutomation(
    harness,
    project.projectId,
    automation.id,
    (row) => Boolean(row.lastRunItemId),
  );
  const chainedRun = await harness.rpc<WorkflowDetail & { item: AutomationRun }>(
    'WorkflowGetItem',
    chained.lastRunItemId ?? '',
  );
  expect(chainedRun.item.source).toBe('automation');
  expect(chainedRun.item.sourceRef).toBe(automation.id);
  expect(chainedRun.item.goal).toContain(`event item-done from run ${seed.id}`);
  await waitForWorkflowState(harness, chainedRun.item.id, 'done');

  // The chained run finishing matches the very same trigger. Chaining an
  // automation into itself is always an authoring accident, so it is recorded as
  // a skip instead of looping forever.
  const selfChained = await waitForAutomation(
    harness,
    project.projectId,
    automation.id,
    (row) => row.skipCount > 0,
  );
  expect(selfChained.lastSkipReason ?? '').toContain('self-chain');
  expect(selfChained.lastRunItemId).toBe(chainedRun.item.id);

  const runs = await harness.rpc<AutomationRun[]>('WorkflowListItems', project.projectId);
  const started = runs.filter((item) => item.sourceRef === automation.id);
  expect(started).toHaveLength(1);
});
