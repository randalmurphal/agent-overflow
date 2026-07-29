import { test, expect } from './fixtures.js';
import {
  doneEnvelope,
  doneResult,
  seedWorkflowProject,
  sessionConfigs,
  setClaudeScenario,
  setCodexScenario,
  start,
  stuckEnvelope,
  stuckResult,
  waitForWorkflowState,
  type WorkflowDetail,
  type WorkflowUnit,
} from './workflows-helpers.js';

// Fan-out phases run N units in parallel and a join that consolidates them
// (spec §3, §9). These specs drive the real runner: each unit is its own
// provider session on its own AO thread, a writing unit gets its own
// sub-worktree on its own branch, and the join's envelope IS the phase's — so
// what routes the gate is the join, never a unit.

const unitById = (detail: WorkflowDetail, unitId: string): WorkflowUnit => {
  const unit = detail.units.find((candidate) => candidate.unitId === unitId);
  if (!unit) {
    throw new Error(
      `unit ${unitId} is not part of the run: ${JSON.stringify(detail.units)}`,
    );
  }
  return unit;
};

const writingUnit = (id: string) => `      - id: ${id}
        provider: claude
        model: claude-opus-4-7
        prompt: ${id}.md
        access: write
        outputs:
          report:
            schema:
              type: string
`;

// A static fan-out with two writing units and a read-only join. The units'
// output contract matches the phase's on purpose: one mock scenario then
// answers both a unit envelope and the join's phase envelope, so the spec
// exercises the real per-element contracts rather than a stub per role.
const staticFanOutYaml = `id: fanout-static
name: Fan-out static
inputs:
  goal:
    schema:
      type: string
phases:
  - id: port
    name: Port in parallel
    shape: fan-out
    outputs:
      report:
        schema:
          type: string
    fan_out:
${writingUnit('alpha')}${writingUnit('beta')}    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('a static fan-out runs each unit isolated and lets the join drive the gate', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'fanout-static', [
    { steps: [{ emit: { lines: [doneResult({ report: 'ok' })] } }] },
  ]);
  const project = await seedWorkflowProject(
    harness,
    'workflow-fanout-static-project',
    [
      {
        name: 'fanout-static',
        yaml: staticFanOutYaml,
        prompts: {
          'alpha.md': 'Port the alpha slice and return the envelope.',
          'beta.md': 'Port the beta slice and return the envelope.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'fanout-static');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.units.map((unit) => unit.unitId)).toEqual(['alpha', 'beta', 'merge']);
  expect(detail.units.map((unit) => unit.status)).toEqual(['done', 'done', 'done']);
  expect(detail.units.map((unit) => unit.kind)).toEqual(['unit', 'unit', 'join']);

  // Every unit is inspectable as an ordinary AO thread, and no two share one.
  const threads = detail.units.map((unit) => unit.threadId);
  expect(threads.every((threadId) => Boolean(threadId))).toBe(true);
  expect(new Set(threads).size).toBe(3);

  // The join's thread is the phase attempt's thread, because the join's
  // envelope is the phase's envelope — that is what every phase-level
  // continuation (answer, complete-takeover) resolves through.
  const merge = unitById(detail, 'merge');
  expect(detail.phases).toHaveLength(1);
  expect(detail.phases[0]?.threadId).toBe(merge.threadId);
  expect(detail.phases[0]?.outputEnvelope).toMatchObject({
    status: 'done',
    outputs: { report: 'ok' },
  });

  // Writing units are isolated on their own branches, cut from the item's.
  const alpha = unitById(detail, 'alpha');
  const beta = unitById(detail, 'beta');
  expect(alpha.branch).toBeTruthy();
  expect(beta.branch).toBeTruthy();
  expect(alpha.branch).not.toBe(beta.branch);
  expect(alpha.branch?.startsWith(`${detail.item.branch}-`)).toBe(true);
  expect(beta.branch?.startsWith(`${detail.item.branch}-`)).toBe(true);
  // The join consolidates them; it is never isolated itself.
  expect(merge.branch ?? '').toBe('');
  expect(merge.worktreePath ?? '').toBe('');
  // A done join consumed its units' checkouts, so the sub-worktrees are
  // retired — the branches above are what survives.
  expect(alpha.worktreePath ?? '').toBe('');
  expect(beta.worktreePath ?? '').toBe('');

  // Three separate provider sessions ran, and each was launched with the access
  // its own definition declared rather than the phase's.
  const configs = await sessionConfigs(harness, 'claude', 3);
  expect(configs).toHaveLength(3);
  expect(configs.filter((config) => config.permissionMode === 'bypassPermissions')).toHaveLength(2);
  const joinConfig = configs.find((config) => config.permissionMode === 'dontAsk');
  expect(joinConfig?.disallowedTools).toEqual(['Write', 'Edit', 'NotebookEdit']);
});

// A dynamic fan-out learns its width at run time: a prior phase emits the array
// and the template stamps one unit per element.
const dynamicFanOutYaml = `id: fanout-dynamic
name: Fan-out dynamic
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: plan.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: object
            properties:
              path:
                type: string
            required: [path]
    gate:
      routes:
        - to: port
  - id: port
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: port-section
      provider: codex
      model: gpt-5.5
      prompt: unit.md
      access: read-only
    join:
      id: merge
      provider: codex
      model: gpt-5.5
      prompt: merge.md
      access: read-only
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('a dynamic fan-out stamps one unit per element of the array a prior phase emitted', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'fanout-plan', [
    {
      steps: [
        {
          emit: {
            lines: [doneResult({ sections: [{ path: 'internal/a' }, { path: 'internal/b' }] })],
          },
        },
      ],
    },
  ]);
  // The units and the join declare no outputs of their own, so one control-only
  // envelope answers every codex turn in this run.
  await setCodexScenario(harness, 'fanout-units', doneEnvelope({}));
  const project = await seedWorkflowProject(
    harness,
    'workflow-fanout-dynamic-project',
    [
      {
        name: 'fanout-dynamic',
        yaml: dynamicFanOutYaml,
        prompts: {
          'plan.md': 'Plan {{goal}} and return the sections.',
          'unit.md': 'Port {{section.path}} and return the envelope.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'fanout-dynamic');
  await waitForWorkflowState(harness, item.id, 'done');

  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((phase) => phase.phaseId)).toEqual(['plan', 'port']);
  // Ids are stamped from the template, one per element, in element order.
  expect(detail.units.map((unit) => unit.unitId)).toEqual([
    'port-section-0',
    'port-section-1',
    'merge',
  ]);
  expect(detail.units.map((unit) => unit.status)).toEqual(['done', 'done', 'done']);
  expect(detail.units.map((unit) => unit.unitIndex)).toEqual([0, 1, 2]);
  // Read-only units are not isolated: they run in the run's own workspace.
  expect(detail.units.every((unit) => !unit.worktreePath)).toBe(true);
  expect(detail.phases[1]?.threadId).toBe(unitById(detail, 'merge').threadId);
  expect(detail.phases[1]?.outputEnvelope).toMatchObject({ status: 'done' });
});

// A mixed-provider fan-out whose codex unit fails: the claude unit still
// finishes, the attempt parks on the failure, and the human repairs one unit.
const mixedFanOutYaml = `id: fanout-recovery
name: Fan-out recovery
inputs:
  goal:
    schema:
      type: string
phases:
  - id: port
    shape: fan-out
    outputs:
      report:
        schema:
          type: string
    fan_out:
      - id: alpha
        provider: claude
        model: claude-opus-4-7
        prompt: alpha.md
        access: read-only
        outputs:
          report:
            schema:
              type: string
      - id: beta
        provider: codex
        model: gpt-5.5
        prompt: beta.md
        access: read-only
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
    gate:
      routes:
        - to: done
cleanup: manual
`;

test('a failed unit parks the attempt and WorkflowRetryUnit repairs it in place', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'fanout-recovery-claude', [
    { steps: [{ emit: { lines: [doneResult({ report: 'ok' })] } }] },
  ]);
  await setCodexScenario(harness, 'fanout-recovery-stuck', stuckEnvelope('beta slice does not build'));
  const project = await seedWorkflowProject(
    harness,
    'workflow-fanout-recovery-project',
    [
      {
        name: 'fanout-recovery',
        yaml: mixedFanOutYaml,
        prompts: {
          'alpha.md': 'Port the alpha slice and return the envelope.',
          'beta.md': 'Port the beta slice and return the envelope.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'fanout-recovery');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'unit-failed');

  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(unitById(parked, 'alpha').status).toBe('done');
  expect(unitById(parked, 'beta').status).toBe('failed');
  // The join must not consolidate an attempt a human has not repaired yet.
  expect(unitById(parked, 'merge').status).toBe('pending');
  expect(parked.phases[0]?.threadId ?? '').toBe('');

  // A scenario only reaches mocks that register after it is set, so the retry's
  // fresh session is the one that gets the repaired behaviour.
  await setCodexScenario(harness, 'fanout-recovery-done', doneEnvelope({}));
  await harness.rpc('WorkflowRetryUnit', item.id, 'beta', 'binding fixed; run it again');
  await waitForWorkflowState(harness, item.id, 'done');

  const repaired = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(repaired.units.map((unit) => unit.status)).toEqual(['done', 'done', 'done']);
  // The unit was repaired in place: same attempt, same row, one more try.
  expect(unitById(repaired, 'beta').unitAttempt).toBe(2);
  expect(unitById(repaired, 'alpha').unitAttempt).toBe(1);
  expect(repaired.phases).toHaveLength(1);
  expect(repaired.phases[0]?.attempt).toBe(1);
  expect(repaired.phases[0]?.threadId).toBe(unitById(repaired, 'merge').threadId);
  expect(repaired.phases[0]?.outputEnvelope).toMatchObject({
    status: 'done',
    outputs: { report: 'ok' },
  });
});

// The many-at-once shape: one cause — a provider usage limit — takes down every
// unit of the attempt, and the human repairs all of them with one call after
// waiting the limit out. The mixed-provider fan-out is what makes it a real
// test: both providers had to fail and both had to be repaired by the same
// command.
test('WorkflowRetryFailedUnits repairs every failed unit of a parked attempt at once', async ({
  harness,
}) => {
  await setClaudeScenario(harness, 'fanout-retry-all-claude-stuck', [
    { steps: [{ emit: { lines: [stuckResult('alpha slice hit the usage limit')] } }] },
  ]);
  await setCodexScenario(harness, 'fanout-retry-all-codex-stuck', stuckEnvelope('beta slice hit the usage limit'));
  const project = await seedWorkflowProject(
    harness,
    'workflow-fanout-retry-all-project',
    [
      {
        name: 'fanout-recovery',
        yaml: mixedFanOutYaml,
        prompts: {
          'alpha.md': 'Port the alpha slice and return the envelope.',
          'beta.md': 'Port the beta slice and return the envelope.',
          'merge.md': 'Consolidate {{units}} and return the envelope.',
        },
      },
    ],
    [],
    'base_branch: main\n',
  );

  const item = await start(harness, project.projectId, 'fanout-recovery');
  await waitForWorkflowState(harness, item.id, 'needs-human', 'unit-failed');

  const parked = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(unitById(parked, 'alpha').status).toBe('failed');
  expect(unitById(parked, 'beta').status).toBe('failed');
  expect(unitById(parked, 'merge').status).toBe('pending');

  // The limit reset: every session started after this point gets the repaired
  // behaviour, on both providers.
  await setClaudeScenario(harness, 'fanout-retry-all-claude-done', [
    { steps: [{ emit: { lines: [doneResult({ report: 'ok' })] } }] },
  ]);
  await setCodexScenario(harness, 'fanout-retry-all-codex-done', doneEnvelope({}));
  await harness.rpc('WorkflowRetryFailedUnits', item.id, 'the usage limit reset');
  await waitForWorkflowState(harness, item.id, 'done');

  const repaired = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(repaired.units.map((unit) => unit.status)).toEqual(['done', 'done', 'done']);
  // Every failed unit was repaired in place: same rows, one more try each, and
  // the attempt they belong to was reopened rather than replaced.
  expect(unitById(repaired, 'alpha').unitAttempt).toBe(2);
  expect(unitById(repaired, 'beta').unitAttempt).toBe(2);
  expect(repaired.phases).toHaveLength(1);
  expect(repaired.phases[0]?.attempt).toBe(1);
  expect(repaired.phases[0]?.outputEnvelope).toMatchObject({
    status: 'done',
    outputs: { report: 'ok' },
  });
});
