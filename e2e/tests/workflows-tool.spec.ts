import { test, expect } from './fixtures.js';
import {
  doneResult,
  seedWorkflowProject,
  setClaudeScenario,
  start,
  waitForWorkflowState,
  type WorkflowDetail,
} from './workflows-helpers.js';

// `driver: tool` phases run real subprocesses in the run's workspace. These
// fixtures bind the project profile to real binaries and to a script committed
// into the seeded repository — never to a shell string.

const agentPhase = (id: string, promptFile: string, inputs: string, next = 'done') =>
  `  - id: ${id}
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: ${promptFile}
    access: read-only
    inputs:
${inputs}
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: ${next}
`;

test('a green tool check routes its gate into an agent phase', async ({ harness }) => {
  await setClaudeScenario(harness, 'tool-green', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const yaml = `id: tool-green
name: Tool green
inputs:
  goal:
    schema:
      type: string
phases:
  - id: verify
    driver: tool
    check: repo-head
    gate:
      routes:
        - when:
            eq:
              ref: verify.passed
              value: true
          to: report
        - park: red-check
${agentPhase(
  'report',
  'tool-green.md',
  `      verify.exit-code:
        schema:
          type: number`,
)}cleanup: manual
`;
  const project = await seedWorkflowProject(
    harness,
    'workflow-tool-green-project',
    [{ name: 'tool-green', yaml }],
    [],
    'checks:\n  repo-head: ["git", "rev-parse", "--verify", "HEAD"]\n',
  );
  const item = await start(harness, project.projectId, 'tool-green');

  await waitForWorkflowState(harness, item.id, 'done');
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((phase) => phase.phaseId)).toEqual(['verify', 'report']);
  expect(detail.checkPhaseIds).toEqual(['verify']);
  // A tool phase holds no provider session, so it has no AO thread.
  expect(detail.phases[0].threadId).toBeFalsy();
  expect(detail.phases[1].threadId).toBeTruthy();
  expect(detail.phases[0].outputEnvelope).toMatchObject({
    status: 'done',
    outputs: { passed: true, 'exit-code': 0 },
  });
});

test('a red tool check loops back through its bound retry and then parks', async ({ harness }) => {
  await setClaudeScenario(harness, 'tool-red', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const yaml = `id: tool-red
name: Tool red
inputs:
  goal:
    schema:
      type: string
phases:
${agentPhase(
  'prepare',
  'tool-red.md',
  `      goal:
        schema:
          type: string`,
  'verify',
)}  - id: verify
    driver: tool
    check: missing-ref
    inputs:
      prepare.complete:
        schema:
          type: boolean
    gate:
      routes:
        - when:
            eq:
              ref: verify.passed
              value: true
          to: done
        - loop: prepare
          max: 1
        - park: red-check
cleanup: manual
`;
  const project = await seedWorkflowProject(
    harness,
    'workflow-tool-red-project',
    [{ name: 'tool-red', yaml }],
    [],
    'checks:\n  missing-ref: ["git", "rev-parse", "--verify", "refs/heads/definitely-missing"]\n',
  );
  const item = await start(harness, project.projectId, 'tool-red');

  // A non-zero exit is `passed: false`, not a phase failure: the gate loops
  // once, and parks when the bound is spent.
  await waitForWorkflowState(harness, item.id, 'needs-human', 'gate');
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((phase) => `${phase.phaseId}.${phase.attempt}`)).toEqual([
    'prepare.1',
    'verify.1',
    'prepare.2',
    'verify.2',
  ]);
  for (const phase of detail.phases.filter((entry) => entry.phaseId === 'verify')) {
    expect(phase.outputEnvelope).toMatchObject({ outputs: { passed: false } });
  }
});

test('a tool command writes its own envelope and the gate routes on it', async ({ harness }) => {
  await setClaudeScenario(harness, 'tool-envelope', [
    { steps: [{ emit: { lines: [doneResult({ complete: true })] } }] },
  ]);
  const yaml = `id: tool-envelope
name: Tool envelope
inputs:
  goal:
    schema:
      type: string
phases:
  - id: scan
    driver: tool
    check: scan
    outputs:
      findings:
        schema:
          type: number
    gate:
      routes:
        - when:
            gt:
              ref: scan.findings
              value: 0
          to: triage
        - to: done
${agentPhase(
  'triage',
  'tool-envelope.md',
  `      scan.findings:
        schema:
          type: number`,
)}cleanup: manual
`;
  const project = await seedWorkflowProject(
    harness,
    'workflow-tool-envelope-project',
    [{ name: 'tool-envelope', yaml, prompts: { 'tool-envelope.md': 'Triage {{scan.findings}} findings.' } }],
    [],
    'checks:\n  scan: ["sh", "scripts/scan.sh"]\n',
    {
      'scripts/scan.sh': [
        '#!/bin/sh',
        'echo "scanning"',
        `printf '%s' '{"status":"done","outputs":{"findings":3},"question":null,"reason":null}' > "$AO_ENVELOPE"`,
        '',
      ].join('\n'),
    },
  );
  const item = await start(harness, project.projectId, 'tool-envelope');

  await waitForWorkflowState(harness, item.id, 'done');
  const detail = await harness.rpc<WorkflowDetail>('WorkflowGetItem', item.id);
  expect(detail.phases.map((phase) => phase.phaseId)).toEqual(['scan', 'triage']);
  // The command owns `findings`; the runner owns the check result, which the
  // command cannot know while it is still writing the file.
  expect(detail.phases[0].outputEnvelope).toMatchObject({
    status: 'done',
    outputs: { findings: 3, passed: true, 'exit-code': 0 },
  });
});
